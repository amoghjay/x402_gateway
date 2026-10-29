# `auth-capture`: how the scheme works, and where it would fit this gateway

Status: exploration, nothing scheduled. Contracts and facilitators were checked live on
2026-10-27; latencies quoted are the 2026-10-16 laptop measurements from
[request-path-budget.md](request-path-budget.md).

## Summary

`auth-capture` is the x402 scheme for payments whose final amount is not known when the
buyer signs, or which may need to be undone. The buyer authorizes a ceiling once; the seller
later captures up to it, voids the rest, or refunds. On EVM it is built on Base's
[commerce-payments](https://github.com/base/commerce-payments) escrow.

For this gateway the short version is: the contracts it needs are **not deployed on Radius**,
**no facilitator on Radius advertises it**, and its headline property, reversibility, is mostly
covered already by settling after the response. It becomes worth building when a route needs
money to move *before* the response or *after* the request ends. For metered pricing, `upto`
is the cheaper first step, and its contract is already on Radius.

## What the scheme does

**One client signature, for a ceiling.** The buyer signs an EIP-3009 authorization (default) or
a Permit2 permit for `requirements.amount`. Unlike `exact`, the recipient of that signature is a
**token collector contract**, not `payTo`. The signature's nonce is a hash of the whole
`PaymentInfo` struct (receiver, token, maximum, the three expiries, fee bounds, operator, salt),
so the signature commits to every term. The facilitator rebuilds `PaymentInfo` from the 402
requirements plus the payload and checks that the nonce matches; it keeps no state of its own.

**Two flows, chosen by `extra.paymentFlow`.**

| Flow | Ordering | Operations |
|---|---|---|
| `escrow` (default) | settle → resource → settle | `authorize` moves the funds into the escrow **before** the resource runs. Afterwards `capture` (any amount up to the hold, repeatable), `void` (release the remainder), and later `refund` (up to the captured amount, until `refundDeadline`). If nobody captures before `captureDeadline`, the buyer calls `reclaim` and gets the hold back. |
| `authorization` | verify → resource → settle | One `charge` after the resource, for any amount up to the ceiling. No hold, so no capture, void or reclaim. `refund` still applies. |

**Who signs what.** `authorize` needs only the buyer's signature. `charge`, `capture`, `void` and
`refund` each need an EIP-712 signature from `extra.receiverAuthorizer`, which is either a key the
resource server holds, or an address the facilitator advertises and signs with on the server's
behalf. In the delegated case the facilitator must authenticate the server out of band (an API
credential or mTLS) and binds the first caller identity to the payment.

**The operator.** `extra.captureAuthorizer` becomes `PaymentInfo.operator`, and the escrow accepts
`capture`, `void` and `refund` only from that `msg.sender`. With `operatorType: "delegated"` the
operator is the facilitator's own submitter; with `"custom"` it is a contract, the facilitator
relays only the collect, and the lifecycle runs out of band. A third type, `"policy"`, is
reserved for an operator that verifies the authorizer signature on chain and consults read-only
policy hooks (cooldowns, freeze windows, arbitration).

**Three expiries, enforced on chain.** `preApprovalExpiry` (the signature's `validBefore`, which is
now plus `maxTimeoutSeconds`) ≤ `captureDeadline` ≤ `refundDeadline`. The accept window must
fit inside the capture window.

**Fees and refund money.** The buyer signs `minFeeBps` and `maxFeeBps`; the fee is taken at
capture or charge. Refunds are pulled from the **operator's** balance, so a delegated facilitator
refuses refunds unless it has a funding agreement with the seller.

**Sync or deferred capture.** With `captureMode: "sync"` the in-request settle after the resource is
a capture, a partial capture plus void, or a void, and nothing has to be stored. With
`"deferred"` the server stores `paymentInfo` (and `saltNonce` when the salt is bound) and captures
later. A route that relays no lifecycle through the facilitator must be `deferred`, or the hold
sits until the buyer reclaims it.

**Replay protection without a nonce store.** Each capture or refund signature binds the escrow
balances it expects to find, `(capturableAmount, refundableAmount)`. That pair never recurs once
either balance moves, so every signature is single-use.

## Checked on 2026-10-27

Read-only `eth_getCode` and `GET /supported` calls. Byte counts are the deployed code size.

| What | Radius testnet `eip155:72344` | Radius mainnet `eip155:723487` | Base mainnet `eip155:8453` |
|---|---|---|---|
| `AuthCaptureEscrow` v1.1 `0xf96815976523E00e65Be8f34cA5e64b4f41EB19c` | none | none | 10978 bytes |
| `AuthCaptureEscrow` v1.0 `0xBdEA0D1bcC5966192B070Fdf62aB4EF5b4420cff` | none | none | 11053 bytes |
| EIP-3009 token collector v1.1 `0x8612dfdc421f80336cd14E8EF9cb1E765dB5ab88` | none | none | 3982 bytes |
| Permit2 token collector v1.1 `0xD69831Aed5bfe262067ec4c751f4F830EcdD446e` | none | none | 4049 bytes |
| `upto` Permit2 proxy `0x4020A4f3b7b90ccA423B9fabCc0CE57C6C240002` | 3142 bytes | 3142 bytes | 3142 bytes |
| Permit2 `0x000000000022D473030F116dDEE9F6B43aC78BA3` | 9152 bytes | 9152 bytes | 9152 bytes |
| Deterministic deployers (Arachnid `0x4e59…956C`, Safe singleton factory, CreateX), Multicall3 | present | present | present |

| Facilitator | Advertises |
|---|---|
| `facilitator.testnet.radiustech.xyz`, `facilitator.radiustech.xyz` | `exact` with `eip3009` and `permit2`; extension `eip2612GasSponsoring`. No `upto`, no `auth-capture`. |
| `x402.org/facilitator` | `exact`, `upto` and `batch-settlement` on Base Sepolia, `exact` on other networks. No `auth-capture`. |
| Coinbase CDP facilitator | `/supported` needs an API key; not checked. |

Other facts:

- The x402 repo ships reference code for all three roles in Go and TypeScript
  (`go/mechanisms/evm/auth-capture/{client,server,facilitator}`, `examples/go/facilitator/auth-capture`,
  `examples/go/servers/auth-capture`). A self-hosted facilitator with `auth-capture` is runnable on
  Base today.
- commerce-payments is audited (Coinbase Protocol Security and Spearbit; the v1.1 fee change was
  reviewed on 2026-07-22). It is deployed with CREATE2, and the deployers it would need exist on
  Radius, so the canonical addresses could be reproduced there. Whether to do that is Radius's
  decision.

## Where it would help a gateway

The project frames designs as points on a latency/risk curve. `exact` settled after the response
sits at roughly 2 ms on the request path with a bounded window of unsettled revenue; settle-before-
release sits at about 1.1 s with zero unpaid responses. `auth-capture` in the `escrow` flow is a
third point: the seller has the money in escrow before serving (like `upfront`), and the buyer
keeps recourse (void, refund, reclaim). The price is two transactions per request instead of one,
and the first of them runs before the backend.

Cases where that is worth paying for:

- **Money moves before the response.** A high-value route that settles up front (a report, a
  dataset) has no way to give the money back under `exact`. Under `auth-capture` a failed response
  voids the hold, and a later dispute can refund.
- **The work finishes after the request.** A backend that answers 202 and completes later. The hold
  is placed at request time, captured when the job completes, voided if it fails. This needs
  `captureMode: "deferred"` and durable payment state.
- **The amount is known only after the response, and refunds matter.** The `ext_proc` filter sees
  the response, so it can compute the real cost in the response phase. The `authorization` flow
  charges that amount in one transaction and keeps `refund` available.

What it does **not** add for this design:

- **No-charge-on-5xx.** Settling after the response already gives that: the filter drops the
  Redis reservation and nothing is submitted. Void and refund are redundant for request-scoped
  routes under the current plan.
- **A faster request path.** `authorize` is an on-chain transaction before the backend runs.
  Through the Radius facilitator that is the measured `/settle` cost, 815 ms median on 2026-10-16
  from a laptop; self-submitted on Radius a transfer took 112 ms median. Neither fits the 150 ms
  Async filter deadline. It fits only the Sync route shape with its 2 s Envoy timeout. The August
  2026 plan reached the same conclusion from the protocol side.
- **Session billing on the wire.** The spec lists "periodic captures against one authorization" as
  a use case, but x402 defines no header for a later request to reference an existing hold. A
  gateway could implement it, but no standard client would send it.

**`upto` for metering first.** `upto` settles a single Permit2 transfer for the actual amount, up
to the signed maximum, with the amount carried in the settle-time `requirements.amount`. It needs
no key on our side and no escrow, and its proxy is already on both Radius networks. What is
missing is a facilitator on Radius that advertises it. `auth-capture`'s `authorization` flow
covers the same case with refundability, at the cost of a receiver-authorizer key or delegation.

## What building it would cost here

- **A signing key or a delegation.** Every lifecycle operation needs the receiver authorizer's
  EIP-712 signature. Either the cluster holds that key, which ends the "no private keys" property,
  or the facilitator signs on our behalf and authenticates us by API credential, which moves the
  trust to the facilitator.
- **Durable payment state.** Deferred capture and any refund need `paymentInfo` and `saltNonce`
  kept until `refundDeadline`. The settlement stream in Redis already keeps the signed
  authorization for `exact`, so this is additive rather than new.
- **A `scheme` per route.** The price table and the `PaymentPolicy` CRD carry no `scheme` field
  today; `exact` is implicit. Adding one is cheapest before the schema is versioned in M3.
- **A settler that speaks lifecycle payloads.** Today's settler sends one collect to `/settle`.
  `capture`, `void` and `refund` are separate `/settle` calls with `payload.type` and an
  authorizer signature.

## Preconditions

All three before any of this leaves the backlog:

1. The commerce-payments contracts are deployed on Radius testnet (and later mainnet).
2. A facilitator advertises `auth-capture` on `eip155:72344`. That is either Radius adding it, or
   this project running the reference Go facilitator in-cluster, which puts a submitter key in
   the cluster.
3. A route whose economics need it: up-front settlement with refund, or delayed delivery.

Until then this note is positioning, not planned work.

## Sources

- [`auth-capture` scheme](https://github.com/x402-foundation/x402/blob/main/specs/schemes/auth-capture/scheme_auth_capture.md) and its [EVM binding](https://github.com/x402-foundation/x402/blob/main/specs/schemes/auth-capture/scheme_auth_capture_evm.md), v1.1, 2026-08-26
- [`upto` scheme](https://github.com/x402-foundation/x402/blob/main/specs/schemes/upto/scheme_upto.md) and its [EVM binding](https://github.com/x402-foundation/x402/blob/main/specs/schemes/upto/scheme_upto_evm.md)
- [x402 payment schemes overview](https://github.com/x402-foundation/x402/blob/main/docs/schemes/overview.mdx): payment flows table
- [Go `auth-capture` mechanism README](https://github.com/x402-foundation/x402/blob/main/go/mechanisms/evm/auth-capture/README.md)
- [base/commerce-payments](https://github.com/base/commerce-payments): deployment addresses and audits
- [request-path-budget.md](request-path-budget.md): the latency measurements quoted above
