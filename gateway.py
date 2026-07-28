import base64
import json

from fastapi import FastAPI, Request, Response

from llm import call_llm
from payment import (
    RESOURCE_URL,
    build_escrow_requirements,
    build_payment_requirements,
    settle_escrow_payment,
    settle_payment,
    verify_payment,
)

app = FastAPI()

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
    amount = authorization["permitted"]["amount"]

    verify_result = verify_payment(signature, authorization, amount)
    if not verify_result.get("isValid"):
        return Response(
            status_code=402,
            content=json.dumps(
                {"error": "payment invalid", "reason": verify_result.get("invalidReason")}
            ),
            media_type="application/json",
        )

    settle_result = settle_payment(signature, authorization, amount)
    if not settle_result.get("success"):
        return Response(
            status_code=402,
            content=json.dumps({"error": "settlement failed"}),
            media_type="application/json",
        )

    tx_hash = settle_result["transaction"]
    if tx_hash in SEEN_SETTLEMENTS:
        return Response(
            status_code=409,
            content=json.dumps({"error": "replay detected", "transaction": tx_hash}),
            media_type="application/json",
        )
    SEEN_SETTLEMENTS.add(tx_hash)
    return tx_hash


def _handle_escrow(payload: dict):
    signature = payload["payload"]["signature"]
    authorization = payload["payload"]["escrowAuthorization"]

    settle_result = settle_escrow_payment(signature, authorization)
    if not settle_result.get("success"):
        error = settle_result.get("error", "")
        # The contract's own revert reasons ARE the replay/expiry signal here —
        # no app-level SEEN_SETTLEMENTS set needed for this path.
        if "invalid nonce" in error:
            return Response(
                status_code=409,
                content=json.dumps({"error": "replay detected", "reason": error}),
                media_type="application/json",
            )
        return Response(
            status_code=402,
            content=json.dumps({"error": "settlement failed", "reason": error}),
            media_type="application/json",
        )
    return settle_result["transaction"]


@app.post("/infer")
async def infer(request: Request):
    body = await request.json()
    prompt = body.get("prompt", "")

    payment_header = request.headers.get("X-PAYMENT")
    if not payment_header:
        return Response(
            content=json.dumps(PAYMENT_REQUIREMENTS_402_BODY),
            status_code=402,
            media_type="application/json",
        )

    payload = json.loads(base64.b64decode(payment_header))
    method = payload["accepted"]["extra"]["assetTransferMethod"]

    if method == "permit2":
        result = _handle_permit2(payload)
    elif method == "inference-escrow":
        result = _handle_escrow(payload)
    else:
        return Response(
            status_code=402,
            content=json.dumps({"error": f"unsupported assetTransferMethod: {method}"}),
            media_type="application/json",
        )

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
