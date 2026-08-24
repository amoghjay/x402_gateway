import base64
import json
import time
import uuid

import requests
import structlog
from fastapi import FastAPI, Request, Response
from prometheus_client import CONTENT_TYPE_LATEST, generate_latest
from web3.exceptions import Web3Exception

from config import settings
from llm import InferenceError, call_llm
from observability import (
    LLM_CALLS,
    PAYMENT_REQUIRED,
    PHASE_SECONDS,
    SETTLEMENT_GAS,
    SETTLEMENTS,
    VALIDATIONS,
    configure_logging,
    log,
)
from payment import (
    build_escrow_requirements,
    build_payment_requirements,
    facilitator_reachable,
    rpc_reachable,
    settle_payment,
    simulate_escrow_settlement,
    submit_escrow_settlement,
    verify_payment,
)

configure_logging()
app = FastAPI()


@app.middleware("http")
async def _correlate(request: Request, call_next):
    """One id per request, bound for every log line in it and returned to the caller
    so a payer can quote it when a settlement is disputed."""
    structlog.contextvars.clear_contextvars()
    payment_id = uuid.uuid4().hex[:16]
    structlog.contextvars.bind_contextvars(payment_id=payment_id)
    response = await call_next(request)
    response.headers["X-Payment-Id"] = payment_id
    return response


def _json(status: int, /, **body) -> Response:
    # Positional-only: otherwise a body field named "status" collides with the arg.
    return Response(status_code=status, content=json.dumps(body), media_type="application/json")


def _underpaid(scheme: str, signed_amount: str) -> Response | None:
    """int() the client's side: it arrives as a wire string, and "9" >= "1000" is
    True lexicographically. The configured price is already an int."""
    if int(signed_amount) < settings.price_base_units:
        return _rejected(
            scheme,
            "underpayment",
            402,
            error="underpayment",
            reason=f"signed amount {signed_amount} is less than required {settings.price_base_units}",
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
        "url": settings.resource_url,
        "description": "One LLM inference call",
        "mimeType": "application/json",
    },
    "accepts": [build_payment_requirements(), build_escrow_requirements()],
}


def _rejected(scheme: str, outcome: str, status: int, **body) -> Response:
    VALIDATIONS.labels(scheme=scheme, outcome=outcome).inc()
    log.info("validation_rejected", scheme=scheme, outcome=outcome, status=status)
    return _json(status, **body)


def _prepare_permit2(payload: dict):
    """Validate without taking any money. Returns (error_response, settle_fn)."""
    scheme = "permit2"
    signature = payload["payload"]["signature"]
    authorization = payload["payload"]["permit2Authorization"]

    if (underpaid := _underpaid(scheme, authorization["permitted"]["amount"])) is not None:
        return underpaid, None

    if signature in SEEN_SIGNATURES:
        return _rejected(
            scheme, "replay", 409,
            error="replay detected", reason="payment signature already used",
        ), None

    verify_result = verify_payment(signature, authorization)
    if not verify_result.get("isValid"):
        return _rejected(
            scheme, "invalid", 402,
            error="payment invalid", reason=verify_result.get("invalidReason"),
        ), None

    VALIDATIONS.labels(scheme=scheme, outcome="valid").inc()

    def settle():
        settle_result = settle_payment(signature, authorization)
        if not settle_result.get("success"):
            SETTLEMENTS.labels(scheme=scheme, outcome="rejected").inc()
            return _json(402, error="settlement failed"), None

        tx_hash = settle_result["transaction"]
        if tx_hash in SEEN_SETTLEMENTS:
            SETTLEMENTS.labels(scheme=scheme, outcome="replay").inc()
            return _json(409, error="replay detected", transaction=tx_hash), None
        SEEN_SETTLEMENTS.add(tx_hash)
        SEEN_SIGNATURES.add(signature)
        SETTLEMENTS.labels(scheme=scheme, outcome="settled").inc()
        log.info("settled", scheme=scheme, transaction=tx_hash)
        return None, tx_hash

    return None, settle


def _prepare_escrow(payload: dict):
    """Validate for free, take nothing. Returns (error_response, settle_fn).
    The simulation catches everything the real tx would, so no app-level state."""
    scheme = "inference-escrow"
    signature = payload["payload"]["signature"]
    authorization = payload["payload"]["escrowAuthorization"]

    if (underpaid := _underpaid(scheme, authorization["amount"])) is not None:
        return underpaid, None

    simulated = simulate_escrow_settlement(signature, authorization)
    if not simulated["ok"]:
        error = simulated["error"]
        if "nonce already used" in error:
            return _rejected(scheme, "replay", 409, error="replay detected", reason=error), None
        return _rejected(scheme, "invalid", 402, error="settlement failed", reason=error), None

    VALIDATIONS.labels(scheme=scheme, outcome="valid").inc()

    def settle():
        settle_result = submit_escrow_settlement(signature, authorization)
        if not settle_result.get("success"):
            SETTLEMENTS.labels(scheme=scheme, outcome="reverted").inc()
            return _json(402, error="settlement failed", reason="transaction reverted"), None
        SETTLEMENTS.labels(scheme=scheme, outcome="settled").inc()
        SETTLEMENT_GAS.labels(scheme=scheme).observe(settle_result["gas_used"])
        log.info(
            "settled",
            scheme=scheme,
            transaction=settle_result["transaction"],
            gas_used=settle_result["gas_used"],
        )
        return None, settle_result["transaction"]

    return None, settle


