"""Guard the one invariant we gave up by committing a generated file.

payment.py used to read the escrow ABI straight out of Foundry's `out/` directory,
which made drift impossible but also made a fresh clone unrunnable — `out/` is build
output and is not tracked, so importing payment.py raised FileNotFoundError before
uvicorn ever bound.

Committing `contracts/abi/InferenceEscrow.json` fixes the clone and reintroduces the
drift risk. This test is the replacement guarantee: edit the contract, forget to run
`contracts/sync-abi.sh`, and CI fails here instead of the gateway encoding calls
against an interface the deployed contract no longer has.

Same shape as `hack/verify-codegen.sh` in Kubernetes and `make manifests` in
kubebuilder — regenerate, diff against what's committed, fail on mismatch.
"""

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
        "contracts/abi/InferenceEscrow.json is stale — run contracts/sync-abi.sh "
        "and commit the result."
    )


def test_payment_module_loads_the_committed_abi():
    """The relocation is only correct if the ABI is actually usable: a bare array
    of entries covering the functions the gateway calls. Guards against someone
    restoring the full Foundry artifact here, which would load but expose no
    functions (the entries would sit under an "abi" key)."""
    abi = json.loads(COMMITTED_ABI.read_text())

    assert isinstance(abi, list), "expected a bare ABI array, not a Foundry artifact"

    functions = {entry["name"] for entry in abi if entry.get("type") == "function"}
    assert {"settle", "deposit", "balances", "withdraw"} <= functions
