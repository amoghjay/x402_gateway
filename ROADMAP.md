# Roadmap: payment enforcement as data-plane infrastructure

> **The claim this roadmap exists to measure.** An L7 proxy will not hold a connection
> open for a settlement rail, and when it gives up, its default is to serve the response
> unpaid. Payment enforcement in a data plane must therefore be reserve-then-capture —
> and the deliverable is what that costs to build, run, and get wrong. The finding
> generalises past x402 to any settlement rail slower than a few hundred milliseconds.

**Status.** Phase 0 (reframe and restructure) is in progress. The application-layer
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
plane** and measuring what it costs to operate. Cloudflare and AWS shipped x402
enforcement at their edges in 2026; a platform team on Envoy Gateway has no equivalent.

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
| 8 | Stretch | `upto`; the same filter as a GCP Service Extensions callout |

### Phase 1 — GCP foundation and Gate Zero

Terraform for a zonal GKE Standard cluster on a spot node pool, Artifact Registry,
Workload Identity Federation for CI, a budget alert and a static IP. Envoy Gateway at a
pinned version, in-cluster Redis, kube-prometheus-stack. Then the most important gate in
the plan: a Go `ext_proc` filter that only logs, attached through `EnvoyExtensionPolicy`,
with `message_timeout` and `max_message_timeout` set explicitly and recorded — before
any payment logic exists.

### Phase 2 — x402 V2 done properly

V2 moved payment data into headers (`PAYMENT-REQUIRED` / `PAYMENT-SIGNATURE` /
`PAYMENT-RESPONSE`), which is what lets a filter decide in `request_headers` without ever
buffering a body. Verify what the Radius facilitator actually supports (`/supported`)
before writing code against it. The validator keeps every lesson from §10: requirements
come from server config, never from the client; price is a floor; asset, `payTo` and
network must match. Chain-specific values live in network profiles, so mainnet is
configuration, not a fork.

### Phase 3 — The enforcing filter

Envoy's `ext_proc.message_timeout` defaults to **200ms**; with `failure_mode_allow: true`
an expired filter **releases the response unpaid**. An on-chain settlement is seconds.
The only shape that fits:

1. `request_headers`: no payment → `ImmediateResponse` 402. Payment → validate, verify,
   **reserve in Redis** (nonce `SET NX` + per-payer in-flight sum), continue.
2. Response: 2xx → enqueue capture, release immediately. 5xx → drop the reservation;
   nobody is charged.
3. `x402-settler` workers settle off-path from a Redis Stream, idempotently.

The synchronous capture mode is built too — it exists to be measured failing.

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
under load; per-request latency decomposition. A reconciliation job checks captured
payments against on-chain transfers. Mainnet waits on a 7-day soak with every SLI at
target and reconciliation at zero diff.

### Phase 7 — Mainnet

Network profile switch, isolated namespace and Redis (AOF on a persistent volume), a
cold `payTo`, image promotion by digest from the soaked testnet build, a capped price in
CEL, and a canary on one route with one wallet before anyone else's.

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
