"""The same four attack classes security_probes.py fires at a live gateway, run
hermetically: underpayment, replay, malformed envelopes, and provider outage.

On-chain assertions stay in `forge test` and in the live probe run. What is checked
here is the gateway's state machine — that nothing serves a completion it was not
paid for, and that no attacker input produces a 5xx.
"""

import base64
import json

import pytest
import requests
from fastapi.testclient import TestClient

import gateway

PRICE = 1000
TX = "0x" + "ab" * 32


@pytest.fixture
def client():
    gateway.SEEN_SIGNATURES.clear()
    gateway.SEEN_SETTLEMENTS.clear()
    return TestClient(gateway.app)


@pytest.fixture
def paid(monkeypatch):
    """A gateway where validation, inference and settlement all succeed."""
    calls = {"settled": 0, "llm": 0}

    def _settle_payment(signature, authorization):
        calls["settled"] += 1
        return {"success": True, "transaction": TX}

    def _submit_escrow(signature, authorization):
        calls["settled"] += 1
        return {"success": True, "transaction": TX, "gas_used": 118_000}

    def _llm(prompt):
        calls["llm"] += 1
        return "a mutex is a lock"

    monkeypatch.setattr(gateway, "verify_payment", lambda s, a: {"isValid": True})
    monkeypatch.setattr(gateway, "settle_payment", _settle_payment)
    monkeypatch.setattr(gateway, "simulate_escrow_settlement", lambda s, a: {"ok": True})
    monkeypatch.setattr(gateway, "submit_escrow_settlement", _submit_escrow)
    monkeypatch.setattr(gateway, "call_llm", _llm)
    return calls


def header(scheme="permit2", amount=PRICE, signature="0xfeed"):
    auth = {"amount": str(amount)}
    payload = {
        "accepted": {"extra": {"assetTransferMethod": scheme}},
        "payload": {
            "signature": signature,
            "permit2Authorization": {"permitted": {"amount": str(amount)}},
            "escrowAuthorization": auth,
        },
    }
    return {"X-PAYMENT": base64.b64encode(json.dumps(payload).encode()).decode()}


SCHEMES = ["permit2", "inference-escrow"]


def test_no_payment_header_challenges_with_both_schemes(client):
    response = client.post("/infer", json={"prompt": "hi"})
    assert response.status_code == 402

    body = response.json()
    assert [a["extra"]["assetTransferMethod"] for a in body["accepts"]] == SCHEMES
    assert all(a["amount"] == str(PRICE) for a in body["accepts"])
    assert "completion" not in response.text


def test_every_response_carries_a_correlation_id(client):
    response = client.post("/infer", json={"prompt": "hi"})
    assert len(response.headers["X-Payment-Id"]) == 16


# --- probe 3: malformed envelopes must never be a 5xx ------------------------
@pytest.mark.parametrize(
    "label,value",
    [
        ("not base64", "not-base64!!"),
        ("not json", base64.b64encode(b"not json").decode()),
        ("missing keys", base64.b64encode(json.dumps({"accepted": {}}).encode()).decode()),
        (
            "non-numeric amount",
            base64.b64encode(
                json.dumps(
                    {
                        "accepted": {"extra": {"assetTransferMethod": "inference-escrow"}},
                        "payload": {"signature": "0xdead", "escrowAuthorization": {"amount": "abc"}},
                    }
                ).encode()
            ).decode(),
        ),
        ("empty", ""),
        ("unsupported scheme", base64.b64encode(
            json.dumps({"accepted": {"extra": {"assetTransferMethod": "dogecoin"}}, "payload": {}}).encode()
        ).decode()),
    ],
)
def test_malformed_envelope_is_4xx_not_5xx(client, label, value):
    response = client.post("/infer", json={"prompt": "x"}, headers={"X-PAYMENT": value})
    assert 400 <= response.status_code < 500, f"{label}: got {response.status_code}"
    assert "completion" not in response.text


