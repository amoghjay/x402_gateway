import base64
import json

from fastapi import FastAPI, Request, Response

from llm import call_llm
from payment import (
    PRICE_BASE_UNITS,
    RESOURCE_URL,
    build_escrow_requirements,
    build_payment_requirements,
    settle_escrow_payment,
    settle_payment,
    verify_payment,
)

app = FastAPI()


def _json(status: int, **body) -> Response:
    return Response(status_code=status, content=json.dumps(body), media_type="application/json")


def _underpaid(signed_amount: str) -> Response | None:
    """Reject an authorization for less than our advertised price. See REPORT.md §10.
    int() both sides — these are strings, and "9" >= "1000" is True."""
    if int(signed_amount) < int(PRICE_BASE_UNITS):
        return _json(
            402,
            error="underpayment",
            reason=f"signed amount {signed_amount} is less than required {PRICE_BASE_UNITS}",
        )
    return None

# In-memory replay guard for the Permit2/facilitator path ONLY. See Phase A:
# the facilitator is idempotent on a replayed payload (returns the same
# cached tx hash rather than re-settling), so this set is what turns that
# into an observable 409 rather than silently serving the request twice.
# The InferenceEscrow path doesn't need this — its own on-chain nonce check
# makes a replay revert instead of silently "succeeding" a second time.
SEEN_SETTLEMENTS: set[str] = set()

PAYMENT_REQUIREMENTS_402_BODY = {
    "x402Version": 2,
    "resource": {
        "url": RESOURCE_URL,
        "description": "One LLM inference call",
        "mimeType": "application/json",
    },
    "accepts": [build_payment_requirements(), build_escrow_requirements()],
}


def _handle_permit2(payload: dict):
    signature = payload["payload"]["signature"]
    authorization = payload["payload"]["permit2Authorization"]

    if (underpaid := _underpaid(authorization["permitted"]["amount"])) is not None:
        return underpaid

    verify_result = verify_payment(signature, authorization)
    if not verify_result.get("isValid"):
        return _json(402, error="payment invalid", reason=verify_result.get("invalidReason"))

    settle_result = settle_payment(signature, authorization)
    if not settle_result.get("success"):
        return _json(402, error="settlement failed")

    tx_hash = settle_result["transaction"]
    if tx_hash in SEEN_SETTLEMENTS:
        return _json(409, error="replay detected", transaction=tx_hash)
    SEEN_SETTLEMENTS.add(tx_hash)
    return tx_hash


def _handle_escrow(payload: dict):
    signature = payload["payload"]["signature"]
    authorization = payload["payload"]["escrowAuthorization"]

    if (underpaid := _underpaid(authorization["amount"])) is not None:
        return underpaid

    settle_result = settle_escrow_payment(signature, authorization)
    if not settle_result.get("success"):
        error = settle_result.get("error", "")
        # The contract's own revert reasons ARE the replay/expiry signal here —
        # no app-level SEEN_SETTLEMENTS set needed for this path.
        if "invalid nonce" in error:
            return _json(409, error="replay detected", reason=error)
        return _json(402, error="settlement failed", reason=error)
    return settle_result["transaction"]


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
        handler = _handle_permit2
    elif method == "inference-escrow":
        handler = _handle_escrow
    else:
        return _json(402, error=f"unsupported assetTransferMethod: {method}")

    # Same, one level down: handlers index client dicts and int() client strings.
    try:
        result = handler(payload)
    except (KeyError, TypeError, ValueError) as exc:
        return _json(400, error="malformed payment payload", reason=f"{type(exc).__name__}: {exc}")

    if isinstance(result, Response):
        return result
    tx_hash = result

    completion = call_llm(prompt)
    resp = Response(
        content=json.dumps({"completion": completion}),
        media_type="application/json",
    )
    resp.headers["X-PAYMENT-RESPONSE"] = tx_hash
    return resp
