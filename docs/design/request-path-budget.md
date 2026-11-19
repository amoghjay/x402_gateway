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
| `Sync` (opt-in) | 10 s | request 250 ms; held response: chain read (0.5 s, asked twice) + 5.5 s for `/settle` + up to 2.5 s asking the chain after an unclear settle; answered at once if `/settle` never reached the facilitator | measured settle max was 1.13 s (laptop, n=10); to be re-measured from GKE. Envoy Gateway has one `messageTimeout` for both phases |

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
never reached the facilitator, so the outcome is known (not settled), yet the filter polls
the chain for 2.5 s and calls it `settlement_unknown`. Harmless; it could answer at once.

**Price table, found while setting up the drills:** a filter started with
`-networks=testnet` rejects the whole price table when one entry is on a network it does
not serve (`httpbin-base`), so a new pod never becomes ready. The old pods kept serving, so
nothing broke, but one stray policy can block every rollout of the filter.

**Not run:** cancelAuthorization mid-request (needs gas in the payer wallet; the chain
read treats a cancelled nonce as used), a kill that lands mid-settle, and a facilitator
slower than 2 s (the case the 6 s settle budget fixes). The last two need a facilitator
the drill can slow down: `-facilitator-url` overrides one endpoint and is refused with two
networks, so it cannot point only Radius testnet at a delaying proxy.

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

## Sources

- [Radius docs](https://docs.radiustech.xyz/llms-full.txt): `eth_sendRawTransactionSync` error codes (4 timed out, accepted; 5 not admitted; 6 nonce gap), the Turnstile
- [EIP-155](https://eips.ethereum.org/EIPS/eip-155), [EIP-3009](https://eips.ethereum.org/EIPS/eip-3009)
- [Envoy ext_proc API](https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/filters/http/ext_proc/v3/ext_proc.proto)
- [Envoy Gateway `ExtProc` types](https://github.com/envoyproxy/gateway/blob/main/api/v1alpha1/ext_proc_types.go)
- [x402 exact EVM scheme](https://github.com/coinbase/x402/blob/main/specs/schemes/exact/scheme_exact_evm.md)
- [Radius docs](https://docs.radiustech.xyz/llms-full.txt): instant finality, `eth_sendRawTransactionSync`, facilitator API, SBC transfer methods, self-verification checks
- [Envoy JWT authn filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/jwt_authn_filter)