@pytest.mark.parametrize("body", ["{bad json", "[1,2,3]", '"a string"'])
def test_malformed_request_body_is_4xx_not_5xx(client, body):
    response = client.post("/infer", content=body, headers={"Content-Type": "application/json"})
    assert 400 <= response.status_code < 500


# --- probe 1: underpayment ---------------------------------------------------
@pytest.mark.parametrize("scheme", SCHEMES)
def test_underpayment_rejected_without_serving(client, paid, scheme):
    response = client.post("/infer", json={"prompt": "free lunch?"}, headers=header(scheme, amount=1))
    assert response.status_code == 402
    assert response.json()["error"] == "underpayment"
    assert "completion" not in response.text
    assert paid["llm"] == 0, "inference ran before the price check"
    assert paid["settled"] == 0


@pytest.mark.parametrize("scheme", SCHEMES)
def test_exact_price_is_accepted(client, paid, scheme):
    response = client.post("/infer", json={"prompt": "hi"}, headers=header(scheme, amount=PRICE))
    assert response.status_code == 200
    assert response.json()["completion"] == "a mutex is a lock"
    assert response.headers["X-PAYMENT-RESPONSE"] == TX
    assert paid["settled"] == 1


# --- probe 1b: replay --------------------------------------------------------
def test_replayed_permit2_signature_is_rejected(client, paid):
    first = client.post("/infer", json={"prompt": "hi"}, headers=header(signature="0xreuse"))
    assert first.status_code == 200

    second = client.post("/infer", json={"prompt": "hi"}, headers=header(signature="0xreuse"))
    assert second.status_code == 409
    assert "completion" not in second.text
    assert paid["settled"] == 1, "replay reached settlement"
    assert paid["llm"] == 1, "replay burned an inference"


def test_escrow_replay_detected_in_simulation(client, paid, monkeypatch):
    monkeypatch.setattr(
        gateway, "simulate_escrow_settlement",
        lambda s, a: {"ok": False, "error": "nonce already used"},
    )
    response = client.post("/infer", json={"prompt": "hi"}, headers=header("inference-escrow"))
    assert response.status_code == 409
    assert paid["llm"] == 0, "validation is supposed to be free"


# --- probe 4: provider outage ------------------------------------------------
def test_provider_outage_withholds_completion_and_does_not_settle(client, paid, monkeypatch):
    def _boom(prompt):
        raise gateway.InferenceError("groq unreachable")

    monkeypatch.setattr(gateway, "call_llm", _boom)

    response = client.post("/infer", json={"prompt": "hi"}, headers=header())
    assert response.status_code == 502
    assert response.json()["charged"] is False
    assert "completion" not in response.text
    assert paid["settled"] == 0, "charged for an inference that never happened"


# --- dependency failures must not be 500s -----------------------------------
def test_facilitator_4xx_is_a_payment_error_not_a_server_error(client, paid, monkeypatch):
    response_400 = requests.Response()
    response_400.status_code = 400

    def _raise(s, a):
        raise requests.HTTPError("400 Client Error", response=response_400)

    monkeypatch.setattr(gateway, "verify_payment", _raise)

    response = client.post("/infer", json={"prompt": "hi"}, headers=header())
    assert response.status_code == 402
    assert response.json()["error"] == "payment invalid"
    assert paid["llm"] == 0


def test_facilitator_unreachable_is_503(client, paid, monkeypatch):
    def _raise(s, a):
        raise requests.ConnectionError("connection refused")

    monkeypatch.setattr(gateway, "verify_payment", _raise)

    response = client.post("/infer", json={"prompt": "hi"}, headers=header())
    assert response.status_code == 503
    assert paid["llm"] == 0


def test_settlement_failure_withholds_the_completion(client, paid, monkeypatch):
    def _raise(s, a):
        raise requests.ConnectionError("facilitator died mid-settle")

    monkeypatch.setattr(gateway, "settle_payment", _raise)

    response = client.post("/infer", json={"prompt": "hi"}, headers=header())
    assert response.status_code == 502
    assert "completion" not in response.text, "served unpaid after settlement failed"
    # No `charged` claim: the tx may still land.
    assert "charged" not in response.json()
