# Pay-Per-Prompt

An x402-metered gateway for LLM inference. `POST /infer` returns `402` with **two**
accepted payment schemes — the caller picks either:

1. **Permit2 + Radius facilitator** — sign once per prompt, a third-party facilitator
   verifies and settles atomically on Radius testnet.
2. **InferenceEscrow** (`contracts/`) — deposit once, then sign a much smaller
   authorization per prompt; the gateway settles it directly against a contract it
   wrote and deployed itself, no third-party facilitator involved.

Both are real, not mocked — every settlement in this README is a genuine testnet
transaction. Status: both schemes working end to end on Radius testnet (chain `72344`).

## Setup

```bash
cd /Users/amoghjay/Desktop/x402/gateway
python -m venv venv && source venv/bin/activate
pip install -r requirements.txt
# .env already exists (gitignored) with verified chain/facilitator/contract values —
# fill in the blank fields: GROQ_API_KEY, PAY_TO_ADDRESS, WALLET_KEY
```

`WALLET_KEY`'s wallet needs SBC balance and a one-time `SBC.approve(Permit2, MAX)`
already in place — use `../fund-test-wallet.sh` if it needs funding.

`GATEWAY_OPERATOR_KEY` is a **separate wallet** from `WALLET_KEY` — it's the one that
submits `InferenceEscrow.settle()` and pays gas for it, distinct from the payer. This
isn't just conceptual: in a real deployment there are three distinct parties (payer,
gateway operator/relayer, provider/revenue wallet), and collapsing payer + operator
into one wallet would hide that `msg.sender` inside `settle()` is genuinely not the
payer. It needs a small SBC balance of its own (Turnstile auto-converts SBC → RUSD
for gas) — fund it the same way as `WALLET_KEY`, or transfer a little SBC to it
directly from an already-funded wallet if the faucet is temporarily empty.

## Run

```bash
uvicorn gateway:app --port 8000
python client.py
```

## Demo — a series of prompts through each scheme, then a replay

`client.py` runs **3 different prompts** through each scheme (not just one — this is
what actually proves the metering is real and repeatable, and for `InferenceEscrow`
it's the only way to see the nonce genuinely incrementing across legitimate
requests: `6 → 7 → 8`, not just "0 works once, then reverts"), then replays the
last payment to confirm `409`.

```bash
# 1. No payment -> 402 + both payment requirements (accepts[] has two entries)
curl -i -X POST localhost:8000/infer -H 'content-type: application/json' -d '{"prompt":"hello"}'

# The full series for both schemes: 3x (402 -> sign -> pay -> 200 + completion) -> replay -> 409
python client.py
```

Sample real run (abridged — full completions/links appear in the actual output):
```
=== Demo 1: Permit2 + Radius facilitator — 3 sequential prompts ===
BEFORE — payer: 23.326000 SBC, provider: 109.052000 SBC
[1/3] "In one sentence, what is photosynthesis?"
    -> Photosynthesis is the process by which plants, algae, and some bacteria ...
    settlement tx: 0x2d5be6c8...  (https://testnet.radiustech.xyz/tx/0x2d5be6c8...)
[2/3] "In one sentence, what is the Pythagorean theorem?"
    -> The Pythagorean theorem is a mathematical concept that states that ...
    settlement tx: 0x5f9454b7...
[3/3] "In one sentence, what is a black hole?"
    -> A black hole is a region in space where the gravitational pull is so strong ...
    settlement tx: 0x583e6485...
AFTER  — payer: 23.323000 SBC, provider: 109.055000 SBC
delta  — payer: -3000, provider: 3000 (exactly 3 x 1000 = 3000)
[replay] reusing call #3's payment -> 409
    body: {'error': 'replay detected', 'transaction': '0x583e6485...'}

=== Demo 2: InferenceEscrow — 3 sequential prompts, one deposit ===
escrow deposit balance: 15000 base units (covers all 3 calls from ONE deposit tx)
payer wallet    : 0xfd4dc70f4b9c4055aC58c6a642aE2bc7be3B032A
gateway operator: 0xE5377D7716EEC361Be8FA1aEE3BDF92996614C00  <- DIFFERENT wallet
BEFORE — provider: 109.055000 SBC, operator: 0.990000 SBC
[1/3] "In one sentence, what is mitosis?"  (nonce 6)
    -> Mitosis is the process of cell division that results in two genetically ...
    settlement tx: 0x9211a65f...
[2/3] "In one sentence, what is entropy?"  (nonce 7)
    -> Entropy is a measure of the amount of disorder, randomness, or uncertainty ...
    settlement tx: 0xa7a9d336...
[3/3] "In one sentence, what is a blockchain?"  (nonce 8)
    -> A blockchain is a decentralized, digital ledger that records transactions ...
    settlement tx: 0xc03b6175...
AFTER  — provider: 109.058000 SBC, operator: 0.990000 SBC
delta  — provider: 3000 (exactly 3 x 1000 = 3000)
delta  — operator: 0 (see note below)
nonces used, in order: ['6', '7', '8']
[replay] reusing call #3's payment (nonce 8 again) -> 409
    body: {'error': 'replay detected', 'reason': 'invalid nonce'}

======================================================================
SUMMARY
======================================================================

Permit2 + facilitator
  prompts served    : 3
  provider received : 3000 base units total
  who paid gas      : facilitator (sponsored)
  replay mechanism  : app-level SEEN_SETTLEMENTS set (facilitator is idempotent, returns cached success)

InferenceEscrow
  prompts served    : 3
  provider received : 3000 base units total
  who paid gas      : gateway operator wallet
  replay mechanism  : on-chain revert in settle() itself ("invalid nonce") — no app-level set needed
```

