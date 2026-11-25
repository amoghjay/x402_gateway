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

## Try it on your laptop

Docker with Compose, and Node 22 for `radius-cli`. No cluster, no account: Envoy, the
filter, Redis, the settler and a demo API, from [`deploy/compose/`](deploy/compose/README.md).

```bash
cd deploy/compose && cp .env.example .env     # set PAY_TO to a wallet you control
docker compose up --build
curl -i localhost:8080/anything/paid          # 402: 0.001 SBC on Radius testnet

npx radius-cli@0.8.0 --network testnet wallet new
npx radius-cli@0.8.0 --network testnet wallet faucet
npx radius-cli@0.8.0 --network testnet wallet pay get 'http://localhost:8080/anything/paid' --threshold 0.001   # 200

curl -s localhost:9090/metrics | grep 'phase_duration_seconds_sum.*route="paid"'   # what the gate added, by phase
```

Pointing it at your own API is two lines in `.env` and a route per price; the
[compose README](deploy/compose/README.md#use-it-with-your-api) has the steps, and how to
enforce a Radius Platform project's policy instead of fixed prices.

## Try the live gateway

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
pay it. Tested so far: our own buyer and `radius-cli` 0.8.0; the reference x402 clients
are next.

## Why the data plane forces the design

Envoy gives an external filter a fixed budget per message: 200 ms by default. A filter
that runs out gets the request failed (504), or, if it fails open, **releases the
response unpaid**. A facilitator takes 0.3–0.4 s to verify and about 0.5–1 s to settle
(measured). So the filter **verifies locally in milliseconds on the request**, asks the
facilitator's `/verify` as the gate standard requires (`-facilitator-verify`, on by
default), and
settles **on the response, while Envoy holds it** (`captureMode: Sync`, the default)
([design note](docs/design/request-path-budget.md)):

```
request_headers   no payment  → 402 + PAYMENT-REQUIRED          (backend never sees it)
                  payment     → verify the signature locally (~0.1 ms),
                                check the cached balance, RESERVE in Redis,
                                then the facilitator's /verify (~0.3–0.4 s)
response          2xx or 3xx  → hold it: check on chain the payment is unspent,
                  (not 304)     settle through the facilitator, then
                                  settled → release, with the transaction
                                  refused → 402 in its place     (nobody is charged)
                                  unclear → ask the chain; still unclear → 503
                  otherwise   → drop the reservation             (nobody is charged)
```

Nothing is served unless its transfer is on chain, so a cancelled authorization, a
moved balance or a replayed payment can cost the seller a backend call, never a
response. The check on chain before settling is there because a facilitator may answer
a repeated settle with success. The shared Redis reservation still stops what an
app-level gateway can't: a payment replayed against another replica, and concurrent
requests overdrawing one balance. A broken `PaymentPolicy` makes its route answer 503,
never serve free.

Measured on GKE on 2026-11-19, on Radius testnet and Base Sepolia: the held response
took about 0.55 s at the median (p99 under 1 s, n=100 at four concurrent buyers);
refused settlements, replays, a deleted Redis record, a facilitator slow by 3 s or 8 s,
a hung or unreachable facilitator and a rolling restart mid-settle all ended with
nobody served unpaid and nobody charged without a response. The one case where a buyer
can still pay and get a 503 is a filter pod killed outright mid-settle; the filter
records every payment before settling it, and the settler lists any that were charged
without a confirmed delivery, for a refund ([results](docs/design/request-path-budget.md),
[how to list them](deploy/k8s/README.md)).

`captureMode: Async` is the opt-in fast path: the response goes out at once and the
`x402-settler` settles about a second later. The payer can cancel or move the funds in
that gap and the seller carries the loss, capped per wallet but not in total.

## Architecture

```
app team ─kubectl apply─► PaymentPolicy ─► x402-controller ─► EnvoyExtensionPolicy + price table

buyer ─► GCP LB ─► Envoy Gateway ─ext_proc─► x402-filter ─/verify, /settle (Sync)─► facilitator
                        │                        │                                          ▲
                        ▼                        ▼                                          │ /settle
                 unmodified backend          Redis (nonce, in-flight, ─settle stream─► x402-settler
                                             balance cache)               (Async)
```

Three Go binaries: the filter, the controller and the settler. On a `captureMode: Sync`
route, the default, the filter calls the facilitator itself: `/verify` on the request,
`/settle` while Envoy holds the response. The settler settles `Async` routes from the
Redis stream, and lists payments charged without a confirmed delivery. The request path
never calls the Kubernetes API. The cluster holds **no private keys**:
buyers sign, and the facilitator submits the transfer. The full map, the three flows and
the reasoning are in [docs/architecture.md](docs/architecture.md).

## Conformance

Routes governed by a Radius Platform project
([`deploy/k8s/demo-platform/`](deploy/k8s/demo-platform/README.md)) follow the
Platform's gate standard, with the deviations listed below.

