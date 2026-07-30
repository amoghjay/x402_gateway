import base64
import json

from fastapi import FastAPI, Request, Response

from llm import InferenceError, call_llm
from payment import (
    PRICE_BASE_UNITS,
    RESOURCE_URL,
    build_escrow_requirements,
    build_payment_requirements,
    settle_payment,
    simulate_escrow_settlement,
    submit_escrow_settlement,
    verify_payment,
)

app = FastAPI()


def _json(status: int, **body) -> Response:
    return Response(status_code=status, content=json.dumps(body), media_type="application/json")


def _underpaid(signed_amount: str) -> Response | None:
    """int() both sides: these arrive as strings, and "9" >= "1000" is True."""
    if int(signed_amount) < int(PRICE_BASE_UNITS):
        return _json(
            402,
            error="underpayment",
            reason=f"signed amount {signed_amount} is less than required {PRICE_BASE_UNITS}",
        )
    return None

# Path A only: the facilitator is idempotent, so a replay returns a cached success
# rather than an error and we must track it ourselves. Signatures catch it before we
# spend an inference; tx hashes catch a re-signed duplicate nonce. Path B needs
# neither — its own on-chain nonce check reverts in the free simulation.
SEEN_SETTLEMENTS: set[str] = set()
SEEN_SIGNATURES: set[str] = set()

PAYMENT_REQUIREMENTS_402_BODY = {
    "x402Version": 2,
    "resource": {
        "url": RESOURCE_URL,
        "description": "One LLM inference call",
        "mimeType": "application/json",
    },
    "accepts": [build_payment_requirements(), build_escrow_requirements()],
}


def _prepare_permit2(payload: dict):
    """Validate without taking any money. Returns (error_response, settle_fn)."""
    signature = payload["payload"]["signature"]
    authorization = payload["payload"]["permit2Authorization"]

    if (underpaid := _underpaid(authorization["permitted"]["amount"])) is not None:
        return underpaid, None

    if signature in SEEN_SIGNATURES:
        return _json(409, error="replay detected", reason="payment signature already used"), None

    verify_result = verify_payment(signature, authorization)
    if not verify_result.get("isValid"):
        return _json(402, error="payment invalid", reason=verify_result.get("invalidReason")), None

    def settle():
        settle_result = settle_payment(signature, authorization)
        if not settle_result.get("success"):
            return _json(402, error="settlement failed"), None

        tx_hash = settle_result["transaction"]
        if tx_hash in SEEN_SETTLEMENTS:
            return _json(409, error="replay detected", transaction=tx_hash), None
        SEEN_SETTLEMENTS.add(tx_hash)
        SEEN_SIGNATURES.add(signature)
        return None, tx_hash

    return None, settle


def _prepare_escrow(payload: dict):
    """Validate for free, take nothing. Returns (error_response, settle_fn).
    The simulation catches everything the real tx would, so no app-level state."""
    signature = payload["payload"]["signature"]
    authorization = payload["payload"]["escrowAuthorization"]

    if (underpaid := _underpaid(authorization["amount"])) is not None:
        return underpaid, None

    simulated = simulate_escrow_settlement(signature, authorization)
    if not simulated["ok"]:
        error = simulated["error"]
        if "nonce already used" in error:
            return _json(409, error="replay detected", reason=error), None
        return _json(402, error="settlement failed", reason=error), None

    def settle():
        settle_result = submit_escrow_settlement(signature, authorization)
        if not settle_result.get("success"):
            return _json(402, error="settlement failed", reason="transaction reverted"), None
        return None, settle_result["transaction"]

    return None, settle


@app.post("/infer")
async def infer(request: Request):
    body = await request.json()
    prompt = body.get("prompt", "")

    payment_header = request.headers.get("X-PAYMENT")
    if not payment_header:
        return _json(402, **PAYMENT_REQUIREMENTS_402_BODY)

    # X-PAYMENT is attacker-controlled: malformed input is a 400, not a 500.
    try:
        payload = json.loads(base64.b64decode(payment_header))
        method = payload["accepted"]["extra"]["assetTransferMethod"]
    except Exception as exc:
        return _json(400, error="malformed X-PAYMENT header", reason=f"{type(exc).__name__}: {exc}")

    if method == "permit2":
        prepare = _prepare_permit2
    elif method == "inference-escrow":
        prepare = _prepare_escrow
    else:
        return _json(402, error=f"unsupported assetTransferMethod: {method}")

    # Phase 1: validate for free. Preparers index client dicts / int() client
    # strings, so garbage raises here too.
    try:
        error, settle = prepare(payload)
    except (KeyError, TypeError, ValueError) as exc:
        return _json(400, error="malformed payment payload", reason=f"{type(exc).__name__}: {exc}")
    if error is not None:
        return error

    # Phase 2: produce the goods BEFORE charging, so a provider outage costs the
    # payer nothing. See REPORT.md §10.3.
    try:
        completion = call_llm(prompt)
    except InferenceError as exc:
        return _json(502, error="inference failed", reason=str(exc), charged=False)

    # Phase 3: settle. On failure we absorb one inference rather than serve unpaid.
    error, tx_hash = settle()
    if error is not None:
        return error

    resp = Response(
        content=json.dumps({"completion": completion}),
        media_type="application/json",
    )
    resp.headers["X-PAYMENT-RESPONSE"] = tx_hash
    return resp
