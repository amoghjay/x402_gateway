# Roadmap to v1.0

**Status (2026-11-22).** The gate is live on GKE with Envoy Gateway, on Radius testnet
and Base Sepolia. It holds each paid response until its payment has settled, and no
failure drill has released a response unpaid. Reading seller policies from Radius
Platform (allow, deny, rate limits, free quotas) is merged and unit-tested but has not
yet run on the cluster; charging on those policies is in review. The gate is testnet
only, its filter and settler export no metrics, and it installs only by following
[`deploy/k8s/`](deploy/k8s/README.md).

This roadmap has one finish line, v1.0, and four phases to reach it. Anything that is
not needed to cross that line is listed under [After v1.0](#after-v10) and waits.

## What v1.0 means

v1.0 is done when all seven of these are true, each shown by a test anyone can rerun:

| # | Claim | Shown by |
|---|---|---|
| 1 | **Nothing is served unpaid.** | The drill set passes on the release candidate: cancelled and replayed payments, a drained wallet, one payment sent to two replicas, Redis killed, a filter pod killed mid-settle, the facilitator slow, hung and unreachable. |
| 2 | **A policy is more than a price.** | One route can allow, deny, rate-limit, give a free quota, check a bot's signature and offer several payment options, from a policy the seller edits without touching the cluster. Every public conformance vector passes in `go test`. |
| 3 | **A stranger can install it.** | One manifest or chart on Envoy Gateway, and `docker compose up` with plain Envoy and no cluster. Both reach a paid 200 by following the README alone. |
| 4 | **Standard clients can pay.** | The reference x402 clients (TypeScript, Go, Python) and `radius-cli` pay through a real Envoy in CI. Payments signed by contract wallets are accepted. |
| 5 | **It can be operated.** | Prometheus metrics for every decision and every settlement outcome, alert rules, and no payment headers in logs. |
| 6 | **Its cost is measured.** | A load test with stated percentiles, and a side-by-side benchmark against single-container payment gateways on the same cluster, API, buyer and load. Raw results in the repo. |
| 7 | **It has taken real money.** | A 7-day testnet soak with every correctness indicator at target, then a mainnet canary on one route. |

**Correctness indicators** (a payment gate is judged on these before availability):

| Indicator | Target |
|---|---|
| unpaid responses released / responses released | 0 |
| paid but undelivered, not recorded for refund | 0 |
| settlements confirmed on chain / paid responses released | 1.0 |
| reservations expired unreleased | 0 |
| added latency, p99 | stated, from the load test |

## Done so far

| Milestone | What it delivered | Evidence |
|---|---|---|
| Core path (M1–M5) | The filter, the `PaymentPolicy` CRD and controller, the settler, replay and overdraft guards shared across replicas. Live on GKE since 2026-11-04. | [docs/architecture.md](docs/architecture.md#status) |
| Money safety (M6a) | `captureMode: Sync` as the default: verify with the facilitator on the request, check on chain and settle while Envoy holds the response. A record of every payment before it settles, and a refund list for the one charged-but-unserved case. Chain, token and facilitator as a profile, with Radius testnet and Base Sepolia both drilled. | [docs/design/request-path-budget.md](docs/design/request-path-budget.md) |
| Policy, first half (M6b) | The controller polls Radius Platform for a project's compiled policy and keeps the last good version. The filter evaluates path rules, allow and deny, rate limits and free quotas, with counters in Redis. 73 of 73 public conformance vectors pass. | `internal/policy`, `internal/counter`, `internal/platform` |

## Phases to v1.0

One phase is in progress at a time, except the soak, which runs on its own once
started. A phase closes when its exit test passes. Each phase names what gets cut first
if it runs long, so a phase is shortened before it is extended.

### Phase 1 — Policy (M6b), in progress

What makes this gate more than a price per route: the seller's rules live in Radius
Platform, and the gate pulls and enforces them.

- Charge on policy routes with EIP-3009 options (#91).
- A facilitator-only path for Permit2 and tokens the gate does not know (#92).
  Unit-tested; its live payment waits under *After v1.0*.
- Bot-signature checks (Web Bot Auth) for rules that depend on who the caller is.
- The 402 lists every payment option, as JSON, markdown or HTML by `Accept`.
- Docs, drill scripts and demo manifests for policy routes; a check on GKE against a
  real Platform project.

**Exit test.** On the cluster, one Platform project drives five routes: charged, free,
denied, metered (free quota, then charged) and rate-limited. Each behaves as the policy
says, a payment in each offered EIP-3009 option settles, and editing the policy in
Platform changes the gate's behaviour with no `kubectl`. The Permit2 option is offered
but not paid live: no client we can fund pays it yet (see *After v1.0*).

**Cut first.** HTML rendering of the option list (JSON and markdown stay).

### Phase 2 — Adoptable (M6c)

Everything a team needs to try it in ten minutes and run it without us.

- **Metrics and alerts.** Counters and histograms for every filter decision, settlement
  outcome and dependency call; alert rules for the correctness indicators.
- **Standalone mode.** `-prices-file` in place of the ConfigMap watch, plain Envoy route
  names, a `validate` command for a price file, and a `deploy/compose/` with Envoy,
  the filter, Redis, the settler and a demo backend.
- **Refuse unpriced routes.** A flag that answers 503 for a route missing from the
  price table. On by default in standalone mode, where one filter covers every route
  and a typo would otherwise serve free.
- **Install.** One manifest or Helm chart with public images.
- **Client interop in CI.** The reference TypeScript, Go and Python clients and
  `radius-cli`, paying through a real Envoy.
- **Contract-wallet signatures.** First establish what the local signature check does
  with an EIP-1271 or ERC-6492 signature today; then let those through to the
  facilitator's verification instead of refusing them.
- **Limits on unsettled money in `Async` mode.** A per-payer and a per-route ceiling,
  so the opt-in serve-first mode has a stated worst case.
- **Load test.** Stated percentiles at a stated concurrency.

**Exit test.** Someone who has not seen the repo installs it both ways from the README,
pays with a reference client, and reads the latency of that request from a metric.

**Cut first.** The `Async` ceilings, then the Helm chart (a plain manifest stays).

### Phase 3 — Proof (M6d)

- Start the 7-day soak on the Phase 2 build. It runs alongside the rest of this phase.
- TLS between Envoy and the filter.
- A reconciliation check: every payment the gate recorded against the transfers on chain.
- The benchmark: this gate and single-container payment gateways on the same cluster,
  API, buyer and load. Added latency at p50 and p99, behaviour with two replicas, and
  behaviour with the facilitator unreachable.
- Publish the results, the method and a recorded drill.

**Exit test.** The soak ends with every correctness indicator at target and
reconciliation at zero difference, and the benchmark can be rerun from the repo.

**Cut first.** A third gateway in the benchmark (one comparison stays).

### Phase 4 — Mainnet and v1.0

- Radius mainnet first: its facilitator already offers the same payment kinds as
  testnet, so this is a profile switch, re-verified at cutover.
- Isolated namespace and Redis, a cold `payTo`, images promoted by digest from the
  soaked build, a capped price, and a canary on one route with one wallet.
- Base mainnet as a second canary once a mainnet facilitator is configured.
- Tag v1.0. Bring the README, this file and the architecture doc in line with what shipped.

**Exit test.** Real payments on mainnet for 48 hours on the canary route with every
correctness indicator at target.

**Cut first.** Base mainnet moves to v1.1 if its facilitator needs an account we do not
have yet.

## After v1.0

Ordered. Each item waits for its trigger, and none of them blocks v1.0.

| Item | What it is | Trigger to start |
|---|---|---|
| GKE Gateway and Google Cloud load balancers | The same filter as a Service Extensions callout. Starts with a logging-only filter to learn how the route is identified and whether the callout can answer 402 and hold a response. | v1.0 tagged |
| The fast lane | An opt-in mode where the gate submits the settlement itself while still holding the response. The settle step alone measured a median of 91 ms on Radius from GKE; end to end it is unbuilt. Needs keys in the cluster and a pool of them. | The benchmark shows the facilitator is most of a paid request's wait |
| Permit2 paid live | A payment in the Platform's Permit2 option (SBC on Base Sepolia) through the facilitator-only path, and a filter killed mid-settle on that path. The buyer script with the reference client is in [`tools/drills/permit2-buyer`](tools/drills/permit2-buyer/); [why it has not run](docs/design/request-path-budget.md#policy-routes-against-the-production-platform-2026-11-23). | SBC on Base Sepolia for our buyer, `radius-cli` able to pay that option, or a seller asking for Permit2 |
| Facilitator failover | An ordered list of facilitators per option, with a cooldown after failures. | A facilitator outage during the soak, or a second mainnet facilitator in use |
| kgateway | A second Envoy-based gateway through its own external-processing attachment. | GKE Gateway working, or a user asking |
| Browser payment page | A page where a person connects a wallet and pays, with a short-lived pass afterwards. Needs its own threat review. | A seller with human traffic |
| Payment channels (`batch-settlement`) | Many requests settled as one transfer. [What it would take](docs/design/batch-settlement.md). | Per-request settlement cost or latency shown to block a real use |
| Usage pricing (`upto`, `auth-capture`) | The final amount is known after the response. [What it would take](docs/design/auth-capture.md). | A seller whose price depends on the response |
| More chains | Polygon, Arbitrum, Arc as profiles; Solana through a facilitator only. | A seller or buyer on that chain |
| MPP | A second protocol on the same 402 status, behind the same policy. | The spec read and a facilitator that speaks it |
| Receipts endpoint and webhooks | A seller-facing record of settled payments. | A seller asking for accounting |

## Not planned

- A hosted service or dashboard. Radius Platform is the control plane for policies.
- Plugins for proxies that are not Envoy (Kong, NGINX, Traefik). Others maintain those.
- Running at CDN edges. A cluster filter does not run there; the fitting design, pay
  once and receive a signed token, stays a design note.
- A contract of our own. The gate settles through facilitators and standard tokens.
- GPU inference backends, and agent-protocol extensions (AP2, A2A).

## Scope rules

These exist so the project finishes.

1. A new idea goes to *After v1.0* with a trigger. It does not enter the current phase.
2. A phase closes on its exit test, not on its list being empty.
3. When a phase runs long, its *cut first* items move to *After v1.0* before anything
   else changes.
4. Every number in the README and docs is measured and dated, or labelled an estimate.
5. A claim is added to the README only after the test that shows it has run.

## Prior art, stated up front

The two flaws that moved enforcement into the gateway, replay across replicas and
concurrent overdraft ([REPORT §10.4 and §10.5](legacy/llm-gateway/REPORT.md)), correspond
to flaw classes documented independently in
[arXiv 2605.30998](https://arxiv.org/abs/2605.30998) (*Free-Riding in the AI Economy*,
May 2026). The contribution here is not the discovery. It is the mitigation built into a
production data plane, with what it costs to operate measured.

Enforcing x402 in infrastructure with the application untouched is not new either:

- **AWS** documents the pattern on CloudFront and Lambda@Edge
  ([guide](https://builder.aws.com/content/38fLQk6zKRfLnaUNzcLPsUexUlZ/monetize-any-http-application-with-x402-and-cloudfront-lambdaedge)):
  verify on the origin request, settle on the origin response for a status below 400.
- **Cloudflare** has announced a Monetization Gateway.
- **[traefik-x402](https://github.com/lukaszraczylo/traefik-x402)** (October 2026, MIT)
  is a Traefik middleware that verifies before the upstream and settles before the first
  byte reaches the client. Its replay guard is in memory per Traefik instance.
- Other open-source gateways take the Envoy and Gateway API route as well. The space is
  moving quickly, so this list is a snapshot.

What this project adds:

- **Inside the cluster's own Envoy,** not a CDN and not a second proxy.
- **The response is held until the payment has settled,** with replay and overdraft
  state shared by every replica. Drilled, with the results in the repo.
- **A policy, not only a price.** Rules, limits, quotas and payment options come from
  Radius Platform, or a route is priced directly with a `PaymentPolicy`.
- **Chain, token and facilitator are configuration,** run on Radius and on Base.

## Why Radius

The gate holds a response while its payment settles, so the rail decides how long a
paying caller waits. Through a facilitator the held response took about 0.55 s at the
median on both Radius testnet and Base Sepolia (GKE, 2026-11-19, n=100), and most of that
is the facilitator round trip, not the chain.

| Radius property | Evidence | What it gives the gate |
|---|---|---|
| Finality on the timescale of the request, no block interval, no reorgs | Docs: a sync receipt is "~100 ms, final". Measured from GKE: a self-submitted transfer reached its final receipt in 91 ms at the median (500 settles, 2026-11-19) | One receipt is final, so there is no confirmation-depth logic. It is what makes the fast lane worth building. |
| A fixed, small fee with no bidding | Transaction-cost API: `$0.000100` per transfer | A price has a computable floor, so a $0.001 call is still economic. |
| Stablecoin-native | SBC is priced in USD; the facilitator sponsors gas | A buyer, including an agent, holds one asset. |
| An x402 V2 facilitator on testnet and mainnet | `/supported` on both | No contract and no keys in the cluster, on either network. |

Whether Radius is measurably faster than another chain for a held response is an open
question until the fast lane runs end to end and the same benchmark runs elsewhere.
Through facilitators, the two chains measured the same.

## Related work

[Anteroom](https://anteroom.dev) ([source](https://github.com/radiustechsystems/anteroom))
is a self-hosted reverse proxy that gates a single app behind proof-of-work, with an
experimental x402 pay door. It is the front door for one app; this is the toll layer
for a platform's gateway.

## How it got here

The project began as an LLM pay-per-prompt gateway with its own escrow contract, which
still lives, with its tests green, under [`legacy/llm-gateway/`](legacy/llm-gateway/).
An audit of it found two flaws that an application cannot fix on its own, because they
come from where enforcement runs. Enforcement moved into the gateway, the escrow
contract was shelved in favour of facilitator settlement, and the backend became any
unmodified HTTP service. The first data-plane design served the response and settled
afterwards; measuring it showed the loss was unbounded, and M6a replaced it with
settling while the response is held.