- **Format versions.** The gate accepts compiled policies with `schema`
  `radius.policy/0.1` and `radius.policy/0.2` (`policy.Schemas` in
  [`internal/policy/policy.go`](internal/policy/policy.go)). A document with any other
  `schema` is refused whole, and the last good version stays in force.
- **Startup log lines.** The filter logs
  `{"msg":"Radius Platform compiled policies accepted",…,"schemas":["radius.policy/0.1","radius.policy/0.2"]}`.
  The controller's leader logs `polling Radius Platform projects` with the same `schemas`.
- **Vectors.** The standard's conformance vectors are in
  [`internal/policy/testdata/vectors/`](internal/policy/testdata/vectors/SOURCE.md),
  copied unchanged at a pinned commit: 73 cases in nine files, 16 of them in `agent.json`
  (`0.2`) and 57 in the eight `0.1` files. `go test ./internal/policy/` runs them; 73 of 73
  pass (2026-11-23).

Deliberate deviations, each explained in the code that makes it:

- **Paths are matched after Envoy normalizes them** (dot segments removed, slashes
  merged). The standard (§4.1) says to compare the path as the client sent it. The
  normalized path is the one Envoy routes on and the backend receives, so a rule cannot
  be dodged with `/free/../paid`
  ([`internal/extproc/platform.go`](internal/extproc/platform.go)).
- **The gate keeps a record of payments.** §6.2 says a gate keeps none and reads nothing
  of the payload beyond `x402Version` and `accepted`. This gate claims every payment in
  Redis against replay, and on the EIP-3009 path it also checks the signature and the
  payer's balance itself, keeps a held-payment record, and reads the authorization's state
  on chain before settling. The reason: the Radius testnet facilitator answered a repeated
  `/settle` with success (re-tested 2026-11-20), so the facilitator alone does not stop a
  replay ([`charge.go`](internal/extproc/charge.go),
  [`facilitated.go`](internal/extproc/facilitated.go)).
- **A redirect is charged.** §6.2 settles on a 2xx. This gate also settles on a 3xx
  other than 304, because a redirect's `Location` can be the content itself, such as a
  presigned download URL (`chargeable` in
  [`internal/extproc/server.go`](internal/extproc/server.go)).
- **Rules that match an agent `category` never match.** The standard takes the category
  only from a source the client cannot forge, such as Cloudflare's verified bot category;
  this gate has none. It loads such a rule instead of refusing the document (§9.2), the
  rule never matches, and the controller says so on the `PaymentPolicy`'s `Synced`
  condition ([`internal/controller/poller.go`](internal/controller/poller.go)). Rules on a
  Web Bot Auth `signature_agent` are verified and do match.

Not a deviation, but easy to trip on: a rule that lists methods never matches `HEAD` or
`OPTIONS`, as in the reference evaluator (the Platform's SDK differs). A `HEAD` to a
`GET`-only endpoint is decided by a later rule or the policy's default
([`internal/policy/policy.go`](internal/policy/policy.go)).

[`tools/drills/platform.sh`](tools/drills/platform.sh) reruns the live checks of a
Platform route (allow, deny, charge, free quota, rate limit, a payment per option and a
replay) against a gateway.

## Why Radius

The gate holds a paid response while its payment settles, so the rail decides how long
a paying caller waits. Through a facilitator the held response took about 0.55 s at the
median on both Radius testnet and Base Sepolia (GKE, 2026-11-19, n=100), and most of
that is the facilitator round trip, not the chain. On Radius a self-submitted transfer
reached its final receipt in 91 ms at the median from GKE (500 settles, 2026-11-19),
there are no reorgs, and a transfer costs a fixed $0.0001, so a $0.001 call is still
economic. Whether Radius is measurably faster for a held response stays open until the
fast lane runs end to end. The argument and its evidence are in
[ROADMAP.md](ROADMAP.md#why-radius).

## Run it yourself

Without Kubernetes: [`deploy/compose/`](deploy/compose/README.md), as above. On GKE:
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
| `deploy/` | Terraform for GKE, Kubernetes manifests, and `compose/`: the gate with Docker Compose and a plain Envoy |
| `docs/` | [architecture](docs/architecture.md), the [request-path budget](docs/design/request-path-budget.md) and its measurements, design notes |
| `tools/` | the demo walkthrough, [drill scripts](tools/drills/README.md), latency probes, GKE pause/resume |
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
Envoy, the response held until its payment has settled, replay and overdraft state
shared by every replica, and routes priced by a `PaymentPolicy` or by a Radius Platform
project's policy. The comparison is in [ROADMAP.md](ROADMAP.md#prior-art-stated-up-front).

## License

[Apache License 2.0](LICENSE). Versions published before this change were under the MIT
License and remain available under it. The policy evaluator and its conformance vectors
are ported from MIT-licensed code; the attribution is in [NOTICE](NOTICE).
