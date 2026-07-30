import base64
import json

import requests

from payment import (
    GATEWAY_OPERATOR_ADDRESS,
    PAY_TO_ADDRESS,
    PAYER_ADDRESS,
    ensure_escrow_deposit,
    explorer_link,
    sbc_balance,
    sign_escrow_authorization,
    sign_permit2_payment,
)

GATEWAY_URL = "http://localhost:8000/infer"

PERMIT2_PROMPTS = [
    "In one sentence, what is photosynthesis?",
    "In one sentence, what is the Pythagorean theorem?",
    "In one sentence, what is a black hole?",
]

ESCROW_PROMPTS = [
    "In one sentence, what is mitosis?",
    "In one sentence, what is entropy?",
    "In one sentence, what is a blockchain?",
]


def build_x_payment_header(resource: dict, requirement: dict, signature: str, auth_key: str, authorization: dict) -> str:
    envelope = {
        "x402Version": 2,
        "resource": resource,
        "accepted": requirement,
        "payload": {
            "signature": signature,
            auth_key: authorization,
        },
    }
    return base64.b64encode(json.dumps(envelope).encode()).decode()


def get_requirements(prompt: str) -> dict:
    resp = requests.post(GATEWAY_URL, json={"prompt": prompt})
    assert resp.status_code == 402
    return resp.json()


def fmt_sbc(base_units: int) -> str:
    return f"{base_units / 1e6:.6f} SBC ({base_units} base units)"


def fmt_nonce(nonce: str) -> str:
    """Escrow nonces are random 32-byte values (unordered, so concurrent prompts
    can't collide), which are far too long to print in full."""
    return f"0x{int(nonce):064x}"[:12] + "…"


def demo_permit2_series(prompts: list[str]) -> dict:
    print(f"=== Demo 1: Permit2 + Radius facilitator — {len(prompts)} sequential prompts ===")

    payer_before = sbc_balance(PAYER_ADDRESS)
    provider_before = sbc_balance(PAY_TO_ADDRESS)
    print(f"BEFORE — payer: {fmt_sbc(payer_before)}, provider: {fmt_sbc(provider_before)}")

    calls = []
    last_x_payment = None
    last_prompt = None
    price = None

    for i, prompt in enumerate(prompts, start=1):
        requirements = get_requirements(prompt)
        permit2_req = next(r for r in requirements["accepts"] if r["extra"]["assetTransferMethod"] == "permit2")
        price = int(permit2_req["amount"])

        signature, authorization = sign_permit2_payment(amount=price)
        x_payment = build_x_payment_header(
            requirements["resource"], permit2_req, signature, "permit2Authorization", authorization
        )

        resp = requests.post(GATEWAY_URL, json={"prompt": prompt}, headers={"X-PAYMENT": x_payment})
        assert resp.status_code == 200, f"call {i} failed: {resp.status_code} {resp.text}"
        tx_hash = resp.headers.get("X-PAYMENT-RESPONSE")
        completion = resp.json()["completion"]

        print(f"[{i}/{len(prompts)}] \"{prompt}\"")
        print(f"    -> {completion}")
        print(f"    settlement tx: {tx_hash}  ({explorer_link(tx_hash)})")

        calls.append({"prompt": prompt, "tx": tx_hash})
        last_x_payment, last_prompt = x_payment, prompt

    payer_after = sbc_balance(PAYER_ADDRESS)
    provider_after = sbc_balance(PAY_TO_ADDRESS)
    print(f"AFTER  — payer: {fmt_sbc(payer_after)}, provider: {fmt_sbc(provider_after)}")
    print(f"delta  — payer: {payer_after - payer_before}, provider: {provider_after - provider_before} "
          f"(exactly {len(prompts)} x {price} = {len(prompts) * price})")

    resp = requests.post(GATEWAY_URL, json={"prompt": last_prompt}, headers={"X-PAYMENT": last_x_payment})
    print(f"[replay] reusing call #{len(prompts)}'s payment -> {resp.status_code}")
    assert resp.status_code == 409
    print(f"    body: {resp.json()}")

    return {
        "scheme": "Permit2 + facilitator",
        "calls": calls,
        "provider_delta": provider_after - provider_before,
        "gas_payer": "facilitator (sponsored)",
        "replay_mechanism": "app-level state (facilitator is idempotent, returns cached success): SEEN_SIGNATURES pre-settle, SEEN_SETTLEMENTS as backstop",
    }


