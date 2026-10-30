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
| `Sync` (opt-in) | 2 s | 1.5 s | measured settle max was 1.13 s |

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

Both models stay available: settle-before-release is `captureMode: Sync`, with its own 2 s
budget, and Phase 6 measures the two side by side.

## Risks we accept

- **No simulation on the request path.** A paused token or a blocklisted payer shows up as a failed settlement. The loss limit above bounds it.
- **A balance cached for a few seconds can be stale.** The in-flight sum covers concurrent requests through this gateway. Spending elsewhere during the TTL is the remaining gap.
- **Caches in front of the gateway.** A shared cache could resell a paid response for free. Paid responses get `Cache-Control: private, no-store`, overriding the upstream's value.

## Measured on GKE: the timeout drill (2026-10-28)

Envoy Gateway v1.9.2, the no-op filter with a `-delay` flag, one `HTTPRoute` behind an
`EnvoyExtensionPolicy`. Durations are from Envoy's access log, not the client.

**What Envoy Gateway configures.** It leaves `message_timeout`, `failure_mode_allow` and
`status_on_error` at Envoy's defaults (200 ms, `false`, unset) and sets
`grpc_service.timeout: 10s`. The 200 ms per-message timeout is the one that fires.

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

## Sources

- [Envoy ext_proc API](https://www.envoyproxy.io/docs/envoy/latest/api-v3/extensions/filters/http/ext_proc/v3/ext_proc.proto)
- [Envoy Gateway `ExtProc` types](https://github.com/envoyproxy/gateway/blob/main/api/v1alpha1/ext_proc_types.go)
- [x402 exact EVM scheme](https://github.com/coinbase/x402/blob/main/specs/schemes/exact/scheme_exact_evm.md)
- [Radius docs](https://docs.radiustech.xyz/llms-full.txt): instant finality, `eth_sendRawTransactionSync`, facilitator API, SBC transfer methods, self-verification checks
- [Envoy JWT authn filter](https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/jwt_authn_filter)
