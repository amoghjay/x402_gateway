# Roadmap: payment enforcement as data-plane infrastructure

> **The claim this roadmap exists to measure.** An L7 proxy will not hold a connection
> open for a settlement rail, and when it gives up, its default is to serve the response
> unpaid. Payment enforcement in a data plane must therefore be reserve-then-capture —
> and the deliverable is what that costs to build, run, and get wrong. The finding
> generalises past x402 to any settlement rail slower than a few hundred milliseconds.

**Status.** Phase 0 (baseline hardening) is complete and merged at `845d52f`: 41 tracked
files, 29 pytest + 9 forge tests, structured logging, Prometheus metrics, CI, container
build. Next up: Phase 1.

## Prior art, stated up front

The two findings left open in [REPORT.md](REPORT.md) §10 correspond to flaw classes
independently documented in [arXiv 2605.30998](https://arxiv.org/abs/2605.30998)
(*Free-Riding in the AI Economy: Demystifying Logic Flaws in x402-Enabled Payment
Systems*, May 2026) — §10.4 maps onto its *allowance overdraft*, §10.5 onto its
*duplicate-settlement race*. The mitigation that paper recommends, two-phase locking
with escrow contracts, is this repo's Path B, arrived at independently. So the
contribution here is not discovery: the flaw classes are documented and the mitigation
is prescribed, but **nobody has implemented that mitigation inside a production data
plane and measured what it costs to operate.** That measurement is the deliverable.

Cloudflare and AWS both shipped x402 enforcement at their edges in mid-2026.
Kubernetes has no equivalent. Phases 2–3 build one, as a Gateway API policy —
while noting honestly that x402 settlement volume is down sharply year-to-date;
the bet being made is the same infrastructure bet those vendors are making, not a
market-size claim.

## Phase 1 — Escrow v3 as an `auth-capture` implementation

The x402 Foundation has since published
[`auth-capture`](https://github.com/x402-foundation/x402/tree/main/specs/schemes/auth-capture)
as a standard scheme — it is `reserve/capture/release` under spec names, so v3
implements the published scheme rather than a parallel private API:

- `authorize` / `capture` / `charge` / `void` / `reclaim` / `refund` per
  `scheme_auth_capture_evm.md`, with the three ordered deadlines
  (`preApprovalExpiry <= authorizationExpiry <= refundExpiry`) enforced on-chain.
- `InferenceEscrow`'s deposit-once model becomes a **custom `tokenCollector`** drawing
  from an internal balance — the spec's intended extension point, not a divergence.
  An EIP-3009 or Permit2 collector ships alongside it so at least one path is stock.
- Single-shot `charge()` (`autoCapture`) stays deployed as the racy control: the A/B
  in Phase 3 is two conformant modes of one published scheme, not two private designs.
- **A conformance table is itself a deliverable** — operation by operation,
  implemented / partial / declared out of scope (fees are out of scope and say so).
- Tier A first: invariant, reentrancy, multi-payer, and cross-contract-replay tests
  against the current contract, so a baseline exists before anything is redeployed.

## Phase 1.5 — x402 V2 transport

V2 moved payment data into headers (`PAYMENT-REQUIRED` / `PAYMENT-SIGNATURE` /
`PAYMENT-RESPONSE`). The gateway dual-stacks: V2 headers served and accepted, the V1
body kept working until the existing demos pass and are updated deliberately. The
homegrown `extra.assetTransferMethod` discriminator is replaced by the standard
`scheme` field. Beyond hygiene, header-borne payments are what let a proxy filter
decide in `request_headers` without ever buffering a request body.

## Phase 2 — The Envoy `ext_proc` filter

The instrument that produces the headline measurement. `ext_authz` is a pre-filter
and physically cannot settle after a response exists; `ext_proc` holds a bidirectional
gRPC stream per request, hooks both `request_headers` and `response_body`, and can
return an `ImmediateResponse` (the 402).

Envoy's own semantics dictate the design: `ext_proc.message_timeout` defaults to
**200ms**, on expiry Envoy returns 504 — unless `failure_mode_allow: true`, in which
case **the response is released unpaid**. An on-chain settlement is seconds. Open
leaks revenue on every slow settlement; Closed fails a buyer whose payment may have
partially landed. The only shape that fits the data plane's contract:

1. `request_headers`: no payment → immediate 402; payment → validate free, **reserve
   in Redis** (milliseconds), continue.
2. `response_body`: release immediately.
3. Async worker: drive the contract's `authorize`/`capture`.

The same wall exists on the protocol side: the spec's `authorize()` is itself an
on-chain call, so the standard scheme applied naively does not fit the 200ms budget
either. The inline reservation is not a shortcut around the spec — it is the price of
honouring it at ingress. Both capture modes get built; the synchronous one exists to
be shown failing, exactly like single-shot `charge()`.

## Phase 3 — The platform layer, and the measurements

**`PaymentPolicy` CRD + controller** (Gateway API direct-attached policy, GEP-2648):
`kubectl apply` a six-line policy at an HTTPRoute fronting unmodified `httpbin` and
the route is monetized; `kubectl delete` and it is free again, backend untouched.

**Correctness SLIs** — a payment filter's SLIs are correctness, not availability:

| SLI | Target |
|---|---|
| unpaid responses released / responses released | 0 |
| settlements captured / paid responses released | 1.0 |
| paid but undelivered | 0 |
| reservations expired unreleased | 0 |
| filter added latency p99 | within budget |

**Measurement campaign** (predictions committed before results):

1. The `message_timeout` sweep — sync capture against 200ms / 1s / 5s / 30s, recording
   when Envoy 504s, when it serves unpaid, and what async capture costs instead.
2. The overdraft measured correctly: escrow funded for exactly *k* calls, *N* fired
   concurrently, amplification predicted at `A = N/k`. The independent variable is the
   balance, not the concurrency.
3. The A/B: the same burst against `charge()` and against `authorize()`+`capture()`,
   with the gas delta reported as the price of the mitigation the literature recommends.
4. The replay finding at multi-replica, then against Redis: kill the store and the
   original bug reproduces one hop over; fail-open serves every replay, fail-closed
   drops every legitimate request. The dependency is the cost of the fix — measured,
   not asserted.
5. Pod deletion under load: no reservation orphaned past its TTL.

## Phase 4 — Writeup

REPORT.md grows a prior-work section (before the findings, not after), the corrected
§10.4 (the shipped runtime's single blocking worker was an accidental mitigation, not
a control), the measured §10.5, and three new sections: the convergence of the
contract's escrow and the filter's reservation on one algorithm with different trust
bases; where payment enforcement should live (app / ingress / edge / WAF) argued from
measurements; and the data plane's contract as the general result.

## Explicitly not doing

No GPU inference backends, no agent-protocol extensions (AP2/A2A), no rewrite of the
audited signing path in Go — EIP-712 signing and settlement stay in Python, because
rewriting working crypto under a deadline is how the §10 class of bug comes back.
