# x402 Gateway: payment enforcement in the data plane

An Envoy `ext_proc` filter and a Kubernetes `PaymentPolicy` CRD that put **any HTTP
route** behind [x402](https://github.com/x402-foundation/x402) per-request payments, on
[Radius](https://radiustech.xyz), with **zero changes to the backend**.

```yaml
apiVersion: x402.dev/v1alpha1
kind: PaymentPolicy
metadata: {name: paid-search}
spec:
  targetRef: {group: gateway.networking.k8s.io, kind: HTTPRoute, name: search}
  price: "1000"          # 0.001 SBC
  payTo: "0x…"
```

`kubectl apply` it and the route answers `402 Payment Required`. A buyer signs, retries,
and gets a `200`. `kubectl delete` it and the route is free again. The backend pod never
restarts.

> **Status: under construction.** The design is done and the constraints are verified;
> the filter and CRD are being built in the open — see [ROADMAP.md](ROADMAP.md).

## Why the data plane forces the design

An L7 proxy gives an external filter **200ms** by default, and a fail-open filter that
blows it **releases the response unpaid**. An on-chain settlement takes seconds. So a
payment enforcer in the data plane cannot settle inline; it has to **decide from local
state in milliseconds and settle asynchronously**:

```
request_headers   no payment  → 402 + PAYMENT-REQUIRED          (backend never sees it)
                  payment     → verify, RESERVE in Redis         (~ms)
response          2xx         → release now, enqueue settlement
                  5xx         → drop the reservation             (nobody is charged)
x402-settler      off-path    → settle through the facilitator   (~s)
```

The shared Redis reservation is what closes the two findings the application-layer
version of this project left open: a payment replayed against another replica, and
concurrent requests overdrawing one balance.

## Architecture

```
Buyer ──► GCP LB ──► Envoy Gateway ──ext_proc──► x402-filter ──verify──► facilitator
                         │                          │                        ▲
                         │                        Redis ──stream──► x402-settler ┘ settle
                         ▼
                  unmodified backends            PaymentPolicy ──► x402-controller
```

Runs on GKE. The cluster holds **no private keys**: buyers sign, and the facilitator
submits the settlement transaction.

## Why Radius

Settling after the response means the seller holds unsettled revenue until settlement
lands, and the rail decides how long that is. On Radius, block numbers are millisecond
timestamps, there are no reorgs, and a settlement costs a fixed ~$0.0001 in stablecoin.
The window shrinks to roughly the length of the request itself, one receipt is final,
and a $0.001 call is still economic. The prediction this project exists to test: on
Radius, even the spec's synchronous *settle, then respond* ordering fits inside a proxy's
timeout, which no slower rail allows. The full argument and its evidence are in
[ROADMAP.md](ROADMAP.md#why-radius).

## Repository

| Path | What |
|---|---|
| `cmd/`, `internal/` | Go: the filter (and, as phases land, the settler) |
| `ROADMAP.md` | phases, gates, and the measurement campaign |
| `legacy/llm-gateway/` | where this started: a FastAPI pay-per-prompt gateway, two settlement paths, an escrow contract, and the [audit findings](legacy/llm-gateway/REPORT.md) that motivated the move to the data plane |

## Development

```bash
make build    # bin/x402-filter
make test     # go test -race ./...
make lint     # golangci-lint
make images   # one distroless image per binary
make legacy-test
```

## Related work

[Anteroom](https://anteroom.dev) gates a single app behind proof-of-work, with an
experimental x402 pay door that settles before issuing a time-based pass. This project is
its complement: per-request payment inside a shared gateway, settled after the response.
