# Pay-Per-Prompt — Final Project Report

**INFO7500 (Cryptocurrency & Smart Contracts) — Final Project**
**Network**: Radius Testnet (chain ID `72344`)

---

## 1. One-line pitch

An x402-metered gateway that sits in front of an LLM inference endpoint. A caller
hits `POST /infer`, gets an HTTP `402 Payment Required`, pays via one of two
settlement mechanisms, and only then receives the model's completion.

## 2. Motivation

x402 turns HTTP's long-unused `402` status code into a real payment protocol: the
server names its price, the client proves payment, the server serves the resource.
This project builds that pattern twice, on purpose:

1. **Permit2 + a third-party facilitator** — the standard, production-recommended
   way to accept x402 payments on Radius. Fast to stand up, battle-tested, but the
   gateway never contains any original settlement logic — it's a client of existing
   infrastructure (Uniswap's Permit2, Radius's `x402ExactPermit2Proxy`, and the
   facilitator itself).
2. **InferenceEscrow** — a Solidity contract written, tested, and deployed for this
   project specifically to close that gap: to be the thing that actually verifies
   signatures and moves money on-chain, rather than calling something else that does.

Building both, side by side, on the same product, is the point: one demonstrates
integrating with existing x402 infrastructure correctly; the other demonstrates
understanding what that infrastructure does well enough to reimplement it.

## 3. Architecture

```
                         POST /infer {prompt}
                                |
                                v
                    +-----------------------+
                    |  gateway.py (FastAPI)  |
                    |  no X-PAYMENT header?  |----> 402 + accepts[] (BOTH schemes)
                    +-----------------------+
                                |
                     X-PAYMENT header present
                                |
                 which accepted.extra.assetTransferMethod?
                    /                              \
            "permit2"                      "inference-escrow"
                |                                    |
                v                                    v
     +----------------------+           +---------------------------+
     | facilitator.testnet  |           | InferenceEscrow.settle()   |
     | .radiustech.xyz      |           | (gateway submits, on-chain |
     | /verify -> /settle   |           |  ECDSA.recover + nonce +   |
     | (atomic on-chain via |           |  deadline check, itself)   |
     | x402ExactPermit2Proxy|           +---------------------------+
     +----------------------+
                |                                    |
        settlement tx hash                    settlement tx hash
                |                                    |
                +------------------+-----------------+
                                   v
                    replay already served this tx hash?
                    (SEEN_SETTLEMENTS set / on-chain revert)
                                   |
                            no -> call Groq -> 200 + completion
                            yes -> 409
```

## 4. Path A — Permit2 + Radius facilitator

- Client signs an **EIP-712 `PermitWitnessTransferFrom`** (Permit2's own type, extended
  with x402's `Witness{to, validAfter}`) — no gas, no on-chain transaction from the
  caller at all.
- Gateway forwards it to the facilitator's `/verify` (validity is signaled by
  `isValid` in the **response body**, not the HTTP status — a bad signature still
  returns `200`), then `/settle`, which submits **one atomic transaction** calling
  `x402ExactPermit2Proxy.settle(permit, owner, witness, signature)`.
- The `Witness` extension is what makes this safe: it binds the recipient
  (`witness.to`) **into the signed hash itself**, so the facilitator — which submits
  the transaction — can never redirect funds to a different address than the payer
  authorized.
- Permit2's domain deliberately omits `version` (Uniswap's own choice); its nonce is
  a random 256-bit value (unordered bitmap), so no on-chain read is needed before
  signing.
- **Replay**: the facilitator is idempotent — a replayed payload returns the *same*
  cached tx hash, `success: true`, not an error. The actual replay guard is the
  gateway's own in-memory `SEEN_SETTLEMENTS` set (`gateway.py`), which is the
  in-memory stand-in for what a real deployment would do with a
  `settlement_tx_hash UNIQUE` database constraint.

## 5. Path B — InferenceEscrow

**Contract**: `contracts/src/InferenceEscrow.sol`, deployed to Radius testnet at
`0xe629C40907f0cB5f93BaeD5F0a97f6E94bD44d7E`.

```solidity
struct Authorization { uint256 amount; uint256 nonce; uint256 deadline; }

IERC20 public immutable token;      // SBC
address public immutable provider; // gateway's payee, fixed at deploy — no
                                    // witness.to needed: single-tenant, so there's
                                    // no "redirect funds" surface to close by signing it

mapping(address => uint256) public balances;   // payer -> unspent deposit
mapping(address => uint256) public nextNonce;   // payer -> next expected nonce
```

