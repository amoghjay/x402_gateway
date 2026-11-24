# deploy/k8s

Raw manifests for what runs in the cluster. Third-party components are installed with
Helm at pinned versions; our own resources are plain YAML, applied with `kubectl`.
Run everything from inside the repo so direnv selects the personal `KUBECONFIG`.

## Order

```bash
# 1. Envoy Gateway v1.9.2: its CRDs, the Gateway API CRDs, and the controller
helm install eg oci://docker.io/envoyproxy/gateway-helm --version v1.9.2 \
  -n envoy-gateway-system --create-namespace
kubectl wait --timeout=5m -n envoy-gateway-system deployment/envoy-gateway --for=condition=Available

# 2. Our GatewayClass, its EnvoyProxy settings, and the Gateway on the reserved IP
kubectl apply -f gateway/

# 3. The PaymentPolicy CRD (generated: `make generate`)
kubectl apply -f crd/

# 4. Our platform components: Redis, the filter with its RBAC, the controller, the settler
kubectl apply -f x402-system/

# 5. The demo backend, its routes, and the PaymentPolicy that prices one of them
kubectl apply -f demo/
```

**Image tags.** Our images are tagged with the 12-character commit they were built from,
pushed by CI on every merge to main that changes code. The same CI run then opens a
`deploy: images <tag>` PR that moves the three tags in `x402-system/` and merges itself
when its checks pass. Rolling it out is still `kubectl apply -f x402-system/`.

Applying a `PaymentPolicy` is the whole act of pricing a route. The controller writes
the `EnvoyExtensionPolicy` that sends the route through the filter, the entry in the
`x402-prices` ConfigMap the filter watches, and the `ReferenceGrant` in `x402-system`
that lets that namespace's policy name the filter Service. Deleting the policy undoes all
three (the extension policy through its owner reference). `kubectl get pp -A` shows each
policy's route, price and `Accepted` status; `-o yaml` has the reason when it is not.

| Path | What |
|---|---|
| `gateway/` | `gateway` namespace; `EnvoyProxy` (LoadBalancer on the reserved IP, proxies on the system pool); `GatewayClass x402`; `Gateway x402` with one HTTP listener that accepts routes only from labelled namespaces |
| `crd/` | `PaymentPolicy` (`x402.dev/v1alpha1`), generated from `api/v1alpha1` |
| `demo/` | `demo` namespace (labelled for routes); httpbin on the spot pool; `HTTPRoute httpbin` sending `/` to it and the `PaymentPolicy` that prices it; `HTTPRoute httpbin-unpriced` for `/anything/free`, which has no policy and so never reaches the filter; `httpbin-sync` (`/anything/sync`, Radius testnet) and `httpbin-base` (`/anything/base`, Base Sepolia), both `captureMode: Sync`, for the settle-before-release drills |
| `demo-platform/` | A route governed by a Radius Platform project: `HTTPRoute httpbin-platform` (`/anything/platform`) and a `PaymentPolicy` with `spec.platform`. Not applied by step 5; it needs your project and a labelled Secret, see [its README](demo-platform/README.md) |
| `x402-system/` | `x402-system` namespace; Redis as a one-replica StatefulSet on the system pool, AOF (`everysec`) on a 10Gi PVC, behind a headless Service; the filter's ServiceAccount and a Role that can list/watch only the `x402-prices` ConfigMap; the `x402-filter` Deployment (2 replicas, payments on via `-redis`), Service and PodDisruptionBudget; the controller's ServiceAccount, ClusterRole (policies, routes, extension policies) and namespaced Role (the one ConfigMap, the one grant, the Lease), and its Deployment; the `x402-settler` Deployment (no Kubernetes API access) |

A paid request, end to end: Envoy sends the request to the filter, which verifies the
signature, reads the payer's balance (cached in Redis), reserves (claims the nonce, adds
to the payer's in-flight sum) and lets it through with `x-x402-payer` set for the
backend. On a `captureMode: Sync` route (the default) the filter then calls `/settle`
itself while Envoy holds the response, and the client gets it, with the transaction in
`PAYMENT-RESPONSE`, only if that succeeded. On an Async route (opt-in) the response goes
out at once, the settlement job goes on the Redis stream, and the settler calls the
facilitator's `/settle` and logs the explorer link. A keyed RPC URL, when there is one, goes in
a Secret as `RADIUS_RPC_URL` on both the filter and the settler.

**Payments charged without a confirmed delivery.** Before a Sync `/settle` the filter
writes the payment to Redis, and clears it once Envoy has the response or the settle is
refused. What is left (a 503 `settlement_unknown`, a filter killed mid-settle, a stream
lost after the transfer) is checked on chain by the settler after 30 s: a used nonce
goes on the `outcomes` stream as `charged_unconfirmed`, with the payer, route and
amount, for the seller to refund or redeliver; an authorization that expired unused is
dropped (nobody was charged). The settler logs each one at error level. To list them:

```
kubectl -n x402-system exec redis-0 -- redis-cli XRANGE outcomes - +
```

**Metrics.** Each component serves Prometheus metrics. Scraping and alert rules are not
in these manifests yet.

| Component | Endpoint | What it adds |
|---|---|---|
| filter | `:9090/metrics` (`-metrics-addr`, port `metrics`) | every decision, how long each phase held the request, dependency calls, balance-cache hits, the price table in force |
| settler | `:8081/metrics` (port `health`) | settlement and held-payment outcomes, dependency calls |
| controller | `:8080/metrics` (`-metrics-addr`, port `metrics`) | controller-runtime's reconcile metrics, Radius Platform fetches |

A paid request on a Sync route is two observations of `x402_filter_phase_duration_seconds`
on its route: `phase="request", decision="payment_accepted"` (the checks before the backend)
and `phase="response", decision="settled"` (the held settle). To read them without a
Prometheus:

```
kubectl -n x402-system port-forward deploy/x402-filter 9090 &
curl -s localhost:9090/metrics | grep -E 'x402_filter_(decisions_total|phase_duration_seconds_(sum|count))'
```

Labels never carry a payer, nonce, transaction or path: `route` is the price-table key,
and `reason` is the 402 reason or a policy's built-in code (`other` and `custom` stand in
for anything else).
