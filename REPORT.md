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
      PHASE 1: validate, FREE                PHASE 1: validate, FREE
      POST /verify + SEEN_SIGNATURES         settle().call() simulation
                |                                    |
                +------------------+-----------------+
                                   |
                        valid? no -> 402 / 409
                                   |
                                  yes
                                   v
                    PHASE 2: call Groq  (BEFORE charging)
                                   |
                    failed? -> 502 {charged: false}, nothing settled
                                   |
                                 served
                                   v
                    PHASE 3: settle on-chain -> tx hash
                                   |
                                   v
                        200 + completion + X-PAYMENT-RESPONSE
```

**Phase ordering is deliberate** — validate for free, serve, *then* charge. Settling
before serving meant a provider outage still took the payer's money and returned
`200` with a placeholder (§10.3).

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
  cached tx hash, `success: true`, not an error. So the replay guard has to be the
  gateway's own, in two layers: `SEEN_SIGNATURES` rejects a duplicate payload
  *before* any inference is spent, and `SEEN_SETTLEMENTS` catches a re-signed
  duplicate nonce afterwards, since only the cached tx hash reveals that case. Both
  are in-memory stands-in for what a real deployment would express as a
  `settlement_tx_hash UNIQUE` database constraint.

## 5. Path B — InferenceEscrow

**Contract**: `contracts/src/InferenceEscrow.sol`, deployed to Radius testnet at
`0x76c03C8b763a0e2f3594bABfb71847e8a6d502D8`.

```solidity
struct Authorization { address settler; uint256 amount; uint256 nonce; uint256 deadline; }

IERC20 public immutable token;      // SBC
address public immutable provider; // gateway's payee, fixed at deploy — no
                                    // witness.to needed: single-tenant, so there's
                                    // no "redirect funds" surface to close by signing it