- `deposit(amount)` — payer moves SBC in once, pays their own gas. Mirrors Permit2's
  one-time `approve(Permit2, MAX)` bootstrap.
- `settle(auth, signature)` — recovers the payer via `ECDSA.recover` over an
  `_hashTypedDataV4` digest (OpenZeppelin's `EIP712` base contract). **`msg.sender`
  here is the gateway operator, not the payer** — the signature is the proof of
  authorization, not who submitted the transaction. Checks
  `nonce == nextNonce[payer]` (strictly incrementing — a deliberate simplification
  vs. Permit2's bitmap, valid because a session's prompts are naturally sequential),
  checks `block.timestamp <= deadline`, debits, pays `provider`.
- `withdraw()` — refunds unspent balance. `nonReentrant` + checks-effects-interactions
  (balance zeroed *before* the external transfer) — the one function sending value
  to an arbitrary caller-controlled address, unlike `settle()`'s fixed `provider`.
- **Replay**: the contract's own `nextNonce` check makes a replayed authorization
  **revert on-chain** (`"invalid nonce"`) — no application-level bookkeeping needed
  for this path at all.

**Tested**: 5/5 Foundry tests (`contracts/test/InferenceEscrow.t.sol`), using
`vm.sign()` to produce real signatures inside the test suite — valid settle,
replayed nonce reverts, expired deadline reverts, over-authorization reverts,
`withdraw()` returns the exact remainder.

**Why deposit-once instead of sign-per-request-against-a-facilitator**: Path A signs
and settles fresh on *every single prompt*. For a product literally called
Pay-Per-Prompt, where one session might fire dozens of requests, that's the wrong
shape. `InferenceEscrow` lets a caller deposit once and draw down repeatedly —
demonstrated directly in the demo below (one deposit transaction covers three
separate metered prompts).

## 6. The three distinct wallets — made real, not just asserted

| Role | Address | Job |
|---|---|---|
| Payer | `0xfd4dc70f4b9c4055aC58c6a642aE2bc7be3B032A` | Signs authorizations. Never submits a transaction for settlement, never pays gas for it. |
| Gateway operator | `0xE5377D7716EEC361Be8FA1aEE3BDF92996614C00` | Submits `InferenceEscrow.settle()`. Pays its own gas (SBC, auto-converted to RUSD via Turnstile). Genuinely different address from the payer — proof that `msg.sender != payer` isn't just a code comment. |
| Provider | `0xbD5fdCde255Abb883cB0C3137037cAef28ed10ac` | Receives every payment, from both paths. |

In Path A, the facilitator plays the "gateway operator" role invisibly (sponsored,
off-chain to us). In Path B, we own that role explicitly — which is also why it costs
us something concrete: a funded wallet and gas management, the exact responsibility
a facilitator normally hides.

## 7. Live demo script

```bash
cd gateway
uvicorn gateway:app --port 8000 &
python client.py
```

Walk through, live:

1. **Show the `402` has two options**:
   ```bash
   curl -i -X POST localhost:8000/infer -H 'content-type: application/json' -d '{"prompt":"hello"}'
   ```
   Point out `accepts[]` has two entries — `extra.assetTransferMethod` is `"permit2"`
   in one, `"inference-escrow"` in the other. This is x402's extensibility mechanism
   working as designed: one endpoint, multiple accepted payment methods, client picks.

2. **Run `client.py`** and narrate as it goes:
   - Demo 1 fires 3 different prompts through Permit2 + facilitator. Point out each
     gets its own real settlement tx — click one open on the explorer.
   - Demo 1 replays the last payment — `409`, and explain *why*: the facilitator
     already returned success for that exact payload once; a second `/settle` call
     returns the same cached hash rather than erroring, so the gateway's own
     `SEEN_SETTLEMENTS` set is what actually produces the `409`.
   - Demo 2 fires 3 different prompts through `InferenceEscrow`, from **one deposit**.
     Point out the nonce printed for each call — `6, 7, 8` — genuinely incrementing
     across real, distinct requests, not just "0 works once."
   - Demo 2 replays the last payment — `409` again, but for a structurally different
     reason: the contract's own `require(nonce == nextNonce[payer])` reverts on-chain.
   - Point out the balance deltas: provider's balance goes up by exactly `3 × price`
     in both demos; the operator wallet's balance is a completely separate number
     from the charge, because gas and the actual payment are two unrelated flows.

3. **Open two explorer links side by side** — one settlement from each path — to
   show both are real, verifiable, independent transactions, not printed strings.

## 8. Real on-chain evidence (from actual runs, not fabricated)

| Item | Value |
|---|---|
| `InferenceEscrow` deployment tx | `0xefb4abe0fefc48b65f205a40ffa37cbf1753f42ed138ada7f851504d9970210f` |
| `InferenceEscrow` address | `0xe629C40907f0cB5f93BaeD5F0a97f6E94bD44d7E` |
| Sample Permit2 settlement | `0x2d5be6c8c5af6c2164e04613f271aa9f923f8271a2387774090681f54fe112de` |
| Sample InferenceEscrow settlement | `0x9211a65f4e3959f899ac34d111423fed2b458a8c7521a96ce2fc1f2ca199809a` |
| Facilitator | `https://facilitator.testnet.radiustech.xyz` |
| Canonical Permit2 | `0x000000000022D473030F116dDEE9F6B43aC78BA3` |
| `x402ExactPermit2Proxy` | `0x402085c248EeA27D92E8b30b2C58ed07f9E20001` |
| SBC token | `0x33ad9e4BD16B69B5BFdED37D8B5D9fF9aba014Fb` |

Every tx hash above resolves at `https://testnet.radiustech.xyz/tx/<hash>`.

## 9. Security properties, and why each exists

- **`isValid` in the body, not the status code** (Path A) — treating a `200` as
  automatically valid would let a malformed/forged payload slip through if the
  gateway only checked HTTP status.
- **Witness binds the recipient into the signature** (Path A) — without it, a
  relayer (the facilitator) could redirect funds to any address, since the
  recipient would otherwise just be a runtime argument, not something the payer
  actually authorized.
- **`provider` is `immutable`, not signed** (Path B) — the same problem, solved
  differently: because this contract only ever pays one address, there's no
  "redirect funds" attack surface to close by signing a recipient field at all.
- **Strictly incrementing nonce, not a bitmap** (Path B) — a deliberate,
  documented simplification: legitimate requests in a session are sequential, so
  ordering is a feature, not a limitation, and it's cheaper to check.
- **`deadline` bounds signature validity** (both paths) — an unbounded signature
  would remain replayable (subject to nonce/idempotency) indefinitely.
- **CEI + `ReentrancyGuard` on `withdraw()`, not `settle()`** (Path B) —
  `withdraw()` sends value to an arbitrary caller-controlled address;
  `settle()` always pays the fixed `provider`, so its reentrancy surface is
  much smaller. The guard is placed where the actual risk is, not everywhere
  reflexively.
- **Replay protection lives at different layers on purpose** — Path A's facilitator
  is idempotent (by design, to protect payers from double-charges on retried HTTP
  requests), so the gateway's own bookkeeping is what's actually observable. Path
  B's contract enforces it itself, on-chain, because nothing else does.

## 10. A real vulnerability we found in our own gateway, and fixed

Everything in §9 is about defending the *payer*. Auditing the code from the
*server's* side turned up a live underpayment hole that both paths shared, which
we exploited on-chain before fixing. It is the most instructive finding in the
project, so it is documented rather than quietly patched.

### The bug

The gateway advertised a price of `PRICE_BASE_UNITS` (1000) in its 402 response
and then never checked that the payment matched it. Grepping the pre-fix code,
`PRICE_BASE_UNITS` appeared in exactly three places, all of them *advertisement*
— it was never once on either side of a comparison.

Nothing downstream could catch this for us:

- **The signature can't.** It covers `amount`, so the amount cannot be tampered
  with in transit — but that only proves *the payer really did agree to this
  number*. Authenticity is not correctness. A customer can genuinely sign a
  cheque for $0.01; catching that is the cashier's job, not the signature's.
- **The contract can't.** `require(balances[payer] >= auth.amount)` is an *upper*
  bound (don't overdraw), never a lower one. `InferenceEscrow` has no `price`
  field at all — price is an HTTP-layer concept the contract has no knowledge of.
- **The facilitator can't, the way we called it.** This is the subtle half.
  `_build_facilitator_request` built `paymentRequirements` by calling
  `build_payment_requirements(amount)` where `amount` had been read out of the
  *client's own authorization*. So the facilitator was asked "does this payload
  match these requirements?" where both sides derived from the attacker's number.
  Self-consistent, therefore valid. **The validation was circular.**

### Exploitation (live, on Radius testnet)

A single request carrying `accepted.amount = "1000"` (the gateway's own
advertised price, untouched) alongside `payload.amount = "1"` — a 1000×
underpayment, with the contradiction sitting inside one envelope:

| Path | Result | Settlement tx |
| --- | --- | --- |
| Permit2 + facilitator | `HTTP 200` + real completion, provider received **+1** | [`0x314a6bce…`](https://testnet.radiustech.xyz/tx/0x314a6bceb6b610037bf5ca0dcfd81e76a19f3066c13499f47f0546235dabc4af) |
| InferenceEscrow | `HTTP 200` + real completion, tab 12000 → **11999**, nonce consumed | [`0x11f155c1…`](https://testnet.radiustech.xyz/tx/0x11f155c153a1c19d926aa2142b105ce1a275eb9b34ce7c38c418d455e2bd4e0b) |

A control probe established that the Radius facilitator's own amount check was
**never broken**. Handed the same 1-unit signature with requirements stating the
honest 1000, it returned:

```json
{ "isValid": false, "invalidReason": "Payment amount 1 is less than required 1000" }
```

So this was not a missing facilitator feature. Our resource server *disarmed a
working defense* by feeding it requirements reconstructed from the attacker's
payload. Delegated validation launders untrusted input into a trusted-looking
verdict unless the requirements come from the delegator.

### The fix — two layers

1. **An explicit check in the gateway**, at the top of both handlers, before any
   verify or settle: `if int(signed_amount) < int(PRICE_BASE_UNITS)` → `402`.
   Path-independent, and the only thing protecting Path B, which has no
   facilitator. Rejecting *before* settlement means no gas is spent, no nonce is
   burned, and the payer's authorization remains spendable.
   Both operands are `int()`-cast deliberately: these arrive as strings, and
   `"9" >= "1000"` is `True` under string comparison, so a 9-unit payment would
   have passed a naive check.
2. **The `amount` parameter was deleted** from `_build_facilitator_request`,
   `verify_payment`, and `settle_payment` — not merely passed correctly. Passing
   `PRICE_BASE_UNITS` at the call site would fix today's bug while leaving the
   hole reachable by any future caller. Removing the parameter makes validating
   against caller-supplied requirements *structurally impossible*, and restores
   the facilitator's own check as an independent second layer.

`<` rather than `<=`: overpayment is permitted, since x402 treats the requirement
as a floor and the surplus goes to the provider.

### Related hardening

Malformed `X-PAYMENT` input (bad base64, bad JSON, missing keys, non-numeric
amounts) previously escaped as unhandled exceptions and surfaced as `500`s —
reporting "we broke" for what is really "you sent garbage." Since every byte of
that header is attacker-controlled, malformed envelopes are expected input, and
now return `400` with the specific parse failure.

### Verified after the fix

Both exploit probes return `402 underpayment` with **zero on-chain movement**
(provider delta 0, escrow nonce unchanged). The full honest demo still settles
exactly 3 × 1000 on each path, both replay guards still fire with `409`, escrow
nonces still advance 10 → 11 → 12, and all 5 Foundry tests still pass.

## 11. Limitations / honest scope notes

- `InferenceEscrow.settle()` is still called once per prompt (not batched) — the
  deposit model's advantage here is avoiding a facilitator round-trip and
  re-signing overhead per request, not reducing on-chain transaction count. Batched
  settlement (accumulate N charges, settle once) is a natural next step, not built.
- The gateway operator wallet is a single, unmonitored hot wallet in this demo — a
  production version would need nonce-collision handling under concurrent load
  (a documented Radius production gotcha) and key rotation/custody practices.
- Pricing is fixed per call (`PRICE_BASE_UNITS`); no tiered or per-token pricing.
- Single LLM provider (Groq, `llama-3.3-70b-versatile`), single model, no
  multi-provider routing.

## 12. What this demonstrates, in one paragraph

Path A shows correct integration with production x402 infrastructure: understanding
the wire protocol, the EIP-712 signing mechanics, and the specific operational
sharp edges (validity-in-body, idempotent replay, hardcoded spender) well enough to
build a working client against a live facilitator on the first attempt. Path B shows
the deeper claim: that the same mechanics — EIP-712 domain separation, signature
recovery, nonce-based replay protection, deadline bounding, and secure withdrawal
patterns — can be designed, implemented, tested, and deployed from scratch, for a
use case (repeated per-prompt billing) that the facilitator model doesn't fit as
naturally as it fits a one-shot purchase.
