# x402 Gateway: payment enforcement in the data plane

An Envoy `ext_proc` filter and a Kubernetes `PaymentPolicy` CRD that put **any HTTP
route** behind [x402](https://github.com/x402-foundation/x402) per-request payments, on
[Radius](https://radiustech.xyz), with **zero changes to the backend**.

```yaml
apiVersion: x402.dev/v1alpha1
kind: PaymentPolicy
metadata: {name: httpbin, namespace: demo}
spec:
  targetRef: {name: httpbin}   # an HTTPRoute in this namespace
  network: testnet
  price: "1000"                # 0.001 SBC
  payTo: "0x2a52…BfBe4"
```

`kubectl apply` it and the route answers `402 Payment Required`. A buyer signs, retries,
and gets a `200`. `kubectl delete` it and the route is free again. The backend pod never
restarts, and it learns who paid from one request header.

**Status: live on GKE against Radius testnet** (checked 2026-11-04). Testnet only:
mainnet waits on a soak, see [ROADMAP.md](ROADMAP.md).

| Check, on the live cluster | Result |
|---|---|
| `kubectl apply` a PaymentPolicy → 402 | 0.34–0.39 s |
| pay with an EIP-3009 signature → 200 | the backend sees `X-X402-Payer`; settled on chain ~1 s after the response |
| replay the same signature | 402 `payment_already_used` |
| 10 concurrent payments against a balance worth 5 | exactly 5 served and settled |
| `kubectl delete` the policy → free | ≤ 0.21 s |
| settler killed mid-queue | the replacement re-drives the queue; one orphaned job that had settled was mis-filed as unpaid, [fixed](https://github.com/amoghjay/x402_gateway/pull/50) |

## Try it

The demo route prices [httpbin](https://httpbin.org) at 0.001 testnet SBC. The cluster
runs on a personal GCP project and may be paused, so if the hostname doesn't answer,
that's why.

```bash
curl -i http://x402-22ade072.nip.io/get              # 402, PAYMENT-REQUIRED header + JSON body
curl -i http://x402-22ade072.nip.io/anything/free    # 200, no policy on this route

make build                                           # bin/x402-buyer (Go 1.27)
bin/x402-buyer keygen                                # a testnet key in ~/.x402, mode 0600
bin/x402-buyer drip                                  # ~0.5 test SBC from the Radius faucet
bin/x402-buyer pay http://x402-22ade072.nip.io/get   # 200, httpbin echoes X-X402-Payer

tools/demo/walkthrough.sh                            # the five-minute walkthrough
```

The 402 is plain x402 V2, so any V2 client with EIP-3009 support should be able to
pay it. So far only our own buyer has been tested against it; `radius-cli wallet pay` is next.

## Why the data plane forces the design

Envoy gives an external filter a fixed budget per message: 200 ms by default, 300 ms
here. A filter that runs out gets the request failed (504), or, if it fails open,
**releases the response unpaid**. A facilitator round trip takes 0.3–0.4 s to verify and
0.6–1.1 s to settle (measured). So the enforcer cannot call out inline. It **verifies
locally in milliseconds and settles after the response**
([design note](docs/design/request-path-budget.md)):

```
request_headers   no payment  → 402 + PAYMENT-REQUIRED          (backend never sees it)
                  payment     → verify the signature locally (~0.1 ms),
                                check the cached balance, RESERVE in Redis
response          2xx         → release now, enqueue settlement
                  not 2xx     → drop the reservation             (nobody is charged)
x402-settler      off-path    → settle through the facilitator   (~1 s)
```

The shared Redis reservation closes two flaws that an app-level gateway can't: a
payment replayed against another replica, and concurrent requests overdrawing one
balance. The cost is a bounded window of unsettled revenue, which the seller carries
and the rail decides the width of.

## Architecture

```
app team ─kubectl apply─► PaymentPolicy ─► x402-controller ─► EnvoyExtensionPolicy + price table

buyer ─► GCP LB ─► Envoy Gateway ─ext_proc─► x402-filter ─► Redis (nonce, in-flight, balance cache)
                        │                                      │ settle stream
                        ▼                                      ▼
                 unmodified backend                       x402-settler ─/settle─► Radius facilitator
```

Three Go binaries: the filter, the controller and the settler. The request path never
calls the facilitator or the Kubernetes API. The cluster holds **no private keys**:
buyers sign, and the facilitator submits the transfer. The full map, the three flows and
the reasoning are in [docs/architecture.md](docs/architecture.md).

## Why Radius

Settling after the response means the seller holds unsettled revenue until settlement
lands, and the rail decides how long that is. On Radius a transfer is final in about
110 ms (measured), there are no reorgs, and a settlement costs a fixed ~$0.0001 in
stablecoin. The window shrinks to roughly the length of the request, one receipt is
final, and a $0.001 call is still economic. Whether the spec's synchronous *settle,
then respond* ordering can also fit is an open question this project measures. The
argument and its evidence are in [ROADMAP.md](ROADMAP.md#why-radius).

## Run it yourself

[`deploy/terraform/`](deploy/terraform/README.md) builds the GKE project (zonal cluster,
spot workload pool, Artifact Registry, keyless CI push).
[`deploy/k8s/`](deploy/k8s/README.md) has the apply order: Envoy Gateway v1.9.2 by Helm,
then our CRD, Redis, the filter, the controller, the settler and the demo.

## Repository

| Path | What |
|---|---|
| `api/v1alpha1/` | the `PaymentPolicy` type; the CRD is generated into `deploy/k8s/crd/` |
| `cmd/` | `x402-filter`, `x402-controller`, `x402-settler`, and `x402-buyer` (a test client) |
| `internal/` | the ext_proc server, the local verifier, the Redis reserve and settle stream, the controller ([map](docs/architecture.md#repository-map)) |
| `deploy/` | Terraform for GKE, Kubernetes manifests |
| `docs/` | [architecture](docs/architecture.md), the [request-path budget](docs/design/request-path-budget.md) and its measurements, design notes |
| `tools/` | the demo walkthrough, latency probes, GKE pause/resume |
| `legacy/llm-gateway/` | where this started: an app-level pay-per-prompt gateway and the [audit findings](legacy/llm-gateway/REPORT.md) that moved enforcement into the data plane |

## Development

```bash
make build         # bin/: the three binaries and the buyer
make test          # go test -race ./...
make test-envtest  # the controller against a real kube-apiserver
make lint          # golangci-lint
make generate      # the CRD from api/v1alpha1
```

## Related work

AWS documents x402 on CloudFront + Lambda@Edge, Cloudflare has a Monetization Gateway
in waitlist, and [traefik-x402](https://github.com/lukaszraczylo/traefik-x402) is the
closest open-source neighbour (it settles before releasing the response).
[Anteroom](https://anteroom.dev) gates a single app behind proof-of-work, with an
experimental x402 pay door. This project's point on that map: inside the cluster's own
Envoy, no facilitator call on the request path, and app teams price their own routes
with a policy. The comparison is in [ROADMAP.md](ROADMAP.md#prior-art-stated-up-front).
