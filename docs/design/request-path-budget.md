# The request-path budget: what happens past 200 ms, and how the filter stays inside it

Status: proposed design for Phase 3. Measurements taken 2026-10-16 (laptop) and 2026-10-28 (GKE: timeout drill and in-cluster baseline).

## The constraint

Envoy gives an `ext_proc` filter **200 ms per message** by default. The timer restarts
on each message Envoy sends the filter (request headers, then response headers). It is a
budget for each phase, not for the whole request.

| Setting | Envoy | Envoy Gateway (`EnvoyExtensionPolicy.extProc`) |
|---|---|---|
| Per-message timeout | `message_timeout`, default 200 ms | `messageTimeout`, default 200 ms |
| Behaviour on failure | `failure_mode_allow`, default `false` | `failOpen`, default `false` |
| Filter asks for more time | `override_message_timeout`, capped by `max_message_timeout` (default 0 = disabled) | not exposed in `ExtProc`; would need an `EnvoyPatchPolicy` |

**By default a timeout fails closed.** Envoy sends a local reply, **504** for a timeout
and 500 for other failures. A response goes out unpaid only when the filter is set to
fail open. The filter's late answer is ignored. The route timeout (15 s by default) is
separate.

What a timeout means for a payment, by phase:

| Timeout during | `failOpen: false` (default) | `failOpen: true` |
|---|---|---|
| Request headers: check and reserve | Buyer gets a 504 and the backend never runs. Nothing is charged, but a valid payer gets an error and any reservation is left behind. | The request reaches the backend unchecked: **free access**. |
| Response headers: release and queue the settlement | The backend did the work, but the buyer gets a 504. A synchronous settle that finished late means **charged, nothing delivered**. | The response goes out but may never be recorded, so the settlement is missed: **lost revenue**. |

`failOpen: true` turns any slowdown into free access, so the filter never uses it.

## What the facilitator costs on the request path

Measured on Radius testnet from a laptop (about 70 ms network round trip to the RPC).
Script: [`tools/latency/measure_settle.py`](../../tools/latency/measure_settle.py).

| Call | n | min | median | max |
|---|---|---|---|---|
| SBC transfer, `eth_sendRawTransactionSync`, submit to **final** receipt | 8 | 100 ms | 112 ms | 179 ms |
| Facilitator `/verify` (Permit2 + EIP-3009) | 10 | 186 ms | ~300 ms | 826 ms |
| Facilitator `/settle` (Permit2 + EIP-3009) | 10 | 604 ms | 815 ms | 1129 ms |

