import asyncio
import json

import gateway


def _body(response):
    return json.loads(response.body)


def _run(coro):
    return asyncio.run(coro)


def test_healthz_touches_no_dependency(monkeypatch):
    """Liveness must not fail on a dependency blip, or a working pod gets restarted."""
    monkeypatch.setattr(gateway, "rpc_reachable", lambda: 1 / 0)
    monkeypatch.setattr(gateway, "facilitator_reachable", lambda: 1 / 0)

    response = _run(gateway.healthz())
    assert response.status_code == 200


def test_json_helper_allows_a_status_field_in_the_body():
    """_json's first arg is positional-only; a body key named "status" used to raise
    TypeError and surface as a 500."""
    response = gateway._json(200, status="ok")
    assert response.status_code == 200
    assert _body(response) == {"status": "ok"}


def test_readyz_reports_per_path_availability(monkeypatch):
    cases = [
        (True, True, 200, ["permit2", "inference-escrow"]),
        (True, False, 200, ["inference-escrow"]),  # Path B alone still settles
        (False, True, 200, ["permit2"]),           # Path A alone still settles
        (False, False, 503, []),
    ]
    for rpc, facilitator, expected_status, expected_schemes in cases:
        monkeypatch.setattr(gateway, "rpc_reachable", lambda r=rpc: r)
        monkeypatch.setattr(gateway, "facilitator_reachable", lambda f=facilitator: f)

        response = _run(gateway.readyz())
        body = _body(response)
        assert response.status_code == expected_status, (rpc, facilitator)
        assert body["schemes_available"] == expected_schemes, (rpc, facilitator)