@app.post("/infer")
async def infer(request: Request):
    # The body is attacker-controlled too: malformed JSON is a 400, not a 500.
    try:
        body = await request.json()
    except Exception as exc:
        return _json(400, error="malformed request body", reason=f"{type(exc).__name__}: {exc}")
    if not isinstance(body, dict):
        return _json(400, error="malformed request body", reason="expected a JSON object")
    prompt = body.get("prompt", "")

    payment_header = request.headers.get("X-PAYMENT")
    if not payment_header:
        PAYMENT_REQUIRED.inc()
        log.info("payment_required")
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
        # Fixed label, never `method`: the client picks that string, and unbounded
        # label values are a cardinality attack on the metrics registry. Past this
        # point `method` is one of the two known schemes.
        VALIDATIONS.labels(scheme="unknown", outcome="unsupported").inc()
        log.info("unsupported_scheme", requested=method)
        return _json(402, error=f"unsupported assetTransferMethod: {method}")

    structlog.contextvars.bind_contextvars(scheme=method)

    # Phase 1: validate for free. Preparers index client dicts / int() client
    # strings, so garbage raises here too.
    phase = time.perf_counter()
    try:
        error, settle = prepare(payload)
    except (KeyError, TypeError, ValueError) as exc:
        VALIDATIONS.labels(scheme=method, outcome="malformed").inc()
        return _json(400, error="malformed payment payload", reason=f"{type(exc).__name__}: {exc}")
    except requests.HTTPError as exc:
        # verify_payment/settle_payment call raise_for_status(). A 4xx means the
        # facilitator rejected the payer's payload; anything else is our dependency
        # failing. Both used to surface as a 500.
        status = exc.response.status_code if exc.response is not None else None
        if status is not None and 400 <= status < 500:
            VALIDATIONS.labels(scheme=method, outcome="invalid").inc()
            return _json(402, error="payment invalid", reason=f"facilitator rejected payload ({status})")
        VALIDATIONS.labels(scheme=method, outcome="unavailable").inc()
        log.warning("facilitator_error", status=status)
        return _json(503, error="validation unavailable", reason=f"facilitator error ({status})")
    except (requests.RequestException, Web3Exception) as exc:
        VALIDATIONS.labels(scheme=method, outcome="unavailable").inc()
        log.warning("validation_unavailable", error=str(exc))
        return _json(503, error="validation unavailable", reason=f"{type(exc).__name__}: {exc}")
    finally:
        PHASE_SECONDS.labels(phase="validate").observe(time.perf_counter() - phase)
    if error is not None:
        return error

    # Phase 2: produce the goods BEFORE charging, so a provider outage costs the
    # payer nothing. See REPORT.md §10.3.
    phase = time.perf_counter()
    try:
        completion = call_llm(prompt)
        LLM_CALLS.labels(outcome="ok").inc()
    except InferenceError as exc:
        LLM_CALLS.labels(outcome="failed").inc()
        log.warning("inference_failed", error=str(exc))
        return _json(502, error="inference failed", reason=str(exc), charged=False)
    finally:
        PHASE_SECONDS.labels(phase="inference").observe(time.perf_counter() - phase)

    # Phase 3: settle. On failure we absorb one inference rather than serve unpaid.
    # No `charged` claim here, unlike phase 2: a receipt timeout (Web3 TimeExhausted)
    # means the tx may still land, so we genuinely do not know.
    phase = time.perf_counter()
    try:
        error, tx_hash = settle()
    except (requests.RequestException, Web3Exception) as exc:
        SETTLEMENTS.labels(scheme=method, outcome="error").inc()
        log.error("settlement_error", error=str(exc))
        return _json(502, error="settlement failed", reason=f"{type(exc).__name__}: {exc}")
    finally:
        PHASE_SECONDS.labels(phase="settle").observe(time.perf_counter() - phase)
    if error is not None:
        return error

    resp = Response(
        content=json.dumps({"completion": completion}),
        media_type="application/json",
    )
    resp.headers["X-PAYMENT-RESPONSE"] = tx_hash
    return resp


@app.get("/healthz")
async def healthz():
    """Liveness only: the process is serving. Deliberately touches no dependency —
    a probe that fails when the RPC blips restarts a pod that was working."""
    return _json(200, status="ok")


@app.get("/readyz")
async def readyz():
    """Readiness is per-path, because the two paths fail independently: Path A needs
    the facilitator, Path B needs our RPC. Ready if either can still settle."""
    rpc = rpc_reachable()
    facilitator = facilitator_reachable()
    ready = rpc or facilitator
    if not ready:
        log.error("not_ready", rpc=rpc, facilitator=facilitator)
    return _json(
        200 if ready else 503,
        status="ready" if ready else "unready",
        rpc=rpc,
        facilitator=facilitator,
        schemes_available=[
            s for s, ok in (("permit2", facilitator), ("inference-escrow", rpc)) if ok
        ],
    )


@app.get("/metrics")
async def metrics():
    return Response(content=generate_latest(), media_type=CONTENT_TYPE_LATEST)