mapping(address => uint256) public balances;                     // payer -> unspent deposit
mapping(address => mapping(uint256 => bool)) public nonceUsed;   // payer -> spent nonces
```

- `deposit(amount)` — payer moves SBC in once, pays their own gas. Mirrors Permit2's
  one-time `approve(Permit2, MAX)` bootstrap. Credits the amount **actually received**
  (measured as a `balanceOf` delta) rather than the amount requested, so a
  fee-on-transfer token can't leave the contract owing more than it holds;
  `nonReentrant` covers tokens with transfer hooks.
- `settle(auth, signature)` — recovers the payer via `ECDSA.recover` over an
  `_hashTypedDataV4` digest (OpenZeppelin's `EIP712` base contract). **`msg.sender`
  here is the gateway operator, not the payer** — the signature is the proof of
  authorization, not who submitted the transaction. But it must be the *specific*
  operator named in `auth.settler` (§10.2), checks the nonce is unspent, checks
  `block.timestamp <= deadline`, debits, pays `provider`.
- `withdraw()` — refunds unspent balance. `nonReentrant` + checks-effects-interactions
  (balance zeroed *before* the external transfer) — the one function sending value
  to an arbitrary caller-controlled address, unlike `settle()`'s fixed `provider`.
- **Replay**: the contract's own `nonceUsed` check makes a replayed authorization
  **revert on-chain** (`"nonce already used"`) — no application-level bookkeeping
  needed for this path at all.

**Why unordered nonces rather than a counter**: nonces are client-chosen random
256-bit values, and the contract only asks *"has this one been spent?"* — never
*"is this the next one?"* A strictly-incrementing counter is cheaper but serialises
the payer: two prompts signed before the first settles would claim the same nonce,
and the second would revert as a replay despite being legitimate — the gateway
would report `409 replay detected` for honest traffic. Permit2 solves this with a
packed bitmap (256 nonces per storage slot); we use a plain mapping, paying roughly
15k extra gas per settlement to avoid ~40 lines of bit manipulation. It also removes
an RPC round-trip: the client no longer reads any on-chain counter before signing.

**Tested**: 9/9 Foundry tests (`contracts/test/InferenceEscrow.t.sol`), using
`vm.sign()` to produce real signatures inside the test suite — valid settle,
replayed nonce reverts, expired deadline reverts, over-authorization reverts,
unauthorized settler reverts *without consuming the nonce*, a rewritten `settler`
field fails recovery, arbitrary out-of-order nonces all settle, `deposit()` credits
only what a fee-on-transfer token actually delivered, and `withdraw()` returns the
exact remainder.

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
     Point out the three unrelated random nonces printed — real, distinct requests,
     not just "0 works once."
   - Demo 2 replays the last payment — `409` again, but for a structurally different
     reason: the contract's own `require(!nonceUsed[payer][nonce])` reverts on-chain.
   - Point out the balance deltas: provider's balance goes up by exactly `3 × price`
     in both demos; the operator wallet's balance is a completely separate number
     from the charge, because gas and the actual payment are two unrelated flows.

3. **Open two explorer links side by side** — one settlement from each path — to
   show both are real, verifiable, independent transactions, not printed strings.

4. **Run `security_probes.py`** — the adversarial half, and the strongest part of the
   demo. Four attacks that all used to succeed against this code:
   - Underpayment on **both** paths: sign 1 base unit for a 1000-unit product. Point
     out that the envelope carries our own advertised `1000` in `accepted` *and* a
     signed `1` in the payload — the contradiction sits inside a single request.
     Now `402`, and the provider's balance does not move.
   - Unauthorized settler: submit an escrow authorization from the **payer's own
     wallet**. Reverts `unauthorized settler`, and critically the nonce is *not*
     consumed — then the named settler settles the same authorization successfully.
     Rejecting costs nothing, so a griefer cannot burn the payer's authorization.
   - Malformed envelopes return `400`, not `500`.

   Every check is asserted against live on-chain state rather than printed output,
   and the script exits non-zero if any attack gets through — so it is a regression
   test, not a narration.

5. **Run `contracts/verify_live.py`** if there's time — proves the full lifecycle
   including `withdraw()`, the one function the main demo never exercises, which is
   what guarantees a payer can always exit.

## 8. Real on-chain evidence (from actual runs, not fabricated)

| Item | Value |
|---|---|
| `InferenceEscrow` deployment tx | `0x86a035d66aa1c5f516181e3ccb7c5529e03275720e81f3d864608cba5d2c4025` |
| `InferenceEscrow` address | `0x76c03C8b763a0e2f3594bABfb71847e8a6d502D8` |
| Sample Permit2 settlement | `0xb86f05301772c923568139dae239cb2e60ad77848583d0d69fae44daf686b4b8` |
| Sample InferenceEscrow settlement | `0x02bb0dc94ea5478195ca25064e3abd96c4f09c8d7bb7506ecfea892f3c0c34d1` |
| Underpayment exploit, pre-fix (Permit2) | `0x314a6bceb6b610037bf5ca0dcfd81e76a19f3066c13499f47f0546235dabc4af` |
| Underpayment exploit, pre-fix (escrow) | `0x11f155c153a1c19d926aa2142b105ce1a275eb9b34ce7c38c418d455e2bd4e0b` |
| Settle by the named settler, after a stranger was rejected | `0xdf5606cf431b6195e5570e02a3da41c37710529838ca21eb776caa48694333c8` |
| Live `withdraw()` — full 15000-unit exit | `0x1629269c12ffb08f127f128472fae1fadbadd8b598d944b8f727621213964e52` |
| Superseded `InferenceEscrow` (pre-`settler`) | `0xe629C40907f0cB5f93BaeD5F0a97f6E94bD44d7E` |
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
- **`settler` is signed, and `settle()` enforces it** (Path B) — Permit2 calls this
  field `spender`. Dropping `witness.to` was safe because `provider` is immutable,
  but dropping `spender` was not, and we shipped without it initially (§10.2).
- **Unordered nonces, not an incrementing counter** (Path B) — a counter is cheaper
  but serialises the payer, turning two concurrent legitimate prompts into a
  spurious "replay." Deliberately traded gas for correctness under concurrency.
- **`deadline` bounds signature validity** (both paths) — an unbounded signature
  would remain replayable (subject to nonce/idempotency) indefinitely.
- **CEI + `ReentrancyGuard` on `withdraw()`, not `settle()`** (Path B) —
  `withdraw()` sends value to an arbitrary caller-controlled address;
  `settle()` always pays the fixed `provider`, so its reentrancy surface is
  much smaller. The guard is placed where the actual risk is, not everywhere
  reflexively.
- **Validate free, serve, then charge** (both paths) — both schemes happen to offer a
  zero-cost validation step (`/verify`; `settle().call()`), which makes it possible to
  confirm a payment is good, deliver the product, and only then take the money. The
  invariant: never charge without delivering, at the price of occasionally delivering
  without charging (§10.3).
- **Replay protection lives at different layers on purpose** — Path A's facilitator
  is idempotent (by design, to protect payers from double-charges on retried HTTP
  requests), so the gateway's own bookkeeping is what's actually observable. Path
  B's contract enforces it itself, on-chain, because nothing else does.

## 10. Four findings from auditing our own code — three fixed, one open

### 10.1 — Underpayment: the gateway never checked the amount it advertised

Everything in §9 is about defences that were designed in. This section covers what was
*missing*, found by auditing our own code after it was already working and demoable.
§10.1–§10.3 were each reproduced against live infrastructure and fixed; §10.4 was found
by auditing the fix in §10.3 and is deliberately left open. All four are documented
rather than quietly patched — the omissions are more instructive than the final code.

They sit at different layers, and that's the point. §10.1 is an application-layer bug
— the gateway trusted client input. §10.2 is a contract-layer bug — an omitted field in
the signed struct. §10.3 is a *protocol-ordering* bug — the right checks in the wrong
sequence. §10.4 is a *time-of-check* bug, and the only one that came from auditing a
fix rather than the original code. §10.1 and §10.2 are one instance each of the
two failure modes described in §11: Path A's *dependency* and Path B's
*responsibility*.

The first two let a caller take more than they paid for. The third let us take
payment without delivering — an audit that only looks for ways the *customer* cheats
misses half the problem.

Note also what §9 has in common: it is all about defending the **payer**. Both bugs
below were found by instead auditing from the **server's** side, and then by asking
what a third party who merely *observes* a signature could do with it.

Both are reproducible on demand via `security_probes.py`.

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

### 10.2 — `settle()` accepted any submitter (the omitted `spender`)

The original `Authorization` struct was `{amount, nonce, deadline}`, and `settle()`
had no `msg.sender` check at all. That was a conscious design decision — the
signature *is* the authorization, so the submitter shouldn't need to be the payer.
But decoupling `msg.sender` from the payer accidentally decoupled it from
**everyone**: any address could submit any authorization it obtained.

**Why the existing defences didn't cover it.** `provider` being `immutable` prevents
*theft* — a stranger cannot redirect the payment to themselves. It does nothing to
prevent a stranger from **burning the authorization**. And the nonce was never
broken: it correctly guaranteed the payment happened exactly once. It simply never
guaranteed it happened *for the benefit of the party who signed it*.

**The attack.** A third party who obtains a copy of the payer's signature (a logged
request, a compromised proxy) calls `settle()` first. It succeeds: funds move to
`provider`, the payer's balance is debited, the nonce is consumed. The gateway then
submits the payer's *actual* request, which now reverts on the spent nonce, so the
gateway returns `409`. **The payer paid and received nothing.** The attacker gains
nothing either — this is griefing, not theft, and an attack does not need to be
profitable to be an attack.

**The fix.** Restore Permit2's `spender` concept under the name `settler`, as a
signed field:

```solidity
struct Authorization { address settler; uint256 amount; uint256 nonce; uint256 deadline; }
keccak256("Authorization(address settler,uint256 amount,uint256 nonce,uint256 deadline)")
require(msg.sender == auth.settler, "unauthorized settler");
```

The check runs **first**, before any state is touched, so a rejected attempt
consumes nothing and the authorization remains redeemable by the legitimate
gateway. And the field can't be rewritten in calldata to dodge the check, because
it is inside the signed hash — altering it changes the digest and recovery yields a
different address.

**Verified live** on `0x76c03C8b…`, using the payer's *own* wallet as the
unauthorized submitter — the strongest form of the test, since even the person whose
money it is cannot submit their own authorization:

```
Attempt 1 — payer's wallet submits (msg.sender != auth.settler)
  reverted: 'unauthorized settler'
  nonce consumed? False        tab balance: 17000 (unchanged)
