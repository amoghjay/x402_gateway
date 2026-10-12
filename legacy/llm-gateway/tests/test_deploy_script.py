"""Deploy.s.sol exists because the live contract was created by hand. This runs it
against anvil and reads the result back through the committed ABI, so CI covers both
the script and the ABI at once.
"""

import json
import os
import re
import shutil
import socket
import subprocess
import time
from pathlib import Path

import pytest
from web3 import Web3

CONTRACTS = Path(__file__).parent.parent / "contracts"
ABI = json.loads((CONTRACTS / "abi" / "InferenceEscrow.json").read_text())

ANVIL_KEY = "0xac0974bec39a17e36ba4a6b4d238ff944bacb478cbed5efcae784d7bf4f2ff80"
SBC = "0x33ad9e4BD16B69B5BFdED37D8B5D9fF9aba014Fb"
PROVIDER = "0xbD5fdCde255Abb883cB0C3137037cAef28ed10ac"

pytestmark = pytest.mark.skipif(
    shutil.which("forge") is None or shutil.which("anvil") is None,
    reason="foundry not installed",
)


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture
def anvil():
    port = _free_port()
    proc = subprocess.Popen(
        ["anvil", "--silent", "--port", str(port)],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    url = f"http://127.0.0.1:{port}"
    w3 = Web3(Web3.HTTPProvider(url, request_kwargs={"timeout": 2}))
    try:
        for _ in range(100):
            if w3.is_connected():
                break
            time.sleep(0.1)
        else:
            pytest.fail("anvil never became reachable")
        yield url, w3
    finally:
        proc.terminate()
        proc.wait(timeout=10)


def test_deploy_script_sets_constructor_args_from_env(anvil):
    url, w3 = anvil

    result = subprocess.run(
        ["forge", "script", "script/Deploy.s.sol",
         "--rpc-url", url, "--private-key", ANVIL_KEY, "--broadcast"],
        cwd=CONTRACTS,
        capture_output=True,
        text=True,
        env={"PATH": os.environ["PATH"],
             "HOME": os.environ.get("HOME", ""),
             "SBC_CONTRACT_ADDRESS": SBC,
             "PAY_TO_ADDRESS": PROVIDER},
    )
    assert result.returncode == 0, result.stderr[-2000:]

    match = re.search(r"InferenceEscrow deployed:\s*(0x[0-9a-fA-F]{40})", result.stdout)
    assert match, f"no address in output:\n{result.stdout[-2000:]}"

    escrow = w3.eth.contract(address=Web3.to_checksum_address(match.group(1)), abi=ABI)
    assert escrow.functions.token().call() == SBC
    assert escrow.functions.provider().call() == PROVIDER

    # Same domain separator as the live deployment, so payment.py's signatures
    # verify against a contract this script produces.
    assert escrow.functions.AUTHORIZATION_TYPEHASH().call().hex() == (
        "2618256e7d8d648525cec86b5a63aec9b65456344b6d3f45b7ee03e4c3c20559"
    )