- The **chain** finishes inside Envoy's 200 ms default. Radius's docs say "~100 ms, final".
- **`/verify` on the request path breaks the default budget** for most requests.
- **Settle-then-respond** (x402's default order, our `captureMode: Sync`) does not reliably fit in 1 s through the facilitator. The maximum was 1.13 s.
- **EIP-3009 settles on SBC testnet.** Radius's docs say SBC has no EIP-3009; `/supported` lists it first, and 4 of the 10 settlements above used it.

## What "verify" has to check, and who says so

- [x402 exact EVM scheme](https://github.com/coinbase/x402/blob/main/specs/schemes/exact/scheme_exact_evm.md):
  - signature recovers to the payer;
  - sufficient balance;
  - amount and validity window meet the requirements;
  - token and network match;
  - for Permit2: Permit2 allowance;
  - simulation of the settlement call.
- Radius facilitator `/verify` returns `isValid` when "the payment signature and balance check pass".
- Radius docs, *Check settlement and delivery*: "If you verify payments yourself, check the payment against your own requirements before settling":
  - `scheme`, `network`, `asset` and `payTo` match what you offered;
  - `amount` is at least the price;
  - the signature is valid and the validity window has not expired.
- Radius docs: SBC has no EIP-1271. Payers are plain accounts, so `ecrecover` is a complete signature check, with no contract call.

All of this can run inside the filter except simulation, and `/settle` simulates anyway.

## What each check costs

Script: [`tools/latency/measure_checks.py`](../../tools/latency/measure_checks.py) and
[`tools/latency/verifybench`](../../tools/latency/verifybench).

| Check | Cost | Source |
|---|---|---|
| Requirements match against the price table | microseconds | trivial |
| EIP-712 Permit2 hash + `ecrecover`, go-ethereum v1.14.12 | **~95 µs** with `CGO_ENABLED=0` (as `make build` ships), ~40 µs with cgo | benchmark, Apple M4 Max |
| `balanceOf` via `eth_call` | p50 81 ms, max 225 ms (testnet) / 168 ms (mainnet) | measured, laptop, n=20 |
| `balanceOf` + Permit2 `allowance` in **one JSON-RPC batch** | p50 ~80 ms, p90 86–111 ms, max 166 ms | measured, both networks, n=20 |
| `balanceOf` from GKE, kept-alive connection | p50 59–62 ms, p99 119–123 ms, max 1.4–2.0 s; **0.74% over 140 ms** | measured, system pool, 1/s for 10 min, n≈540 per network |
| `balanceOf` from GKE, **new connection** (TCP + TLS) | p50 ~265 ms, max 518 ms; **100% over 140 ms** | measured, same run, n=19 per network |
| Redis round trip (`PING`) from another pod | p50 0.09 ms, p99 0.81 ms, max 3.7 ms | measured, system pool, n=20,000 |
| Nonce `SET … NX EX` (the replay guard) | p50 0.10 ms, p99 0.79 ms, max 8.3 ms, AOF on | measured, same run; the in-flight sum adds a Lua script on top |

The balance check is required. Without it, an empty wallet can sign valid payments
indefinitely. Each one passes the signature check, the response is served, and only
the settlement fails later. The in-flight sum also needs a known balance to subtract
from.

## Proposed design

**Request path**, under a filter-owned deadline of 150 ms:

1. Decode `PAYMENT-SIGNATURE` and match it against the route's requirements.
2. Recover the signature (~95 µs).
3. Read the balance from the Redis cache (TTL of a few seconds). On a miss, call `balanceOf` (~80 ms) and cache the result. EIP-3009, the first transfer method, needs nothing else. Permit2 adds its `allowance` to the same JSON-RPC batch.
4. Check that cached balance minus in-flight reservations covers the price, then reserve the nonce and amount atomically.
5. Past the deadline, the filter itself answers 503 with `Retry-After` and releases the reservation. Envoy's 504 should only ever fire if the filter hangs. What else happens at the deadline is decided below ("serving a valid payer when our side is slow").

**After the response:** on a 2xx, queue the settlement; the settler calls `/settle` off the request path. On a 5xx, drop the reservation.

**Limiting losses:** a settlement that fails for lack of funds marks the payer in Redis, and the filter refuses that payer until the balance refreshes. A bad wallet gets at most one cache window of requests.

**Envoy settings**, generated by the PaymentPolicy controller and never set by app teams:

| `captureMode` | `messageTimeout` | Filter deadline | Why |
|---|---|---|---|
| `Async` (default) | 300 ms | 150 ms | the worst cache miss measured (225 ms, laptop) fits Envoy's timeout; the filter deadline is re-set from the in-cluster baseline at Gate Zero |
| `Sync` (default) | 10 s | request 250 ms + the facilitator's `/verify` (up to 2 s, ~0.3–0.4 s measured); held response: chain read (0.5 s, asked twice) + 5.5 s for `/settle` + up to 2.5 s asking the chain after an unclear settle; answered at once if `/settle` never reached the facilitator | measured settle max was 1.13 s (laptop, n=10); to be re-measured from GKE. Envoy Gateway has one `messageTimeout` for both phases |

`failOpen` is hard-coded to `false` and is not a PaymentPolicy field.

**Precedent.** Envoy's [`jwt_authn`](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/jwt_authn_filter)
filter verifies signatures locally, using keys fetched remotely and cached for
`cache_duration`, rather than calling the issuer on every request. This design does the
same with payment signatures and balances.

## The trade-off against settle-before-release

The closest prior work, [traefik-x402](https://github.com/lukaszraczylo/traefik-x402) and
AWS's CloudFront pattern, calls the facilitator inline:

1. `/verify` before the upstream;
2. `/settle` after a successful upstream status, before any byte reaches the client.

That guarantees **zero unpaid responses**. On Radius's facilitator it adds roughly **1.1 s**
per paid request (≈0.3 s verify + ≈0.8 s settle, medians measured above). It also has to run
in a proxy that tolerates holding a request that long; Traefik middleware can, but an Envoy
`ext_proc` filter on default settings cannot.

This design takes the other point on the curve: **~2 ms** on the request path, plus a bounded
window of unsettled revenue. The bounds are the balance check, the in-flight sum, and the loss
limit.

Shared Redis is what makes that safe. With settle-before-release, a replay on another replica
only costs duplicate upstream work, because its settlement fails before the body leaves. With
settle-after, that replay would be a free response, so the nonce must be claimed somewhere
every replica sees.

Both models stay available: settle-before-release is `captureMode: Sync` (built 2026-11-19,
unit-tested, not yet run on the cluster). The filter calls `/settle` on the response phase
while Envoy holds the backend's 2xx: settled releases it, a refusal becomes a 402 and an
unclear outcome a 503 in its place. Phase 6 measures the two side by side.

### Sync capture on GKE, first run (2026-11-19)

Images `e25dec140cb3`, Envoy Gateway v1.9.2, route `/anything/sync` on Radius testnet,
facilitator `facilitator.testnet.radiustech.xyz`, one buyer from a laptop. These are
functional checks with a handful of requests, not a latency baseline.

| Check | Result |
|---|---|
| Paid request | 200, `PAYMENT-RESPONSE` names the transaction; 4 of 4 |
| `/settle` as timed by the filter, from the pod | 583, 684, 922, 1157 ms (n=4) |
| Settlement refused after the backend answered 200 | Client got **402** with the facilitator's reason and none of the backend's body. Envoy Gateway does replace a held response with an `ImmediateResponse` on the response-headers phase |
| How the refusal was forced | A wallet holding exactly one price paid twice within the cached-balance window; the second passed the door and failed at `/settle` ("Insufficient balance: has 0, needs 1000"), 195 ms |
| A payment already settled outside the gate, sent through it | **Released.** The facilitator answered `/settle` with success and the old transaction in 88 ms |

The last row is a finding about the facilitator, and it changes the design. The gate
standard assumes a facilitator lets at most one settle of a payment succeed. Radius
testnet's answered a second `/settle` of the same payment with success. So a successful
`/settle` does not prove that this request paid, and the Redis nonce claim was the only
thing between a replay and a free response. The filter now reads `authorizationState`
before it settles: a nonce already used on chain is a replay and gets a 402 without a
`/settle` call; if the read fails, the response is withheld with a 503. The Redis claim
is still the first guard, and the only one against two copies arriving at once.
Open question for the Radius facilitator team: is the repeated success intended?

### Sync drills on GKE, both chains (2026-11-19)

Images `052abe3c9a04` (with the chain read before settling). Routes `/anything/sync`
(Radius testnet, SBC, Radius facilitator) and `/anything/base` (Base Sepolia, test USDC,
`x402.org/facilitator`). One buyer per chain, from a laptop, requests one after another.
The drills are `tools/drills/sync.sh`; the Redis and pod kills were `kubectl delete pod`.

**Latency of the held response**, as timed by the filter (chain read + `/settle`):

| Route | Paid requests | Settled | p50 | p90 | max |
|---|---|---|---|---|---|
| Radius testnet | 50 | 50 | 548 ms | 780 ms | 1,975 ms |
| Base Sepolia | 30 | 29 | 548 ms | 662 ms | 752 ms |

The request phase (verify, cached balance, reserve) took p50 1 ms on both.

**Drills.** "Unpaid" means a backend response released without a transfer.

| Drill | Radius testnet | Base Sepolia |
|---|---|---|
| Settlement refused after the backend answered (wallet holding one price pays twice) | 402, no backend body | 402 `invalid_exact_evm_insufficient_balance`, response withheld (2026-11-19, `426a878e6fad`) |
| Payment settled outside the gate, then sent through it | 402 `payment_already_used` (was 200 before the chain read) | 402 |
| Same, and the facilitator asked to settle it twice directly | second settle: **success** | second settle: refused, `invalid_exact_evm_nonce_already_used` |
| One signed payment, 12 copies at once | 11 x 402, 1 x 503, one price spent (see below) | 1 x 200, 11 x 402, one price spent |
| Paid once, Redis nonce records deleted, replayed | 402 | 402 `payment_already_used` from the chain read, 42 ms, no `/settle` (2026-11-19) |
| Redis pod killed | paid requests 503 for about 8 s, unpaid 402 throughout, then 200 again | not run (Redis does not depend on the chain) |
| Facilitator unreachable: its hostname resolved to a dead IP in the filter pods only (`hostAliases`) | 3 x 503 `settlement_unknown` in 4.7-5.2 s (2 s dial timeout + 2.5 s chain poll), not charged; unpaid requests 402 throughout (2026-11-19) | not run |
| Both filter pods force-killed about 0.35 s into a paid request | 200, paid; the kill missed the settle window, so this did not test a kill mid-settle | not run |

No drill released a response unpaid.

**Charged and not served: 2 of about 75 Radius requests.** Twice the Radius facilitator
took longer than 2 s to answer `/settle`. The shared HTTP client's 2 s limit on response
headers cut the call off (the settle budget was 3 s), the chain still showed the nonce
unused, and the filter answered 503 `settlement_unknown`. The transfer landed afterwards:
the buyer paid and got nothing. One of the two is the 503 in the burst row. Fixed after
this run: the facilitator call has its own HTTP client with no header limit, the settle
budget is 6 s, and after an unclear answer the filter asks the chain every 250 ms for
2.5 s before giving up, under a 10 s `messageTimeout`.

**Rerun with the fix (images `426a878e6fad`, 2026-11-19):** 100 paid requests on Radius
testnet, one after another: 100 x 200, 100 settled, no `settlement_unknown`, exactly 100
prices spent (3.407698 → 3.307698 SBC). Held response p50 539 ms, p90 754 ms, p99 983 ms,
max 1,011 ms. No `/settle` took longer than 2 s in this run, so the slow path the fix is
for was not exercised here; it is covered by unit tests only.

**Concurrent load (same images, same day):** 4 buyers at once from one wallet, 25 paid
requests each, Radius testnet. 99 x 200 and settled, 1 x 503; exactly 99 prices spent
(3.303698 → 3.204698 SBC). Held response p50 567 ms, p90 829 ms, max 1,631 ms. The 503 was
`dependency_unavailable`: the `authorizationState` read before `/settle` hit its 0.5 s
budget (`eth_call: context deadline exceeded`), so nothing was settled and the buyer was
not charged. Safe, but a valid payer got an error. Candidates: retry the read once inside
the budget, or give it more of the 10 s `messageTimeout`; not built.
What is still missing for the case that remains: a record of unknown outcomes that a
seller can refund or redeliver from.

**One refusal of a valid payment: 1 of 30 on Base.** The x402.org facilitator answered
`invalid_exact_evm_transaction_failed` in 280 ms. The gate returned 402 and the buyer
was not charged (balance checked). A retry when the nonce is still unused would have
served it; not built.

**Facilitator unreachable, a detail:** a connection that never opened means `/settle`
never reached the facilitator, so the outcome is known (not settled), yet the filter
polled the chain for 2.5 s and called it `settlement_unknown`. Fixed in #70 (below).

**Price table, found while setting up the drills:** a filter started with
`-networks=testnet` rejected the whole price table when one entry was on a network it
did not serve (`httpbin-base`), so a new pod never became ready. Fixed in #74: a broken
route is blocked (503 `policy_invalid`) and the rest of the table loads.

**Second round, images `811c9b1cc3ab` (#69, #70), 2026-11-19, Radius testnet.** Radius
testnet's `/settle` went through `tools/drills/fac-shim` (`-facilitator-url
testnet=…`); Base Sepolia kept its own facilitator. Every request is one paid request
from the laptop; "charged" is read from the buyer's balance.

| Drill | Client got | Charged | Filter decision |
|---|---|---|---|
| Proxy forwarding (baseline) | 200 in 1.2 s | once | `settled`, 902 ms |
| Facilitator answers 3 s late (the case behind the two charged 503s) | 200 in 4.2 s, twice | once each | `settled`, 4,027 / 4,073 ms |
| Facilitator answers 8 s late (past the 5.5 s settle budget) | 200 in 5.8 s | once | `settled_on_chain`, 5,612 ms: the chain showed the transfer |
| Facilitator hangs, never forwards | 503 `settlement_unknown` in 8.0 s | no | under Envoy's 10 s |
| Both filter pods deleted (graceful) 2 s into an 8 s settle | 200 in 5.8 s | once | the 20 s drain finished the request |
| Both filter pods **force-killed** 2 s into an 8 s settle | **503 in 3.0 s, empty body** (Envoy's `statusOnError`) | **once** | none: the pod died, no log line |
| Facilitator unreachable (dead IP) | 503 `dependency_unavailable` in 2.2 s, twice | no | dial timeout, answered at once (was ~5 s) |
| 4 buyers × 25 at once, after a warm-up request | 100 × 200 | exactly 100 prices | `settled` p50 556 ms, p90 835, p99 993, max 1,059; the pre-settle read never needed its retry this run |

The force kill is the one case left where a buyer pays and gets nothing. No log records
it, because the process that knew is gone; only the chain does. So the record of unknown
outcomes has to be written **before** `/settle` is called (a claim in Redis), and resolved
afterwards by reading `authorizationState`, not reconstructed from logs.

On the first paid request after the rollout, Radius answered 503 `deadline` once: the
payer's balance was not cached on a 20 s-old pod and the read took longer than the 250 ms
request budget. Not charged; a known request-phase limit, not new.

**Not run:** cancelAuthorization mid-request (needs gas in the payer wallet; the chain
read treats a cancelled nonce as used, which the replay drill covers).

**Third round: M6a complete, images `f1d39c699d7c`, 2026-11-20.** On the cluster: Sync
by default (all three demo routes), the held-payment record (#78), the retry after a
passing refusal (#79) and the facilitator's `/verify` on the request (#80).

| Check | Result |
|---|---|
| 6 paid requests on each Sync route (`/anything`, `/anything/sync` on Radius, `/anything/base` on Base) | 18 × 200, exactly one price each |
| Request phase with `/verify` (balance + reserve + facilitator) | Radius p50 210 ms, max 733 (n=12); Base p50 189 ms, max 419 (n=6); was ~1 ms without |
| Held response (chain read + settle) | Radius p50 632 ms, max 937; Base p50 600 ms, max 968 |
| Both filter pods force-killed 2 s into an 8 s settle (`fac-shim`, `-facilitator-verify=false` for the drill) | client 503 in 2.9 s, buyer charged; the payment was in `held`; **57 s later on the `outcomes` stream as `charged_unconfirmed`** with payer, route, amount, and logged by the settler |
| Normal paid requests | leave nothing in `held` |
| A policy with a zero `payTo` on the free route | `Accepted=False Invalid`, its EnvoyExtensionPolicy kept, the route answered **503 `policy_invalid`**; deleted, the route was free again (200) |

With this, every drill on the M6 plan's list has run or is covered by a unit test
(cancelAuthorization), no response was released unpaid, and the one way a buyer can still
pay without getting the response (the filter dies mid-settle) ends on the seller's
refund list instead of nowhere.

**Revised 2026-11-07: the window is not bounded.** Between our 200 and the settlement, the
payer can call `cancelAuthorization` on SBC (present on Radius, checked by `eth_call`) or
move the wallet's funds in one transaction, and every response served in that window goes
unpaid. The balance check and the in-flight sum cap what one wallet can take per window;
the loss limit marks that wallet afterwards. None of them caps the total, because wallets
are free to create. Every reference x402 implementation, traefik-x402 and radius-sdk settle
before the body leaves; the x402 spec, its issue #1169 and two 2026 papers name this race
and offer no remedy for `exact`. What changes the picture is the speed of settling
ourselves, measured below: the gap can be closed by settling first without paying the
facilitator's ~1 s. The design that follows from it is the M6 plan.

## Risks we accept

- **No simulation on the request path.** A paused token or a blocklisted payer shows up as a failed settlement. The loss limit above bounds it.
- **A balance cached for a few seconds can be stale.** The in-flight sum covers concurrent requests through this gateway. Spending elsewhere during the TTL is the remaining gap.
- **Caches in front of the gateway.** A shared cache could resell a paid response for free. Paid responses get `Cache-Control: private, no-store`, overriding the upstream's value.

## Measured on GKE: the timeout drill (2026-10-28)

Envoy Gateway v1.9.2, the no-op filter with a `-delay` flag, one `HTTPRoute` behind an
`EnvoyExtensionPolicy`. Durations are from Envoy's access log, not the client.

**What Envoy Gateway configures.** The Gate Zero policy set none of `messageTimeout`,
`failOpen` or `statusOnError`, so Envoy Gateway left `message_timeout`,
`failure_mode_allow` and `status_on_error` at Envoy's defaults (200 ms, `false`, unset)
and set `grpc_service.timeout: 10s`. All three are `EnvoyExtensionPolicy` fields in
v1.9.2 (`ext_proc_types.go`) and are honoured; the controller sets `messageTimeout` and
`statusOnError: 503` (M4). The per-message timeout is the one that fires.

**Raised 2026-11-04, after the first live paid calls:** `messageTimeout: 300ms` and a
filter deadline of 250 ms (from 200/150). The testnet RPC answered `balanceOf` in
130–270 ms that day, against the 60–80 ms this note measured in October, so with the
old budget almost every first-time payer took the one-time 503 + `Retry-After`. The
arithmetic above (hop 4 + verify 0.1 + Redis 1 + read) still holds; only the read's
number moved. To be re-measured under the Phase 6 soak.

| Filter state | `failOpen: false` | `failOpen: true` |
|---|---|---|
| Answers in 100 ms | 200; p50 104 ms (n=23) against 1 ms on an unpriced route. The hop costs about 4 ms. | — |
| Answers in 1 s | **504** at 200 ms ±1, empty body, `ext_proc_error_per-message_timeout_exceeded`. The backend is not called. `statusOnError` does **not** change it. | **200, unpaid**, after 200 ms. The access log reads `via_upstream`, like any success; only `ext_proc.failure_mode_allowed` counts it. |
| Returns a gRPC error | **`statusOnError`** (503 when set), 2–5 ms. `details` carries the code and the filter's message. | not tested |
| No ready pods | **500** `direct_response` from Envoy Gateway, 0 ms. `statusOnError` does **not** change it. | Envoy Gateway **drops the filter from the route**: 200, unpaid, no added latency, no `ext_proc` counter. Only the policy's `Accepted=False` shows it. |

What follows for the design:

- **A buyer sees 504, 503 or 500 depending on how the filter failed.** Only the filter's
  own deadline gives one answer (503 with `Retry-After`), so that deadline is required,
  not an optimisation.
- **`failOpen: true` is ruled out by measurement.** With no filter pods it makes the route
  free without slowing it down or counting anything. A per-route "open" mode, if ever
  built, lives inside the filter and covers only its dependencies.
- **Filter availability matters as much as filter latency**: no ready pods gives the least
  useful answer.
- **Stat names** (per listener, `http.<listener>.ext_proc.`): `streams_started`,
  `streams_closed`, `stream_msgs_sent`, `stream_msgs_received`, `message_timeouts`,
  `immediate_responses_sent`, `failure_mode_allowed`. They are not per route; the access
  log's `response_code_details` tells routes apart.

## Decided 2026-10-30: serving a valid payer when our side is slow

A payer who sent a valid payment should not get an error because a dependency of ours is
slow. Without a fallback they get a 503 and are not charged (settlement happens only after
a 2xx, and the reservation is released), but the call fails. Instead the seller absorbs
the failure inside a bounded risk budget. The in-cluster baseline below sizes it: a warm
RPC connection fits the deadline 99.26% of the time, a cold one never does, and a
returning payer is served from the cache.

**In M5, the core path:**

1. **Keep the RPC connection warm.** An idle connection guarantees a miss on the next
   cache miss, so the filter sends a keepalive (an HTTP/2 ping or `eth_blockNumber`)
   about every 20 s.
2. **Let a timed-out balance read finish and fill the cache.** At the deadline the filter
   answers 503 with `Retry-After: 1`, but the in-flight `balanceOf` runs to completion and
   stores its result, so the retry is a cache hit.
3. **Serve a returning payer on the last known balance.** When a refresh overruns the
   deadline, a cached balance younger than about 60 s, minus in-flight reservations,
   still counts. The risk is at most one cache window of requests per payer whose wallet
   was drained elsewhere, the same bound the loss limit already applies.
4. **`statusOnError: 503`** on the `EnvoyExtensionPolicy`, so a filter that errors returns
   a retryable code. It covers filter errors only; a timeout stays Envoy's 504 (see the
   drill above).
5. **Two filter replicas and a PodDisruptionBudget.** Zero ready pods is the worst
   answer: an Envoy Gateway 500, or a free route under `failOpen`.

**Residual.** A first-time payer on the 0.74% path gets one 503 with `Retry-After`, is not
charged, and the retry succeeds from the filled cache. Only a shared credit removes that,
and it is a business-risk call.

**Backlog, each with its trigger:**

- Hedged RPC (a second request fired when the first is slow): if the keyed RPC still
  spikes, or real traffic shows more than about 1% of calls over 140 ms.
- A small credit shared by all unknown payers (one global cap, not per wallet, because
  wallets are free to create): if first-request 503s matter to a seller.
- Background refresh of active payers' balances: if the real cache-miss rate is high.
- Keyed RPC A/B and an RPC load test: when an API key is in hand.

Redis being down has no safe fallback: without the replay guard, one signature buys
unlimited calls. If the fallbacks do not hold, [batch-settlement.md](batch-settlement.md)
removes the balance read from the request path with an escrow contract.

## Measured on GKE: the in-cluster baseline (2026-10-28)

Probes ran as Jobs on the system pool, next to the filter
([`tools/latency/k8s/`](../../tools/latency/k8s/)). Keyless RPC; the numbers are in the
table under "What each check costs".

- **Redis is not a factor.** The replay guard's `SET … NX EX` has a p99 under 1 ms.
- **A warm RPC connection fits the deadline 99.26% of the time.** A cache miss costs the
  hop (~4 ms) + signature (~0.1 ms) + Redis (~1 ms) + `balanceOf` (p99 123 ms), which is
  about 130 ms at p99, inside the filter's 150 ms. The 0.74% that does not fit includes
  spikes up to 2 s.
- **A new connection never fits.** TCP + TLS to the RPC's Cloudflare front pushes every
  cold call past 140 ms (p50 ~265 ms). A filter whose RPC connection has gone idle misses
  its deadline on the next cache miss, so the connection has to be kept warm.
- **How often a valid payer would hit the deadline** ≈ P(cache miss) × 0.74%, provided
  the connection is warm. A payer's first request is always a miss.

Not yet measured: the RPC with an API key (rate limits and whether the tail changes), the
RPC under load (no 429s at one call a second), and the facilitator from GKE.

## Measured on GKE: settling ourselves (2026-11-19)

Can the filter submit the EIP-3009 `transferWithAuthorization` itself, with
`eth_sendRawTransactionSync`, fast enough to settle *before* releasing the response?
[`cmd/x402-settlebench`](../../cmd/x402-settlebench) ran as a Job on the system pool
([manifest](../../tools/latency/k8s/settlebench-job.yaml)), testnet, a keyed RPC URL
unless stated, one submitter key paying gas, one payer signing 1-unit transfers to the
demo seller. Latency is submit to final receipt, measured in the pod.

**Latency, one at a time** (500 settles at 2/s, each preceded by an `eth_call` of the same
calldata): **500/500 settled.**

| | p50 | p90 | p99 | max | > 150 ms | > 250 ms | > 400 ms |
|---|---|---|---|---|---|---|---|
| settle (submit → final receipt) | 91 ms | 98 ms | 156 ms | 296 ms | 1.2% | 0.2% | 0% |
| `eth_call` pre-flight, same calldata | 72 ms | 78 ms | 114 ms | 1,584 ms | 0.4% | 0.4% | 0.2% |

So the chain executes and finalises the transfer in ~20 ms on top of the RPC round trip;
a settle costs about what the `balanceOf` read on today's request path costs (p50 60–270
ms across days, [above](#measured-on-gke-the-in-cluster-baseline-2026-10-28)). The
October laptop figure (100–179 ms) was the same thing seen through a slower network.

**Keyless, same shape** (200 settles at 4/s): **200/200**, p50 89 ms, p90 102 ms, p99
131 ms, max 135 ms; pre-flight p50 71 ms. No different from the keyed run, so the API
key buys quota (10 MGas/s), not latency. The 130–270 ms `balanceOf` days in November
were the endpoint, not the lack of a key.

**One key, back to back** (100 settles, each submitted as soon as the previous receipt
arrived, no pre-flight): **98 settled**, p50 138 ms, p90 148 ms, max 221 ms, which is
about **7 settles per second per key**. Latency is ~45 ms higher than at 2/s, so the node
serialises something per sender. The two misses were one event: a submit that got no
response within the client's 2 s header timeout, then the next nonce rejected as a gap
(code 6) because the stalled transaction had not executed; a nonce resync recovered the
run. A stall like that is the uncertain case the settler must handle: check the nonce or
`authorizationState` before resubmitting, never re-sign.

**Burst** (100 transactions signed with consecutive nonces, submitted at once from one
key): **1 settled, 99 rejected** with code 6 "nonce gap", each in 86–198 ms, the whole
burst in 1.7 s. The rejections cost nothing and leave the nonce where it was. Radius's
sync submit does not queue a future nonce; one key is a strictly serial pipe, so
throughput comes from a pool of keys. That is what the facilitator's 100-wallet pool is
for, and a self-settling filter needs the same.

**What a failed settle costs.** Each case: `eth_call` first (free, 70–90 ms, returns the
exact reason), then the real submit. **Every revert consumed the submitter's nonce and
paid gas**; the sync method returns a receipt with `status: 0x0`, not an error.

| Case | On-chain reason (`Error(string)`) | Fee |
|---|---|---|
| expired | `EIP3009: authorization is expired` | 0.00003 RUSD |
| not yet valid | `EIP3009: authorization is not yet valid` | 0.00003 |
| bad signature | `EIP3009: invalid signature` | 0.00005 |
| unfunded payer | `ERC20: transfer amount exceeds balance` | 0.00013 |
| reused nonce | `EIP3009: authorization is used` | 0.00003 |
| cancelled by the payer | `EIP3009: authorization is used` (same string; only the `AuthorizationCanceled` log tells them apart) | 0.00003 |
| success | — | 0.00014 (141,104 gas × 0.986 gwei) |

The payer's cancel transaction used 63,077 gas (~$0.00006): the attack on settle-after is
as cheap as the research said. For settle-first the consequence is different: a signature
from an unfunded wallet costs *us* 0.00013 if submitted blind, so the local verifier (which
already refuses expired, invalid and replayed signatures) has to be followed by a balance
check (the cached `balanceOf`, or the free `eth_call` at +70 ms) before the submit.

**Fees.** 141,104 gas at the fixed 0.986 gwei is 0.000139 RUSD per settle; the submitter
pays it in the native token, and the faucet funds RUSD as well as SBC. One thing is
unexplained: across every run the submitter's **SBC also fell in 0.01 steps, one step per
0.01 RUSD of cumulative gas** (500 settles: 0.07 SBC and 0.0696 RUSD; 98: 0.02 and
0.0136; 200: 0.03 and 0.0278), with no SBC `Transfer` from the submitter on chain and
nothing moved by `eth_call`s, keyed or keyless, successful or reverting. That is the
Turnstile's 0.01 SBC conversion step, but the converted value does not show up as native
balance, so as measured a settle costs about **2 × 0.00014**. Open question for the Radius
chain team; until answered, budget 0.0003 per settle.

## Policy routes on GKE, first run (2026-11-23)

The first live run of a route governed by a Radius Platform project, after the charge
paths merged (#91, #92). Images `15af45399319`. One `PaymentPolicy` with `spec.platform`
on `HTTPRoute httpbin-platform` (`/anything/platform`), reading a test-mode project in a
Radius Platform development environment whose compiled policy has five rules and three
payment options per charge. The controller reported `Synced=True`, and the Platform
reported the gate as connected on the current policy version, as an unidentified gate (it
does not know this gate's User-Agent).

| Route | Rule | Answer |
|---|---|---|
| `/free` | allow | 200 |
| `/blocked` | deny 403 `not_for_sale` | 403, body `{"code":"not_for_sale"}` |
| `/paid/:id` | charge 0.001 | 402 listing all three options, in the document's order |
| `/metered` | 3 free a minute per IP, then charge | 200, 200, 200, then 402 with `RateLimit-Limit: 3`, `RateLimit-Remaining: 0` |
| `/limited` | 5 a minute, one counter for everyone | 200 five times, then 429 with `RateLimit-*` and `Retry-After` |
| a path with no rule | the policy's default, allow | 200 |

The routes priced directly by a `PaymentPolicy` (`/get`, `/anything/sync`,
`/anything/base`) still answered 402, and `/anything/free` 200.

**Payments.** Two paid requests to `/paid/42` with our own buyer, each matched to the
option for its network and settled through the facilitator that option names:

| Option | Verify, before the backend | Response held to settle | Transfer on chain |
|---|---|---|---|
| SBC on Radius testnet, EIP-3009 | 520 ms | 658 ms | [`0x1fbb4e73…e7f0`](https://testnet.radiustech.xyz/tx/0x1fbb4e73990f373cc59950728e74d6135a924f3d533c571b530d3f94c76be7f0), 0.001 SBC to the seller |
| USDC on Base Sepolia, EIP-3009 | 418 ms | 363 ms | [`0x8dc253c3…d83d`](https://sepolia.basescan.org/tx/0x8dc253c36ccf8fa86b00e3ec68512b3c7d27980b8e2f8138a515e476fbb0d83d), 0.001 USDC to the seller |

Both receipts were read from the chain's RPC: status success, the option's token, the
buyer as sender, the seller as recipient, 1000 atomic units. The Base Sepolia payment went
through Radius's testnet facilitator, as the option says, not through the filter's own
default for that network. One request each is a check that the path works, not a latency
figure.

**Replay.** One signed payment for `/paid/…` sent four times: the first answered 200 with
`PAYMENT-RESPONSE`; the other three answered 402 `payment_already_used`, each still offering
all three options.

**A policy edit reaches the gate with no cluster command.** The `/blocked` rule was
changed to allow in the Platform: the route answered 200 five seconds later. It was changed
back: 403 again after 13 seconds. The policy's `Synced` condition followed the document's
version (2, 3, 4). The controller polls about once a minute, so up to a minute is expected.

**Not run yet** (as of this run; the next section ran the production Platform and the
unreachable drill, and says why Permit2 did not run).

- A payment in the third option (SBC on Base Sepolia by Permit2), which takes the
  facilitator-only path. Our buyer signs EIP-3009 only, so it needs another client. That
  run also checks that a real client's Permit2 `deadline` falls inside the offer's window,
  which the filter now requires.
- The Platform unreachable: the last good policy should stay in force.
- A filter killed mid-settle on the facilitator-only path, which leaves only a log line.
- The same checks against the production Platform.

**Seen along the way.**

- The 402's help text describes only the first option. The `accepts` list is complete.
- The Platform's wallet page showed both payments, with the paid URL and the transaction.
  Its per-project payment list stayed empty: that list is fed by gates reporting events,
  which this gate does not do.

## Policy routes against the production Platform (2026-11-23)

The same checks as the first run, now against the production Radius Platform, in a new
organization where we are admin. The project has the same five endpoints and rules, the
same seller wallet, test mode, and the same three options per charge; its compiled policy
was identical to the development one apart from its IDs. The controller runs with no
`-platform-api-url`, so it reads the production API by default, and `kubectl diff` of
`deploy/k8s/x402-system/` against the cluster is empty. Images `15af45399319`.

**A key without the right permission.** The first key we stored was accepted but refused
for this project: `403 forbidden: missing permission endpoints:read`. Until a fetch
succeeded, the Platform routes answered 503 `{"code":"policy_unavailable"}` (`noPolicy:
Deny`) and the routes priced directly by a `PaymentPolicy` kept answering 402. A connector
key made on the project's Gate page fixed it; the controller read the replaced Secret on
its next poll, about 30 seconds later, with no restart. The Platform then reported the
gate as connected on the current version, as an unidentified gate.

| Route | Answer |
|---|---|
| `/free`, and a path with no rule | 200 |
| `/blocked` | 403 `{"code":"not_for_sale"}` |
| `/paid/:id` | 402 listing all three options, in the document's order |
| `/metered` | 200 three times (`RateLimit-Remaining` 2, 1, 0), then 402 |
| `/limited` | 200 five times, then 429 with `Retry-After: 50` |

**Payments**, one in each EIP-3009 option, each settled through the facilitator the
option names (`facilitator.testnet.radiustech.xyz`):

| Option | Verify, before the backend | Response held to settle | Transfer on chain |
|---|---|---|---|
| SBC on Radius testnet | 601 ms | 804 ms | [`0xa3493e7f…aace`](https://testnet.radiustech.xyz/tx/0xa3493e7faaa720720b94101bdfc0f8e0453354610f39c94e13a9c05ae888aace) |
| USDC on Base Sepolia | 316 ms | 971 ms | [`0xe2f14179…d203`](https://sepolia.basescan.org/tx/0xe2f14179fade8d31e20b1486153a90c163e2fb529f3666a4704dc6f5d78cd203) |

Both receipts read from the chain's RPC: status success, the option's token, 1000 atomic
units from the buyer to the seller. As before, one request each shows the path works; it
is not a latency figure.

**Replay.** One signed payment sent four times: 200 with `PAYMENT-RESPONSE`, then three
402 `payment_already_used`, each still offering all three options.

**A policy edit reaches the gate with no cluster command.** `/blocked` changed to allow in
the Platform: 200 about 15 seconds after the edit was saved. Changed back: 403 about 20
seconds later.

**The Platform unreachable.** The controller's Deployment was patched with a
`hostAliases` entry sending `platform-api.radiustech.xyz` to an unroutable address, which
also restarted the controller. During the outage `/blocked` was changed to allow in the
Platform (version 5).

- For 2.5 minutes, sampled every 25 seconds, the routes kept enforcing version 4: free 200,
  blocked 403, paid 402. The restarted controller seeded version 4 from the price-table
  ConfigMap and never served without a policy.
- Every fetch failed with a client timeout; the policy's condition read `Synced=False
  FetchFailed … version 4 stays in force`, and the ConfigMap kept version 4.
- The Platform reported the gate as `behind`: on version 4 while version 5 was current,
  last fetch 3 minutes earlier.
- With the patch removed, the new controller fetched version 5 and `/blocked` answered 200
  18 seconds after the rollout finished. The rule was then restored (version 6, the same
  content as version 4): 403 again 19 seconds later.

**The Permit2 option was not paid.** It is the only option that takes the
facilitator-only path, and no client we can fund pays it:

- `radius-cli` 0.8.0 refuses it before signing. Its `sell check` against `/paid/:id`
  passes 30 checks and fails two, both on that option: the asset "is not USDC on
  base-sepolia … buyers refuse it (asset_mismatch)", and the domain "Stable Coin" v1 does
  not match USDC's. So Radius's own buyer cannot pay the Platform's default third option.
- The reference client `@x402/evm` 2.32.0 can: it signs Permit2 whenever the offer's
  `extra.assetTransferMethod` is `permit2`, for any token, with a `deadline` of now +
  `maxTimeoutSeconds` (60 s), inside the 90 s the filter allows. A buyer script with it is
  ready; it needs SBC on Base Sepolia, plus a little ETH for the one-time Permit2 approval
  because the option declares no gas sponsoring.
- SBC on Base Sepolia (`0xf9FB20B8…0F16`) has no faucet we could find, its `mint` is
  role-gated, and Uniswap v3 on Base Sepolia has no SBC pool against WETH or USDC.
- The Platform's catalog offers no other Permit2 token on a test network: USDC always
  compiles to EIP-3009.

**What did run on that path, unfunded.** A Permit2 payment signed by the reference client
for a buyer with no SBC and no allowance (the client pays only its built-in default
assets unless given `spendControls.allowedAssets`; the script allows exactly the offer's
token, capped at its amount), sent twice to `/paid/:id`:

- First: the gate identified the Permit2 payload, claimed its replay key, and asked the
  option's facilitator to verify it: refused in 511 ms ("Insufficient balance: has 0,
  needs 1000"). 402, nothing served, nothing settled.
- Second: 402 `payment_already_used` from the replay claim, without calling the
  facilitator.

So identification, the claim and the facilitator's verify are checked live on this path.
A settled payment, its held response and a filter killed mid-settle are not. They move to
*After v1.0* in the roadmap, with the trigger written there.

**Seen along the way.**

- On the facilitator-only path every log line carries `path` twice: the request's, and
  `"facilitator"` added by `facilitated.go`. Most log tools keep the last, so the request
  path is lost. Rename the second key (step 6). Fixed in step 6: the second key is now
  `via`.
- A payment the facilitator refused at verify keeps its replay claim until the claim
  expires (the payment's deadline plus a margin), so presenting it again answers
  `payment_already_used` instead of the refusal's reason. Nothing is served either way,
  and clients sign a new payment per request; worth a clearer answer in step 6. Fixed in
  step 6: the path now releases the claim (`reserve.Store.Unclaim`) whenever it answers
  before `/settle` is sent, since `/verify` moves no money; a resend gets the refusal's
  reason, and a retry after a `/verify` 503 is checked again.

## Web Bot Auth on GKE (2026-11-23)

The first live run of `signature_agent` rules (#100, #101), images `50d1bcddf591`, against
the production Platform project from the previous section. The test agent is a Cloudflare
Worker on a domain we control, serving one Ed25519 key at
`/.well-known/http-message-signatures-directory` (`max-age=300`); requests were signed the
way ChatGPT's agent signs: `Signature-Agent` as a string, `@authority @method @path
signature-agent` covered, valid 60 s, with a nonce and `tag="web-bot-auth"`.

**The controller fetched the key, and only that one.** A rule was added in the Platform in
front of `/blocked`'s deny: a verified signature agent from the test origin is allowed
(the policy compiled to `radius.policy/0.2`). 21 seconds after the edit, `Synced` read
`version 7 … loaded; signature agents: <origin> 1 keys`, and the price-table ConfigMap held
the key under `agents.json`, fetched from inside the cluster through Cloudflare with no
bot challenge.

| Request to `/blocked` (`/free`, `/paid/1` where noted) | Answer |
|---|---|
| unsigned | 403 `not_for_sale` |
| signed by the test agent | **200**, rule `…blocked/signed-agent`, logged `signatureAgent` |
| signed, with a query string added (path unchanged) | 200 |
| signed by another key, same `Signature-Agent` | 403 (no such key at that origin) |
| signed for `/other`, sent to `/blocked` (`@path` covered) | 403 |
| signed 200 s ago (valid 60 s) | 403 |
| `created` 200 s in the future | 403 |
| signed, to `/free` | 200 (its own rule) |
| signed, to `/paid/1` | 402: a verified agent still pays where the rule charges |

No request was refused because of its signature: every 403 is the deny rule, reached
because the agent rule did not match. With the rule removed (version 8, the content of
version 6) the controller dropped `agents.json` and a signed request was denied again.

**Seen along the way.** The Platform lists each controller pod that fetched in the last 24
hours as its own gate: after this rollout it showed the live controller `connected` on
version 7 next to the replaced pod on version 6 and the pod from the outage drill on
version 4, so the project's summary read `behind` although the only running gate was
current. A question for the Platform team: how gates are told apart, and whether a gate
that stopped fetching should drop out of the summary sooner.

## Phase 1 exit test on GKE (2026-11-23)

Step 6 deployed (images `1b5b9a0373c3`: #105, #106, #107), then the exit test run in one
pass with [`tools/drills/platform.sh all`](../../tools/drills/platform.sh) against the
production Platform project from the sections above (policy version 8, three options per
charge). `kubectl diff` of `deploy/k8s/x402-system/` against the cluster was empty after
the rollout; Redis was not restarted.

**`platform.sh all`: 10 checks, 0 failed.**

| Check | Expected | Got |
|---|---|---|
| `/free` | 200 | 200 |
| `/blocked` | 403 `not_for_sale` | 403 `not_for_sale` |
| `/paid/42`, no payment | 402 listing 3 options | 402, the 3 options in the document's order |
| `/metered` | 200 × 3, then 402 | 200 × 3 (`RateLimit-Remaining` 2, 1, 0), then 402 |
| `/limited` | 200 × 5, then 429 with `Retry-After` | 200 × 5, then 429, `Retry-After: 24` |
| pay, SBC on Radius testnet | 200 with a successful `PAYMENT-RESPONSE` | 200, [`0xa34b9705…8e9e`](https://testnet.radiustech.xyz/tx/0xa34b9705c74fb35363666bfd56c388b5d5e4650f7c0917716e33600dd8fb8e9e) |
| the same payment again | 402 `payment_already_used`, 3 options | 402 `payment_already_used`, 3 options |
| pay, USDC on Base Sepolia | 200 with a successful `PAYMENT-RESPONSE` | 200, [`0xabe8840f…32fd`](https://sepolia.basescan.org/tx/0xabe8840f7f8e854497e5f691afb0821c87870d7df9038502af48c425af5732fd) |
| the same payment again | 402 `payment_already_used`, 3 options | 402 `payment_already_used`, 3 options |

Both receipts read from the chain's RPC: status success, the option's token, 1000 atomic
units from the buyer to the seller.

**The policy edit with no `kubectl`** was not repeated on this build: it was shown on
this project earlier the same day (`/blocked` changed to allow in the Platform, 200 about
15 seconds later; changed back, 403 about 20 seconds later), and step 6 did not touch the
controller's fetch path. With it, every part of the exit test has run on the cluster.
Permit2 stays offered and unpaid, as the exit test allows.

**The new 402, live.** `GET /paid/42` with no payment, by `Accept`:

| `Accept` | Body |
|---|---|
| `*/*` (curl) | JSON |
| Chrome's default | HTML |
| `text/markdown, text/html;q=0.9, */*;q=0.8` | markdown |
| `text/markdown, text/html` | markdown (a tie goes to markdown) |
| `text/html;q=0.5, application/json` | JSON |

Every answer carries `Vary: Accept`. The `error` line names one command for every option
`radius-cli` can pay: `radius-cli --network testnet wallet pay get '<url>' --networks
radius,base --threshold 0.001 (3 payment options; see the response body)`. The JSON's
`options` give each option a `payWith`: `radius-cli` for SBC on Radius and USDC on Base
Sepolia, and for SBC on Base Sepolia by Permit2, which `radius-cli` 0.8.0 refuses, "an
x402 v2 client that supports Permit2, e.g. @x402/fetch".

**That command, run as printed** (radius-cli 0.8.0, with `--private-key` added for our
test wallets): the wallet holding SBC on Radius paid on Radius testnet, settled in 705 ms;
the wallet holding only USDC on Base Sepolia paid on Base Sepolia, settled in 980 ms. Two
requests show the path works; they are not a latency figure. Before #107 was built, the
same command form was run four ways on the previous images (each network alone, and both
with one funded): 4 of 4 paid on the network the wallet could cover.

## Sources

- [Radius docs](https://docs.radiustech.xyz/llms-full.txt): `eth_sendRawTransactionSync` error codes (4 timed out, accepted; 5 not admitted; 6 nonce gap), the Turnstile
- [EIP-155](https://eips.ethereum.org/EIPS/eip-155), [EIP-3009](https://eips.ethereum.org/EIPS/eip-3009)
- [Envoy ext_proc API](https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/filters/http/ext_proc/v3/ext_proc.proto)
- [Envoy Gateway `ExtProc` types](https://github.com/envoyproxy/gateway/blob/main/api/v1alpha1/ext_proc_types.go)
- [x402 exact EVM scheme](https://github.com/coinbase/x402/blob/main/specs/schemes/exact/scheme_exact_evm.md)
- [Radius docs](https://docs.radiustech.xyz/llms-full.txt): instant finality, `eth_sendRawTransactionSync`, facilitator API, SBC transfer methods, self-verification checks
- [Envoy JWT authn filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/jwt_authn_filter)