Attempt 2 — the named settler submits the SAME authorization
  success: True                nonce consumed? True
  tab balance: 17000 -> 16000 (charged 1000)
```

**A note on debugging EIP-712.** Updating the struct without updating
`AUTHORIZATION_TYPEHASH` changes the digest, so recovery returns an unrelated
address — which has no spent nonces and zero balance, and therefore fails at
`require(balances[payer] >= auth.amount)` with **`"insufficient balance"`**. A
typehash typo reports a *funding* error. `test_RevertWhenStrangerRewritesSettlerField`
pins that behaviour down deliberately so the trap is documented, not rediscovered.

### 10.3 — Payment was not tied to a delivered inference

The first two bugs let a caller take more than they paid for. This one is the
mirror image: the *gateway* could take payment and deliver nothing.

**The bug.** `/infer` settled first and called the LLM afterwards, and `llm.py`
wrapped the provider call in a bare `except Exception` that returned
`"[canned fallback] The metered inference endpoint received your prompt."` So if
Groq was down, rate-limited us, or rejected our key, the sequence was: charge the
payer 1000 units on-chain → provider call fails → swallow the exception → return
**HTTP 200** with a placeholder string. The payer paid full price for a sentence
that contains no inference, and nothing anywhere reported an error.

Worth being precise about what made this bad: the settle-then-serve *ordering* was
survivable on its own, and the swallowed exception was survivable on its own. It was
the combination — charge first, then silently substitute a fake success — that
turned an outage into undetectable theft-by-omission.

**The fix — three phases, in this order.** Both paths already had a *free*
validation step, which is what makes the reordering possible at no cost:

| Phase | Path A | Path B |
|---|---|---|
| 1. Validate (free) | `POST /verify` | `settle().call()` simulation |
| 2. Serve | `call_llm()` | `call_llm()` |
| 3. Settle | `POST /settle` | `settle()` transaction |

`llm.py` now raises `InferenceError` instead of fabricating a completion, and the
gateway returns `502 {"error": "inference failed", "charged": false}` without ever
reaching phase 3.

**The residual risk.** If settlement fails *after* a successful inference, the gateway
has spent an LLM call it cannot bill for. We accept that and return an error rather
than serving the completion unpaid: **never charge without delivering; occasionally
deliver without charging.** The alternative — charging first — is exactly the bug
being fixed.

That exposure is bounded at one inference *per request*, but **not in aggregate**, and
it does not require a race to trigger. Auditing this fix is what surfaced §10.4.

**A second benefit.** Phase 1 now catches replays *before* any inference is spent.
Path B got this for free (the simulation reverts on a spent nonce). Path A needed a
new pre-settle `SEEN_SIGNATURES` guard, because its old tx-hash check only fired
*after* `/settle` returned the facilitator's cached hash — meaning a replayed
payload used to cost us a full inference call before being rejected. The tx-hash
check remains as a backstop, since only the cached hash reveals a *re-signed*
duplicate nonce.

**Verified live** by `security_probes.py`, which spawns a gateway with deliberately
invalid LLM credentials and pays it correctly:

```
--- Provider outage (valid payment, broken LLM) ---
  PASS  returns 5xx, not a fake 200 — got 502: {"error": "inference failed", ...}
  PASS  no canned completion served
  PASS  response says charged=false
  PASS  provider received nothing
  PASS  payer's tab untouched
  PASS  nonce NOT consumed — settle never ran
