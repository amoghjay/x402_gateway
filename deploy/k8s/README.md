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

# 4. Our platform components: Redis, the filter with its RBAC, and the controller
kubectl apply -f x402-system/

# 5. The demo backend, its routes, and the PaymentPolicy that prices one of them
kubectl apply -f demo/
```

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
| `demo/` | `demo` namespace (labelled for routes); httpbin on the spot pool; `HTTPRoute httpbin` sending `/` to it and the `PaymentPolicy` that prices it; `HTTPRoute httpbin-unpriced` for `/anything/free`, which has no policy and so never reaches the filter |
| `x402-system/` | `x402-system` namespace; Redis as a one-replica StatefulSet on the system pool, AOF (`everysec`) on a 10Gi PVC, behind a headless Service; the filter's ServiceAccount and a Role that can list/watch only the `x402-prices` ConfigMap; the `x402-filter` Deployment and Service; the controller's ServiceAccount, ClusterRole (policies, routes, extension policies) and namespaced Role (the one ConfigMap, the one grant, the Lease), and its Deployment |
