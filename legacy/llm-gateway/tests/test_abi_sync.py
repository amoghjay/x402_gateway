"""payment.py reads a committed ABI, so it can go stale. Fail here, not at runtime."""

import json
import shutil
import subprocess
from pathlib import Path

import pytest

CONTRACTS_DIR = Path(__file__).parent.parent / "contracts"
COMMITTED_ABI = CONTRACTS_DIR / "abi" / "InferenceEscrow.json"


@pytest.mark.skipif(shutil.which("forge") is None, reason="forge not installed")
def test_committed_abi_matches_contract_source():
    regenerated = subprocess.run(
        ["forge", "inspect", "InferenceEscrow", "abi", "--json"],
        cwd=CONTRACTS_DIR,
        capture_output=True,
        text=True,
        check=True,
    )
    assert json.loads(regenerated.stdout) == json.loads(COMMITTED_ABI.read_text()), (
        "stale ABI — run contracts/sync-abi.sh and commit the result"
    )


def test_committed_abi_is_a_bare_array():
    """Guards against restoring the full Foundry artifact, which loads but exposes
    no functions."""
    abi = json.loads(COMMITTED_ABI.read_text())
    assert isinstance(abi, list)

    functions = {e["name"] for e in abi if e.get("type") == "function"}
    assert {"settle", "deposit", "balances", "withdraw"} <= functions