```

That last assertion is the important one: the nonce being unspent proves `settle()`
was never reached, so the payer's authorization is still usable once the provider
recovers.

### 10.4 — Concurrent requests amplify inference cost (found, **not** fixed)

This one was found by auditing §10.3's *own fix*, and it is documented rather than
patched. It is the most interesting finding in the section precisely because it is the
consequence of a correct fix, not of a careless one.

**The mechanism.** Phase 1 asks the chain "does this payer have enough balance?" —
a **read of mutable state**. Nothing holds that balance. So N concurrent requests from
one payer all pass phase 1, because none of them has settled yet. All N reach phase 2
and consume an inference call. Then one settles; the rest revert `insufficient balance`
and return `402`. A classic time-of-check-to-time-of-use gap: phase 1's answer is only
true at the instant it is asked.

It needs no race to exploit — simply send N requests at once. Timing a `withdraw()`
between phases achieves the same thing with a single request.

**Severity, precisely** — this matters, because the obvious framing overstates it:

| Claim | True? |
|---|---|
| The payer's deposit can be over-drawn | **No.** `require(balances[payer] >= auth.amount)` is absolute; balances cannot go negative |
| The attacker gets free inference | **No.** The completion is withheld whenever settlement fails |
| Funds are at risk | **No.** No accounting invariant is violated |
| **Our provider API spend is amplified** | **Yes.** One payment of `price` can induce N inference calls |

So this is a **cost-amplification / resource-exhaustion** issue against the gateway
operator, not a theft or accounting bug. The contract behaves correctly throughout;
the gateway's *use* of it is what assumes a stable balance.

**Path A has the same exposure.** Between `/verify` and `/settle` the payer can move
their SBC elsewhere, so the facilitator's verdict is equally time-of-check. This is a
property of validate-then-act, not of `InferenceEscrow`.

**The right fix: reserve/capture.** Stop reading a balance and start *holding* one —
the same pattern as a card authorization hold:

```solidity
reserve(auth, signature)  // validate, consume the nonce, move `amount` from
                          // balances[payer] into a reserved bucket keyed by nonce
