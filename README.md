# Pay-Per-Prompt

An x402-metered gateway for LLM inference. `POST /infer` returns `402` until paid via a
signed Permit2 authorization, settled on Radius testnet by the facilitator, then serves
one completion from Groq.

Status: MVP working end to end on Radius testnet (chain `72344`), settled via the
Radius facilitator (`facilitator.testnet.radiustech.xyz`) using Permit2 + the
canonical `x402ExactPermit2Proxy`.

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

## Run

```bash
uvicorn gateway:app --port 8000
python client.py
```

## Demo — the three acceptance criteria

```bash
# 1. No payment -> 402 + payment requirements
curl -i -X POST localhost:8000/infer -H 'content-type: application/json' -d '{"prompt":"hello"}'

# 2 & 3. Full loop: 402 -> sign -> pay -> 200 + completion -> replay same payload -> 409
python client.py
```

Sample real run:
```
[1] no payment -> 402
    price required: 1000 base units
[2] with payment -> 200
    completion: Photosynthesis is the process by which plants ...
    settlement tx: 0x107b0ea01e71f22485a2952f045e8bb50fc6d9c01413ebbc3f85ac1750039a13
[3] replay same payment -> 409
    body: {'error': 'replay detected', 'transaction': '0x107b0ea0...'}
```

Both settlement transactions from development are verifiable on
[the Radius testnet explorer](https://testnet.radiustech.xyz):
`0x9714e90a…` (prior project spike, for comparison) and the tx hashes above (this project).

## Design notes

- **Payment path is real, not mocked**: the caller only signs an EIP-712 Permit2
  authorization (no gas, no on-chain tx from the caller). The facilitator does the
  actual on-chain settlement atomically via `x402ExactPermit2Proxy.settle(...)`.
- **Replay protection lives in the gateway, not the facilitator or the chain**:
  the facilitator is idempotent on a replayed payload — it returns the *same*
  cached tx hash rather than re-settling or reverting. The gateway's in-memory
  `SEEN_SETTLEMENTS` set is what turns that into an observable `409`.
- **`isValid` is in the `/verify` response body, not the HTTP status** — a bad
  signature still returns `200`.
