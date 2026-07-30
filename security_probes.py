"""Adversarial probes against our own gateway and contract.

Each probe is an attack that USED to work and is now blocked. Run with the gateway
up (`uvicorn gateway:app --port 8000`). Every assertion is checked against live
on-chain state, not against printed output — see REPORT.md §10.

  1. Underpayment      — sign 1 base unit for a 1000-unit product, both paths.
  2. Unauthorized settler — redeem someone else's escrow authorization.
  3. Malformed envelope  — garbage X-PAYMENT should be 400, never 500.
  4. Provider outage   — if the LLM fails, the payer must not be charged.
"""
import base64
import json
import os
import subprocess
import time

import requests
from web3.exceptions import ContractLogicError

from client import build_x_payment_header, get_requirements
from payment import (
    GATEWAY_OPERATOR_ADDRESS,
    PAYER_ADDRESS,
    PAY_TO_ADDRESS,
    _decode_revert_reason,
    _escrow,
    ensure_escrow_deposit,
    explorer_link,
    sbc_balance,
    settle_escrow_payment,
    sign_escrow_authorization,
    sign_permit2_payment,
)

GATEWAY_URL = "http://localhost:8000/infer"
UNDERPAY = 1
RESOURCE = {"url": GATEWAY_URL, "description": "One LLM inference call", "mimeType": "application/json"}

failures = []


def check(label: str, ok: bool, detail: str = "") -> None:
    print(f"  {'PASS' if ok else 'FAIL'}  {label}{f' — {detail}' if detail else ''}")
    if not ok:
        failures.append(label)


# ---------------------------------------------------------------- probe 1
def probe_underpayment(name: str, requirement: dict, auth_key: str, signer) -> None:
    print(f"\n--- Underpayment via {name} ---")
    advertised = int(requirement["amount"])
    provider_before = sbc_balance(PAY_TO_ADDRESS)

    signature, authorization = signer(amount=UNDERPAY)
    # `accepted` carries the gateway's OWN advertised price, untouched. Only the
    # signed payload says 1 — the contradiction sits inside a single request.
    header = build_x_payment_header(RESOURCE, requirement, signature, auth_key, authorization)
    print(f"  advertised {advertised}, signed {UNDERPAY} ({advertised}x underpayment)")

    resp = requests.post(GATEWAY_URL, json={"prompt": "free lunch?"}, headers={"X-PAYMENT": header})
    check("rejected with 402", resp.status_code == 402, f"got {resp.status_code}: {resp.text[:90]}")
    check("no funds moved", sbc_balance(PAY_TO_ADDRESS) == provider_before)
    check("no completion served", "completion" not in resp.text)


# ---------------------------------------------------------------- probe 2
def probe_unauthorized_settler() -> None:
    print("\n--- Unauthorized settler (escrow) ---")
    price = int(escrow_req["amount"])
    ensure_escrow_deposit(min_amount=price * 2)

    signature, auth = sign_escrow_authorization(amount=price)
    nonce = int(auth["nonce"])
    auth_tuple = (auth["settler"], int(auth["amount"]), nonce, int(auth["deadline"]))
    tab_before = _escrow.functions.balances(PAYER_ADDRESS).call()

    print(f"  auth.settler     = {auth['settler']} (the gateway operator)")
    print(f"  attempting from  = {PAYER_ADDRESS} (the payer's OWN wallet)")

    # Even the payer cannot submit their own authorization — only the named settler.
    try:
        _escrow.functions.settle(auth_tuple, bytes.fromhex(signature[2:])).call({"from": PAYER_ADDRESS})
        check("stranger rejected", False, "NO REVERT — settler check is not working")
    except ContractLogicError as exc:
        reason = _decode_revert_reason(exc)
        check("stranger rejected", reason == "unauthorized settler", f"reverted: {reason!r}")

    check("nonce not consumed", not _escrow.functions.nonceUsed(PAYER_ADDRESS, nonce).call())
    check("tab untouched", _escrow.functions.balances(PAYER_ADDRESS).call() == tab_before)

    # The rejection cost nothing, so the authorization is still redeemable.
    result = settle_escrow_payment(signature, auth)
    check("named settler still succeeds", result.get("success") is True)
    check("nonce now consumed", _escrow.functions.nonceUsed(PAYER_ADDRESS, nonce).call())
    charged = tab_before - _escrow.functions.balances(PAYER_ADDRESS).call()
    check("charged exactly once", charged == price, f"charged {charged}")
    if result.get("transaction"):
        print(f"  settled by named settler: {explorer_link(result['transaction'])}")