capture(nonce)            // pay `provider` from the reserved amount
release(nonce)            // after deadline, return it to the payer — callable by anyone
```

Phase 1 becomes `reserve` and phase 3 becomes `capture`. Concurrent requests then
contend on *real* state: N simultaneous prompts require `N × price` actually deposited,
and the TOCTOU gap closes because the funds are no longer readable-but-unclaimed.

Worth stating the cost honestly: that is **two on-chain transactions per prompt instead
of one**, which directly weakens Path B's amortisation advantage in §11. It is a real
trade-off, not a free improvement.

**A cheap interim** (also not implemented): track in-flight requests per payer and
require `balance >= price × (in_flight + 1)`. Application-layer, no redeploy — but
per-process, so it breaks across multiple gateway instances for the same reason
`SEEN_SETTLEMENTS` does.

**Why it is not fixed.** Reserve/capture is a struct change, a redeploy, a signing
change, and a documentation pass, eight days before the presentation, against a demo
that currently works end to end. Given the true severity — our own API cost, no funds
at risk — shipping it in a hurry would be the wrong call. It is recorded here with the
remedy identified, which is the honest position.

## 11. Path A vs Path B — trade-offs, and why both exist

### The one distinction everything follows from

**Path A rents payment infrastructure. Path B owns it.** Every other difference is
downstream of that, including where the money sits: Path A leaves funds in the
payer's own wallet until the instant of payment; Path B requires them at rest in a
contract we wrote.

| Dimension | Path A — Permit2 + facilitator | Path B — InferenceEscrow |
|---|---|---|
| Who verifies the signature | facilitator off-chain, then Permit2 on-chain | our contract, on-chain, every time |
| Who pays gas | facilitator (sponsored) | our operator wallet, from its own balance |
| Payer's funds live | in the payer's wallet until settlement | pre-deposited in our contract |
| Per-prompt work | 2 HTTP round trips + 1 on-chain transfer | 1 free dry run + 1 on-chain transfer |
| On-chain txs per prompt | 1 | 1 — deposit-once cuts *round-trip* overhead, **not** tx count |
| Payer setup | one `approve(Permit2, MAX)`, ever | `deposit()` per top-up, `withdraw()` to exit |
| Replay defence | Permit2's on-chain nonce, but the facilitator is **idempotent**, so the gateway sees a cached success and must keep `SEEN_SETTLEMENTS` | `nonceUsed` reverts on-chain; **the gateway keeps no state** |
| Gateway statefulness | per-process, lost on restart, wrong across instances | stateless — survives restarts and horizontal scaling |
| Liveness depends on | chain **+ facilitator** | chain only |
| Interoperability | any standard x402 client | bespoke `extra.assetTransferMethod` |
| Code we own | none — Permit2 and the proxy are existing infra | all of it |
| Audit status | Permit2 is widely audited, secures large value | unaudited; **we found a griefing bug in it ourselves** (§10.2) |

### Path A — advantages and shortcomings

**Advantages.** No capital lock-up — funds never leave the payer's wallet until
payment. No gas cost and no hot wallet for us to fund, monitor, or rotate keys for.
Battle-tested primitives, so the risk that settlement itself is broken is near zero.
Interoperable by construction. And atomic with no deposit step, which is the right
shape for a first-time or one-off caller.

**Shortcomings.** A third party sits in the critical path, so its liveness becomes
ours. Its idempotency hides on-chain truth, forcing app-level replay state that
doesn't survive a restart and breaks with two gateway instances. Delegated
validation is a subtle trust boundary — §10.1 is the proof: it validates against
whatever requirements we hand it, so passing client-derived requirements made it
approve a 1000× underpayment, and its own check was never broken. Two extra HTTP
round trips per prompt. The one-time `approve(Permit2, MAX)` is a standing unlimited
allowance. No business logic is expressible — you get "transfer exactly this, now,"
so prepaid tabs, refunds, batching and tiering are all off the table. And the
facilitator observes every payment.

### Path B — advantages and shortcomings

**Advantages.** No third-party dependency: only the gateway and the chain. The
resource server becomes **stateless for replay**, because the contract's `nonceUsed`
mapping is authoritative — correctness survives restarts and scales horizontally,
which Path A cannot offer at any price. Fewer moving parts per prompt, with no
external HTTP at all. We control the economics, so prepaid tabs work and refunds,
batching, per-payer credit and tiered pricing become expressible. `withdraw()`
guarantees a payer exit even if the gateway disappears or the operator key is lost.

**Shortcomings.** Capital lock-up and a worse first-use experience: a one-off caller
pays two extra transactions (`deposit`, `withdraw`) to buy a single prompt, which is
strictly worse than Path A. We run a hot wallet that must stay funded and online,
with transaction-nonce management under concurrent load that this project does not
handle. The code is unaudited and bespoke, and we have direct evidence that this
matters — the first deployment omitted Permit2's `spender` field (§10.2). The blast
radius is larger: deposits sit *at rest* behind our code, so where a bug on Path A
could at worst misroute one payment, here it could put every payer's balance at
risk. `token` and `provider` are immutable, so the contract is single-tenant and
rigid. It is not interoperable. And gas per settlement is higher — a fresh
`nonceUsed` slot costs ~20k where Permit2's packed bitmap amortises to ~5k, a
deliberate ~15k trade to avoid ~40 lines of bit manipulation.

### Why build Path B at all, given that list?

Three distinct reasons, none of which is "it is better":

1. **Requirement.** The facilitator path contains **zero lines of original
   contract logic** — it only *calls* Permit2 and `x402ExactPermit2Proxy`, both
   pre-existing audited infrastructure. Integrating someone else's contracts
   correctly, however well done, cannot demonstrate smart-contract work.
2. **Architecture.** The on-chain nonce check makes the resource server hold no
   state at all. This is a property Path A structurally cannot provide, because the
   facilitator's idempotency obscures the on-chain outcome by design.
3. **Economics at volume.** One deposit amortises across dozens of prompts, and each
   prompt then costs one free dry run plus one transaction, with **no external HTTP
   round trips**. For a high-volume session that is a genuine operational win, not
   merely a different trade-off.

### Which is actually better?

**Neither — it depends on volume per payer.** A one-off caller should use Path A;
deposit-plus-withdraw to buy a single prompt is absurd, and Path A's audited
infrastructure and zero capital lock-up dominate. A high-volume session should use
Path B, amortising one deposit and dropping two HTTP round trips per request.

The more interesting conclusion is that **the two paths fail in opposite
directions.** Path A's weakness is *dependency* — a third party in the critical
path, whose caching obscures on-chain truth and whose delegated validation is easy
to misuse. Path B's weakness is *responsibility* — no dependency, but every line of
the primitive is ours to get right, with payer funds at rest behind it. Renting
infrastructure buys audited correctness at the cost of control; owning it buys
control at the cost of being the one who has to be correct. §10.1 and §10.2 are one
instance of each failure mode, found in our own code.

Offering both under a single `accepts[]` is what makes that trade-off demonstrable
rather than theoretical.

## 12. Limitations / honest scope notes

- **Concurrent requests from one payer amplify inference cost** (§10.4) — phase 1 reads
  a balance rather than holding it, so N simultaneous requests each consume an LLM call
  while only one settles. No funds at risk and no free inference; the cost falls on the
  gateway operator. Remedy identified (reserve/capture), not implemented.

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

## 13. What this demonstrates, in one paragraph

Path A shows correct integration with production x402 infrastructure: understanding
the wire protocol, the EIP-712 signing mechanics, and the specific operational
sharp edges (validity-in-body, idempotent replay, hardcoded spender) well enough to
build a working client against a live facilitator on the first attempt. Path B shows
the deeper claim: that the same mechanics — EIP-712 domain separation, signature
recovery, nonce-based replay protection, deadline bounding, and secure withdrawal
patterns — can be designed, implemented, tested, and deployed from scratch, for a
use case (repeated per-prompt billing) that the facilitator model doesn't fit as
naturally as it fits a one-shot purchase.