def demo_escrow_series(prompts: list[str]) -> dict:
    print(f"\n=== Demo 2: InferenceEscrow — {len(prompts)} sequential prompts, one deposit ===")

    requirements = get_requirements(prompts[0])
    escrow_req = next(r for r in requirements["accepts"] if r["extra"]["assetTransferMethod"] == "inference-escrow")
    price = int(escrow_req["amount"])

    balance = ensure_escrow_deposit(min_amount=price * len(prompts))
    print(f"escrow deposit balance: {balance} base units (covers all {len(prompts)} calls from ONE deposit tx)")
    print(f"payer wallet    : {PAYER_ADDRESS}")
    print(f"gateway operator: {GATEWAY_OPERATOR_ADDRESS}  <- DIFFERENT wallet, submits settle() + pays its own gas")

    provider_before = sbc_balance(PAY_TO_ADDRESS)
    operator_before = sbc_balance(GATEWAY_OPERATOR_ADDRESS)
    print(f"BEFORE — provider: {fmt_sbc(provider_before)}, operator: {fmt_sbc(operator_before)}")

    calls = []
    last_x_payment = None
    last_prompt = None
    last_nonce = None

    for i, prompt in enumerate(prompts, start=1):
        requirements = get_requirements(prompt)
        escrow_req = next(r for r in requirements["accepts"] if r["extra"]["assetTransferMethod"] == "inference-escrow")

        signature, authorization = sign_escrow_authorization(amount=price)
        x_payment = build_x_payment_header(
            requirements["resource"], escrow_req, signature, "escrowAuthorization", authorization
        )

        resp = requests.post(GATEWAY_URL, json={"prompt": prompt}, headers={"X-PAYMENT": x_payment})
        assert resp.status_code == 200, f"call {i} failed: {resp.status_code} {resp.text}"
        tx_hash = resp.headers.get("X-PAYMENT-RESPONSE")
        completion = resp.json()["completion"]

        print(f"[{i}/{len(prompts)}] \"{prompt}\"  (nonce {fmt_nonce(authorization['nonce'])})")
        print(f"    -> {completion}")
        print(f"    settlement tx: {tx_hash}  ({explorer_link(tx_hash)})")

        calls.append({"prompt": prompt, "tx": tx_hash, "nonce": authorization["nonce"]})
        last_x_payment, last_prompt, last_nonce = x_payment, prompt, authorization["nonce"]

    provider_after = sbc_balance(PAY_TO_ADDRESS)
    operator_after = sbc_balance(GATEWAY_OPERATOR_ADDRESS)
    print(f"AFTER  — provider: {fmt_sbc(provider_after)}, operator: {fmt_sbc(operator_after)}")
    print(f"delta  — provider: {provider_after - provider_before} (exactly {len(prompts)} x {price} = {len(prompts) * price})")
    print(f"delta  — operator: {operator_after - operator_before} (its OWN SBC spent on gas for {len(prompts)} settle() txs — unrelated to the charges above)")
    print(f"nonces used (random + unordered, all distinct): {[fmt_nonce(c['nonce']) for c in calls]}")

    resp = requests.post(GATEWAY_URL, json={"prompt": last_prompt}, headers={"X-PAYMENT": last_x_payment})
    print(f"[replay] reusing call #{len(prompts)}'s payment (nonce {fmt_nonce(last_nonce)} again) -> {resp.status_code}")
    assert resp.status_code == 409
    print(f"    body: {resp.json()}")

    return {
        "scheme": "InferenceEscrow",
        "calls": calls,
        "provider_delta": provider_after - provider_before,
        "gas_payer": "gateway operator wallet",
        "replay_mechanism": 'on-chain revert in settle() itself ("nonce already used") — no app-level set needed',
    }


def print_summary(r1: dict, r2: dict):
    print("\n" + "=" * 70)
    print("SUMMARY")
    print("=" * 70)
    for r in (r1, r2):
        print(f"\n{r['scheme']}")
        print(f"  prompts served    : {len(r['calls'])}")
        for c in r["calls"]:
            nonce_note = f" (nonce {fmt_nonce(c['nonce'])})" if "nonce" in c else ""
            print(f"    - {c['tx'][:14]}...{nonce_note}  {explorer_link(c['tx'])}")
        print(f"  provider received : {r['provider_delta']} base units total")
        print(f"  who paid gas      : {r['gas_payer']}")
        print(f"  replay mechanism  : {r['replay_mechanism']}")


if __name__ == "__main__":
    result1 = demo_permit2_series(PERMIT2_PROMPTS)
    result2 = demo_escrow_series(ESCROW_PROMPTS)
    print_summary(result1, result2)