# ---------------------------------------------------------------- probe 3
def probe_malformed_envelopes() -> None:
    print("\n--- Malformed X-PAYMENT envelopes ---")
    cases = {
        "not base64": "not-base64!!",
        "not json": base64.b64encode(b"not json").decode(),
        "missing keys": base64.b64encode(json.dumps({"accepted": {}}).encode()).decode(),
        "non-numeric amount": base64.b64encode(
            json.dumps(
                {
                    "accepted": {"extra": {"assetTransferMethod": "inference-escrow"}},
                    "payload": {"signature": "0xdead", "escrowAuthorization": {"amount": "abc"}},
                }
            ).encode()
        ).decode(),
    }
    for label, header in cases.items():
        resp = requests.post(GATEWAY_URL, json={"prompt": "x"}, headers={"X-PAYMENT": header})
        check(f"{label} -> 4xx not 5xx", 400 <= resp.status_code < 500, f"got {resp.status_code}")


# ---------------------------------------------------------------- probe 4
def probe_provider_outage() -> None:
    """Spawn a gateway whose LLM credentials are broken, then pay it properly.

    The payment is entirely valid — the *provider* is what's down. Settling before
    serving meant this took the payer's money and returned 200 with a canned
    placeholder. Now inference happens first, so a failure means no charge at all.
    """
    print("\n--- Provider outage (valid payment, broken LLM) ---")
    port = 8001
    env = {**os.environ, "GROQ_API_KEY": "gsk_deliberately_invalid_key_for_probe"}
    proc = subprocess.Popen(
        ["venv/bin/python", "-m", "uvicorn", "gateway:app", "--port", str(port)],
        env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
    )
    url = f"http://localhost:{port}/infer"
    try:
        for _ in range(40):  # wait for readiness
            try:
                if requests.post(url, json={"prompt": "ping"}, timeout=2).status_code == 402:
                    break
            except requests.RequestException:
                time.sleep(0.5)
        else:
            check("broken-LLM gateway started", False, "never became ready")
            return

        req = requests.post(url, json={"prompt": "ping"}).json()
        escrow = next(r for r in req["accepts"] if r["extra"]["assetTransferMethod"] == "inference-escrow")
        price = int(escrow["amount"])
        ensure_escrow_deposit(min_amount=price * 2)

        signature, auth = sign_escrow_authorization(amount=price)
        nonce = int(auth["nonce"])
        header = build_x_payment_header(RESOURCE, escrow, signature, "escrowAuthorization", auth)

        provider_before = sbc_balance(PAY_TO_ADDRESS)
        tab_before = _escrow.functions.balances(PAYER_ADDRESS).call()
        resp = requests.post(url, json={"prompt": "what is a mutex?"}, headers={"X-PAYMENT": header})

        check("returns 5xx, not a fake 200", resp.status_code >= 500, f"got {resp.status_code}: {resp.text[:80]}")
        check("no canned completion served", "completion" not in resp.text)
        check("response says charged=false", resp.json().get("charged") is False)
        check("provider received nothing", sbc_balance(PAY_TO_ADDRESS) == provider_before)
        check("payer's tab untouched", _escrow.functions.balances(PAYER_ADDRESS).call() == tab_before)
        check("nonce NOT consumed — settle never ran", not _escrow.functions.nonceUsed(PAYER_ADDRESS, nonce).call())
    finally:
        proc.terminate()
        proc.wait(timeout=10)


if __name__ == "__main__":
    requirements = get_requirements("probe warm-up")
    permit2_req = next(r for r in requirements["accepts"] if r["extra"]["assetTransferMethod"] == "permit2")
    escrow_req = next(r for r in requirements["accepts"] if r["extra"]["assetTransferMethod"] == "inference-escrow")

    print("=" * 72)
    print("ADVERSARIAL PROBES — every one of these used to succeed")
    print("=" * 72)
    print(f"payer    {PAYER_ADDRESS}\noperator {GATEWAY_OPERATOR_ADDRESS}\nprovider {PAY_TO_ADDRESS}")

    probe_underpayment("Permit2 + facilitator", permit2_req, "permit2Authorization", sign_permit2_payment)
    probe_underpayment("InferenceEscrow", escrow_req, "escrowAuthorization", sign_escrow_authorization)
    probe_unauthorized_settler()
    probe_malformed_envelopes()
    probe_provider_outage()

    print("\n" + "=" * 72)
    if failures:
        print(f"{len(failures)} CHECK(S) FAILED: {failures}")
        raise SystemExit(1)
    print("All probes blocked. No funds moved, no nonces burned, no 5xx.")
