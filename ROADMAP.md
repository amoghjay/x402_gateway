# Roadmap: payment enforcement as data-plane infrastructure

> **The claim this roadmap exists to measure.** An L7 proxy gives an external filter a
> fixed budget (200 ms per message by default in Envoy). When the filter runs out, the
> request fails with a 504, or goes out unpaid if the filter fails open. Neither is
> acceptable for a payment, and a facilitator round trip does not fit the budget. Payment
> enforcement in a data plane must therefore check locally and capture after the response
> (reserve-then-capture). The deliverable is what that costs to build, run, and get wrong.
> The finding generalises past x402 to any settlement path slower than the proxy's budget.

**Status.** Phase 0 and Phase 1 are done: the GKE cluster is in Terraform, Envoy Gateway
and a no-op `ext_proc` filter serve the static IP, and the timeout drill and in-cluster
baseline are measured and written up (`docs/design/request-path-budget.md`). Next is
Phase 2, x402 V2. The application-layer
gateway it grew out of — both settlement paths, five findings, 29 pytest + 9 forge
tests — lives on, green, under [`legacy/llm-gateway/`](legacy/llm-gateway/).

## What changed, and why

The project started as an LLM pay-per-prompt gateway with its own escrow contract. Its
two open findings ([REPORT §10.4 and §10.5](legacy/llm-gateway/REPORT.md)) are not
application bugs: replay across replicas and concurrent overdraft are properties of
*where* enforcement runs. So enforcement moves into the data plane, and the product is
the enforcer rather than any one backend.

Two scope decisions follow:

- **The escrow contract is shelved.** The `exact` scheme settles through the Radius
  facilitator, which submits the transaction itself — so the cluster holds **no private
  keys and no hot wallet**. Contract work returns only if a policy needs on-chain
  enforcement that no facilitator offers.
- **The LLM is no longer the product.** Demo backends are deliberately unmodified
  (`httpbin`) or deliberately ordinary (a small paid-data API).

## Prior art, stated up front

§10.4 and §10.5 correspond to flaw classes independently documented in
[arXiv 2605.30998](https://arxiv.org/abs/2605.30998) (*Free-Riding in the AI Economy*,
May 2026) — *allowance overdraft* and *duplicate-settlement race*. The contribution
here is not discovery: it is implementing the mitigation **inside a production data
plane** and measuring what it costs to operate.

Enforcing x402 in infrastructure, with the application untouched, is not new either:

- **AWS** documents the same pattern on CloudFront and Lambda@Edge
  ([guide](https://builder.aws.com/content/38fLQk6zKRfLnaUNzcLPsUexUlZ/monetize-any-http-application-with-x402-and-cloudfront-lambdaedge)).
  Verification runs on the origin request; settlement runs on the origin response, only for a
  status below 400. "Without modifying a single line of backend code."
- **Cloudflare** has a Monetization Gateway in waitlist.
- **[traefik-x402](https://github.com/lukaszraczylo/traefik-x402)** (October 2026, MIT) is an
  x402 v2 Traefik middleware. It calls the facilitator's `/verify` before the upstream and
  `/settle` after a successful upstream status, before the first byte reaches the client. Its
  replay guard is in-memory per Traefik instance. It is the closest prior work to this project.

What this project adds is the point on the latency/risk curve and where it runs:

- **In the cluster's own Envoy data plane,** not a CDN.
- **No facilitator call on the request path.** It verifies locally, reserves in Redis shared by
  every replica, and settles after the response. That moves roughly a second of facilitator
  latency (measured on Radius) off each paid request, and in exchange accepts a bounded window of
  unsettled revenue. The settle-before-release model the others use is kept as
  `captureMode: Sync`, so both can be measured side by side.
- **Self-service through a `PaymentPolicy` with status conditions,** so app teams price their own
  routes.

## Phases

| # | Phase | Gate to leave it |
|---|---|---|
| 0 | Reframe and restructure | legacy suites green from `legacy/`; Go module + CI in place |
| 1 | GCP foundation + **Gate Zero** | a no-op Go `ext_proc` filter logs both phases for traffic to the GKE public IP |
| 2 | x402 V2 done properly | golden vectors pass; buyer CLI pays the real facilitator with V2 headers |
| 3 | The enforcing filter + settler | 402 → paid 200 → replay rejected → 5xx not charged, live; all probes blocked |
| 4 | `PaymentPolicy` CRD + controller | apply six lines of YAML → route priced; delete → free; backend never restarts |
| 5 | Showcase | scripted five-minute demo + SLI dashboard on GKE over TLS |
| 6 | Testnet confidence | 7-day soak, chaos runs, reconciliation at zero diff, measurement campaign |
| 7 | Mainnet | only after Phase 6's gate; canary first |
| 8 | Stretch | `upto`, then `auth-capture` ([what it would take](docs/design/auth-capture.md)); `batch-settlement` if the request-path fallbacks do not hold ([what it would take](docs/design/batch-settlement.md)); the same filter as a GCP Service Extensions callout |

### Phase 1 — GCP foundation and Gate Zero

Terraform for a zonal GKE Standard cluster on a spot node pool, Artifact Registry,
Workload Identity Federation for CI, a budget alert and a static IP. Envoy Gateway at a
pinned version (v1.9.2) and in-cluster Redis. Then the most important gate in
the plan: a Go `ext_proc` filter that only logs, attached through `EnvoyExtensionPolicy`,
with `processingMode` and `messageTimeout` set explicitly and recorded — before
any payment logic exists. (Envoy Gateway sends neither phase unless `processingMode`
asks for it, and does not expose Envoy's `max_message_timeout`.)

### Phase 2 — x402 V2 done properly

V2 moved payment data into headers (`PAYMENT-REQUIRED` / `PAYMENT-SIGNATURE` /
`PAYMENT-RESPONSE`), which is what lets a filter decide in `request_headers` without ever
buffering a body. The Radius facilitator's `/supported` (checked 2026-10-12) advertises
V2 `exact` on SBC with two transfer methods, `permit2` and `eip3009`, selected by the
spec's `extra.assetTransferMethod`. The filter starts with `eip3009`, the spec's default:
a signature plus a balance is the whole local check, and the buyer needs no approval
transaction. `permit2` follows, for tokens without EIP-3009. EIP-3009 settles on SBC
testnet (measured), although Radius's docs list it as unavailable for SBC. The legacy app's
real deviation was filing its escrow under `scheme: exact` — that goes. The validator
keeps every lesson from §10: requirements
come from server config, never from the client; price is a floor; asset, `payTo` and
network must match. Chain-specific values live in network profiles, so mainnet is
configuration, not a fork.

### Phase 3 — The enforcing filter

Envoy's `ext_proc.message_timeout` defaults to **200ms**. When it expires, the default
(`failure_mode_allow: false`) is a **504**; with `failure_mode_allow: true` the response
is **released unpaid**. Measured on Radius testnet, facilitator `/verify` takes ~300 ms
(median) and `/settle` 0.6–1.1 s, so neither belongs on the request path. The shape that
fits ([design note](docs/design/request-path-budget.md)):

1. `request_headers`: no payment → `ImmediateResponse` 402. Payment → verify **locally**
   (EIP-712 `ecrecover`, requirements match, balance from a cached RPC read; Permit2 adds
   its allowance to the same read), **reserve in Redis** (nonce `SET NX` that outlives the signature's
   deadline + per-payer in-flight sum), continue. All under a filter-owned deadline
   shorter than Envoy's timeout, so the filter, not Envoy, answers when it is slow.
   A valid payer is not failed for our slowness where a bounded fallback exists: the
   RPC connection stays warm, a timed-out balance read still fills the cache for the
   retry, and a returning payer is served on a recent cached balance (decided after the
   baseline, in the design note).
2. Response: 2xx → enqueue capture, release immediately. 5xx → drop the reservation;
   nobody is charged.
3. `x402-settler` workers settle off-path from a Redis Stream, idempotently.

The synchronous capture mode is built too, with a 2 s budget, to measure whether
settle-then-respond can fit from inside the cluster.

### Phase 4 — The platform layer

A `PaymentPolicy` CRD, direct-attached to one HTTPRoute per Gateway API GEP-713, with
CEL validation and standard status conditions. The controller projects accepted policies
into the filter's price table and owns the `EnvoyExtensionPolicy`, so routes without a
policy never touch the filter.

**Correctness SLIs** — a payment filter's SLIs are correctness, not availability:

| SLI | Target |
|---|---|
| unpaid responses released / responses released | 0 |
| settlements captured / paid responses released | 1.0 |
| paid but undelivered | 0 |
| reservations expired unreleased | 0 |
| filter added latency p99 | within budget |

### Phase 6 — Testnet confidence, and the measurement campaign

Predictions committed before results: the `message_timeout` sweep (200ms / 1s / 5s /
30s); the overdraft with a balance funded for exactly *k* calls, predicting `A = N/k`;
replay across replicas, then with Redis killed (fail-open vs fail-closed); pod deletion
under load; per-request latency decomposition. Rail metrics alongside them: receipt
latency p50/p99, time at risk per paid request, settlement fee as a share of price,
settlement misses within the authorization window — and, optionally, the same filter
pointed at a reference rail (Base Sepolia through the public x402 facilitator, a
network-profile change) so the comparison in [Why Radius](#why-radius) is measured
rather than asserted. A reconciliation job checks captured
payments against on-chain transfers. Mainnet waits on a 7-day soak with every SLI at
target and reconciliation at zero diff.

### Phase 7 — Mainnet

Radius mainnet (`eip155:723487`) already has an x402 V2 facilitator at
`facilitator.radiustech.xyz` with the same `exact` kinds as testnet, so this is a network
profile switch, re-verified at cutover. Then: isolated namespace and Redis (AOF on a persistent volume), a
cold `payTo`, image promotion by digest from the soaked testnet build, a capped price in
CEL, and a canary on one route with one wallet before anyone else's.

## Why Radius

The filter's design has a cost it cannot avoid: it releases the response in
milliseconds and settles afterwards, so from that moment until settlement lands the
seller holds **unsettled revenue — the time at risk**. Replays, concurrent overdrafts and
expiring authorizations all live in that gap. The rail decides how wide it is, and
everything else in the filter follows from it.

| Radius property | Evidence (checked 2026-10-12, measured 2026-10-16) | What it buys the filter |
|---|---|---|
| Finality on the timescale of the request — no block interval, no reorgs | `eth_blockNumber` returns a millisecond timestamp; docs: a sync receipt is "~100 ms, final"; measured: transfer to final receipt 100–179 ms, median 112 ms | Time at risk shrinks to roughly one request. Reservation TTLs are seconds, not minutes. The settler needs no confirmation-depth logic: one receipt is final. |
| Fixed fee, no bidding, failed transactions free | transaction-cost API: `$0.000100` per transfer | Prices have a computable floor (the fee is 10% of a $0.001 call, 1% of a $0.01 one), so a policy can be rejected for pricing below its own settlement cost. Settler retries never cost money. |
| Stablecoin-native | SBC is priced in USD; gas through Turnstile; facilitator offers `eip2612GasSponsoring` | Buyers — including agents — hold one asset. Nothing in the path manages a native token. |
| x402 V2 facilitator on testnet **and** mainnet | `/supported` on both; 10 testnet settlements, `permit2` and `eip3009` | No contract and no keys in the cluster, on either network. |

**The question this sets up for measurement.** On a ~12s-block chain, or even a ~2s L2,
the time at risk is an order of magnitude longer than the request it is attached to. On
Radius the chain itself finalizes inside Envoy's default budget (112 ms median, measured).
The facilitator round trip is the slower part: 0.82 s median, up to 1.13 s, measured from
a laptop. Whether `captureMode: Sync`, the spec's default *settle, then respond*, fits a
1 s budget from inside the cluster is an open question for Phase 6. If it does, routes on
Radius can choose their settlement ordering instead of being forced into async. The
measurements exist to answer that with numbers anyone can reproduce
([`tools/latency/`](tools/latency/README.md)).

## Related work

[Anteroom](https://anteroom.dev) ([source](https://github.com/radiustechsystems/anteroom))
is a self-hosted reverse proxy that gates a single app behind proof-of-work, with an
experimental x402 pay door that settles before issuing a time-based pass. This project is
the complement: per-request payment inside a shared gateway, settled after the response,
with reservations shared across replicas. *Anteroom is the front door for one app; this
is the toll layer for a platform's gateway.*

## Explicitly not doing

No new contract work (see above). No GPU inference backends. No agent-protocol
extensions (AP2/A2A). No signing in Go: the filter never signs anything — buyers sign,
and the facilitator settles.
