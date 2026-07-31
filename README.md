# Pay-Per-Prompt

An **x402-metered gateway for LLM inference**, on Radius testnet (chain `72344`).
`POST /infer` costs money: unpaid requests get `402 Payment Required` with a
machine-readable price, the caller pays by signing a message, and the gateway serves
the completion. Every transaction here is real — nothing is mocked.

## What this is trying to show

The gateway advertises **two independent ways to pay for the same product**, so they
can be compared under identical conditions:

| | Scheme | Settled by | Original code? |
|---|---|---|---|
**Path A** | Permit2 + Radius facilitator | a third-party facilitator service | none — existing audited infra |
**Path B** | `InferenceEscrow` (`contracts/`) | a contract written and deployed for this project | all of it |

**Path A rents payment infrastructure; Path B owns it.** Every difference between
them follows from that, and the point of building both is to make the trade-off
demonstrable rather than theoretical — including the parts where Path B is *worse*.

Two things this repo deliberately does beyond "it works":

- **Three genuinely separate wallets** (payer / gateway operator / provider), never
  collapsed for convenience, so `msg.sender != payer` inside `settle()` is a fact
  you can watch rather than a claim.
- **Its own attacks.** `security_probes.py` runs four exploits that all used to
  succeed against this code, asserting against live on-chain state.

Full analysis lives in **[REPORT.md](REPORT.md)** — §11 for the path comparison and
trade-offs, §10 for the four findings from auditing this code: two where a caller could
take more than they paid for, one where *we* could take payment and deliver nothing,
and one found by auditing that last fix, left open on purpose.

## Setup

```bash
python -m venv venv && source venv/bin/activate
pip install -r requirements.txt
# .env is gitignored and already holds verified chain/facilitator/contract values.
# Fill in: GROQ_API_KEY, PAY_TO_ADDRESS, WALLET_KEY, GATEWAY_OPERATOR_KEY
```

| Env var | Role | Needs |
|---|---|---|
`WALLET_KEY` | **payer** | SBC balance + one-time `approve(Permit2, MAX)` |
`GATEWAY_OPERATOR_KEY` | **gateway operator** — submits `settle()`, pays its gas | a small SBC balance |
`PAY_TO_ADDRESS` | **provider** — receives revenue | nothing |

Fund with `../fund-test-wallet.sh`, or transfer SBC directly if the faucet is dry.

## Run

```bash
uvicorn gateway:app --port 8000        # terminal 1

python client.py                       # happy path: 3 prompts through each scheme
python security_probes.py              # adversarial: 4 attacks, all blocked
cd contracts && forge test             # 9/9 contract tests
cd contracts && python verify_live.py  # live deposit -> settle -> withdraw
```

`client.py` runs **three** prompts per path, not one — that's what proves the
metering is repeatable, that three independent nonces are each spent exactly once,
and that the balance deltas land on exactly `3 × price`. It then replays the last
payment to show both paths reject it (`409`) for structurally different reasons.

Verify any settlement independently:

```bash
cast receipt <tx> --rpc-url https://rpc.testnet.radiustech.xyz
```

## How a paid request flows

**Path A** — sign per prompt, facilitator settles:

```
402 + accepts[2]  →  sign PermitWitnessTransferFrom
                  →  POST /verify   phase 1: validate, free
                  →  call_llm()     phase 2: serve, before charging
                  →  POST /settle   phase 3: facilitator submits + pays gas
```

**Path B** — deposit once, then sign per prompt:

```
deposit()  (once, payer pays gas)
402 + accepts[2]  →  sign Authorization{settler, amount, nonce, deadline}
                  →  settle().call()   phase 1: free simulation, this path's /verify
                  →  call_llm()        phase 2: serve, before charging
                  →  settle()          phase 3: operator wallet submits + pays gas
withdraw()  (any time, payer's choice)
```

No third party is involved anywhere in Path B.

## Adversarial probes

`security_probes.py` — each of these used to work; the script exits non-zero if any
gets through.

| Probe | Attack | Expected |
|---|---|---|
Underpayment (both paths) | sign 1 base unit for a 1000-unit product | `402`, no funds move, no completion |
Unauthorized settler | redeem an escrow authorization from a wallet other than `auth.settler` | revert, **nonce not consumed**, named settler still succeeds |
Malformed envelope | bad base64 / bad JSON / missing keys / non-numeric amount | `400`, never `500` |
Provider outage | pay correctly, but the LLM credentials are broken | `502` + `charged: false`, **nonce not consumed** |

The settler probe submits from **the payer's own wallet** — even the person whose
money it is cannot redeem their own authorization; only the named settler can.

## Deployed addresses

| Item | Address |
|---|---|
`InferenceEscrow` (current) | `0x76c03C8b763a0e2f3594bABfb71847e8a6d502D8` |
`InferenceEscrow` (superseded, pre-`settler`) | `0xe629C40907f0cB5f93BaeD5F0a97f6E94bD44d7E` |
SBC token | `0x33ad9e4BD16B69B5BFdED37D8B5D9fF9aba014Fb` |
Canonical Permit2 | `0x000000000022D473030F116dDEE9F6B43aC78BA3` |
`x402ExactPermit2Proxy` | `0x402085c248EeA27D92E8b30b2C58ed07f9E20001` |
Facilitator | `https://facilitator.testnet.radiustech.xyz` |

## Files

| File | Purpose |
|---|---|
`gateway.py` | the `402`/`200` endpoint, scheme routing, price enforcement |
`payment.py` | EIP-712 signing for both schemes, facilitator calls, on-chain settle |
`client.py` | the happy-path demo |
`security_probes.py` | the adversarial demo |
`llm.py` | Groq inference call |
`contracts/src/InferenceEscrow.sol` | the deployed contract |
`contracts/test/InferenceEscrow.t.sol` | 9 Foundry tests |
`contracts/verify_live.py` | live deposit → settle → withdraw lifecycle |

## Gotchas worth knowing

- **`isValid` is in the facilitator's `/verify` response *body*, not the HTTP
  status.** A bad signature still returns `200`.
- **The server validates against requirements it holds itself**, never values echoed
  back from the client. `verify_payment`/`settle_payment` take no `amount` parameter
  *by design* — see REPORT.md §10.1 for the bug that taught us this.
- **The operator's SBC balance often shows a `0` delta.** Radius converts SBC → RUSD
  in one-time top-ups, not per transaction, so gas paid from an earlier conversion
  doesn't appear in an SBC balance check. A blind spot in the measurement, not a bug.
- **The escrow ABI is read from Foundry's build artifact**, so it cannot drift from
  the deployed contract.
- **Requests validate for free, serve, and only then charge.** Both schemes have a
  zero-cost validation step, so a provider outage returns `502 charged: false`
  instead of taking the money — never charge without delivering. See REPORT.md §10.3.

## Known limitations

- **Concurrent requests from one payer amplify inference cost** (REPORT.md §10.4) —
  validation *reads* a balance rather than holding it, so N simultaneous requests each
  consume an LLM call while only one settles. No funds at risk and no free inference:
  the cost falls on the gateway operator. The fix is a reserve/capture split; found by
  auditing the §10.3 fix, documented rather than rushed.
- `SEEN_SIGNATURES`/`SEEN_SETTLEMENTS` grow unbounded and are lost on restart (Path A
  only — Path B keeps no state).
- The operator wallet is a single unmonitored hot wallet with no nonce-collision
  handling under concurrent load.
- Fixed price per call; single provider and model; no routing.
- The claim that Permit2 uses a packed nonce bitmap comes from documentation, not
  independent verification against the deployed Permit2 on this chain.