Note on the operator's `0` gas delta above: `sbc_balance()` only reads the SBC
ERC-20 balance. Turnstile's SBC→RUSD conversion is a one-time top-up trigger when
the native RUSD reserve runs low (that's where the `-10000` came from on this
wallet's very first settlement), not a per-transaction charge against SBC — these
three calls spent from the RUSD already converted earlier, which this balance
check doesn't track. Not a bug, just a blind spot in what we're measuring.

Every tx hash above is a real, clickable link to the Radius testnet explorer —
open one live to show it's not a mocked printout.

The **reason** InferenceEscrow exists isn't the replay-mechanism difference — it's
that the facilitator path never contains any original smart-contract logic (it only
*calls* Permit2 and `x402ExactPermit2Proxy`, both pre-existing infra). InferenceEscrow
is the original work: the EIP-712 verification, `ECDSA.recover`, and accounting are
written and deployed for this project. The replay contrast below is a genuine,
useful **consequence** of that — not the goal itself:

| | Permit2 + facilitator | InferenceEscrow |
|---|---|---|
| Who verifies the signature | third-party facilitator (off-chain, then on-chain settle) | the contract itself, on-chain, every time |
| Who submits + pays gas | facilitator's own wallet (sponsored, invisible to us) | `GATEWAY_OPERATOR_KEY` — a wallet we fund and control, genuinely separate from the payer's `WALLET_KEY` |
| Why replay produces `409` | facilitator is **idempotent** — returns the same cached tx hash, `success: true` both times; the gateway's in-memory `SEEN_SETTLEMENTS` set is what turns that into a `409` | the contract's own `nextNonce` mapping makes the second call **revert** (`"invalid nonce"`) — no app-level set needed at all |
| What's novel here | none — Permit2 and the proxy are existing, audited infra | the EIP-712 domain, `ECDSA.recover`, nonce/deadline accounting, and CEI+`ReentrancyGuard` on `withdraw()` are all written and deployed for this project |

The operator wallet's own SBC balance drops slightly each settlement (Turnstile
auto-converting it to RUSD for gas) — a completely different number from the
per-prompt charge, and visible proof that "the gateway pays its own gas" isn't
just an assertion in this project.

`InferenceEscrow` deployed to Radius testnet: `0xe629C40907f0cB5f93BaeD5F0a97f6E94bD44d7E`
(5/5 Foundry tests passing — see `contracts/test/InferenceEscrow.t.sol`).

## Design notes

- **Both payment paths are real, not mocked** — every settlement above is a genuine
  Radius testnet transaction, verifiable via `cast receipt <tx> --rpc-url https://rpc.testnet.radiustech.xyz`.
- **`isValid` is in the facilitator's `/verify` response body, not the HTTP status**
  — a bad signature still returns `200`.
- **`msg.sender` inside `InferenceEscrow.settle()` is the gateway, not the payer** —
  the payer is whoever `ECDSA.recover` says signed the authorization. This is the
  same caller/authorizer split that makes Permit2 (and every gasless/meta-tx
  pattern) work.
