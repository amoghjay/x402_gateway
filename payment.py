import json
import os
import re
import secrets
import time
from pathlib import Path

import requests
from dotenv import load_dotenv
from eth_abi import decode as abi_decode
from eth_account import Account
from eth_account.messages import encode_typed_data
from web3 import Web3
from web3.exceptions import ContractLogicError

load_dotenv()

_ERROR_STRING_SELECTOR = bytes.fromhex("08c379a0")


def _decode_revert_reason(exc: ContractLogicError) -> str:
    """web3.py doesn't always auto-decode Solidity's Error(string) revert data
    on this RPC — it comes back as a raw hex blob inside the exception's repr.
    Pull out the hex, strip the Error(string) selector, ABI-decode the rest."""
    match = re.search(r"0x[0-9a-fA-F]+", str(exc))
    if match:
        data = bytes.fromhex(match.group(0)[2:])
        if data[:4] == _ERROR_STRING_SELECTOR:
            try:
                (reason,) = abi_decode(["string"], data[4:])
                return reason
            except Exception:
                pass
    return str(exc)

CHAIN_ID = int(os.environ["CHAIN_ID"])
PERMIT2_CONTRACT_ADDRESS = os.environ["PERMIT2_CONTRACT_ADDRESS"]
X402_PROXY_ADDRESS = os.environ["X402_PROXY_ADDRESS"]
SBC_CONTRACT_ADDRESS = os.environ["SBC_CONTRACT_ADDRESS"]
PAY_TO_ADDRESS = os.environ["PAY_TO_ADDRESS"]
WALLET_KEY = os.environ["WALLET_KEY"]
FACILITATOR_URL = os.environ["FACILITATOR_URL"]
PRICE_BASE_UNITS = os.environ["PRICE_BASE_UNITS"]
RPC_URL = os.environ["RPC_URL"]
ESCROW_CONTRACT_ADDRESS = os.environ["ESCROW_CONTRACT_ADDRESS"]
GATEWAY_OPERATOR_KEY = os.environ["GATEWAY_OPERATOR_KEY"]
RESOURCE_URL = "http://localhost:8000/infer"
PAYER_ADDRESS = Account.from_key(WALLET_KEY).address
GATEWAY_OPERATOR_ADDRESS = Account.from_key(GATEWAY_OPERATOR_KEY).address

_w3 = Web3(Web3.HTTPProvider(RPC_URL))

# Load the ABI from Foundry's artifact so it can't drift from the deployed contract.
_ESCROW_ARTIFACT = Path(__file__).parent / "contracts" / "out" / "InferenceEscrow.sol" / "InferenceEscrow.json"
ESCROW_ABI = json.loads(_ESCROW_ARTIFACT.read_text())["abi"]
_escrow = _w3.eth.contract(address=Web3.to_checksum_address(ESCROW_CONTRACT_ADDRESS), abi=ESCROW_ABI)

_ERC20_ABI = [
    {
        "type": "function",
        "name": "approve",
        "stateMutability": "nonpayable",
        "inputs": [{"name": "spender", "type": "address"}, {"name": "value", "type": "uint256"}],
        "outputs": [{"type": "bool"}],
    },
    {
        "type": "function",
        "name": "allowance",
        "stateMutability": "view",
        "inputs": [{"name": "owner", "type": "address"}, {"name": "spender", "type": "address"}],
        "outputs": [{"type": "uint256"}],
    },
    {
        "type": "function",
        "name": "balanceOf",
        "stateMutability": "view",
        "inputs": [{"name": "account", "type": "address"}],
        "outputs": [{"type": "uint256"}],
    },
]
_sbc = _w3.eth.contract(address=Web3.to_checksum_address(SBC_CONTRACT_ADDRESS), abi=_ERC20_ABI)


def sbc_balance(address: str) -> int:
    return _sbc.functions.balanceOf(Web3.to_checksum_address(address)).call()


def explorer_link(tx_hash: str) -> str:
    return f"https://testnet.radiustech.xyz/tx/{tx_hash}"


def _send(account, fn):
    tx = fn.build_transaction(
        {
            "from": account.address,
            "chainId": CHAIN_ID,
            "nonce": _w3.eth.get_transaction_count(account.address, "pending"),
            "gas": 300_000,
            "gasPrice": _w3.eth.gas_price,
        }
    )
    signed = account.sign_transaction(tx)
    tx_hash = _w3.eth.send_raw_transaction(signed.raw_transaction)
    return _w3.eth.wait_for_transaction_receipt(tx_hash, timeout=60)


def ensure_escrow_deposit(min_amount: int, top_up: int = 20_000) -> int:
    """Check the caller's InferenceEscrow balance; approve + deposit a top-up
    if it's below `min_amount`. Returns the resulting balance. This is the
    one on-chain, gas-paying step the caller does — everything after is just
    signing, same trade-off as Permit2's one-time approve(Permit2, MAX)."""
    account = Account.from_key(WALLET_KEY)
    current = _escrow.functions.balances(account.address).call()
    if current >= min_amount:
        return current

    allowance = _sbc.functions.allowance(account.address, ESCROW_CONTRACT_ADDRESS).call()
    if allowance < top_up:
        _send(account, _sbc.functions.approve(Web3.to_checksum_address(ESCROW_CONTRACT_ADDRESS), top_up))

    _send(account, _escrow.functions.deposit(top_up))
    return _escrow.functions.balances(account.address).call()


def build_payment_requirements(amount: str = PRICE_BASE_UNITS) -> dict:
    """The single `accepts[]` entry — same object used in the 402 response and
    as `paymentRequirements` in the /verify and /settle calls to the facilitator."""
    return {
        "scheme": "exact",
        "network": f"eip155:{CHAIN_ID}",
        "amount": amount,
        "payTo": PAY_TO_ADDRESS,
        "asset": SBC_CONTRACT_ADDRESS,
        "maxTimeoutSeconds": 300,
        "extra": {
            "assetTransferMethod": "permit2",
            "name": "Stable Coin",
            "version": "1",
        },
    }


def sign_permit2_payment(amount: int, deadline_seconds: int = 300) -> tuple[str, dict]:
    """Sign a Permit2 PermitWitnessTransferFrom authorizing `amount` of SBC to PAY_TO_ADDRESS.

    Returns (signature_hex, authorization_dict), where authorization_dict is the
    `permit2Authorization` shape the facilitator's /verify and /settle expect
    inside paymentPayload.payload.
    """
    account = Account.from_key(WALLET_KEY)

    domain = {
        "name": "Permit2",
        "chainId": CHAIN_ID,
        "verifyingContract": PERMIT2_CONTRACT_ADDRESS,
    }

    types = {
        "PermitWitnessTransferFrom": [
            {"name": "permitted", "type": "TokenPermissions"},
            {"name": "spender", "type": "address"},
            {"name": "nonce", "type": "uint256"},
            {"name": "deadline", "type": "uint256"},
            {"name": "witness", "type": "Witness"},
        ],
        "TokenPermissions": [
            {"name": "token", "type": "address"},
            {"name": "amount", "type": "uint256"},
        ],
        "Witness": [
            {"name": "to", "type": "address"},
            {"name": "validAfter", "type": "uint256"},
        ],
    }

    nonce = int.from_bytes(secrets.token_bytes(32), "big")
    deadline = int(time.time()) + deadline_seconds

    message = {
        "permitted": {"token": SBC_CONTRACT_ADDRESS, "amount": amount},
        "spender": X402_PROXY_ADDRESS,
        "nonce": nonce,
        "deadline": deadline,
        "witness": {"to": PAY_TO_ADDRESS, "validAfter": 0},
    }

    signable = encode_typed_data(domain_data=domain, message_types=types, message_data=message)
    signed = account.sign_message(signable)

    # hexbytes .hex() is not 0x-prefixed on this version.
    signature_hex = "0x" + signed.signature.hex()
    authorization = {
        "permitted": {"token": SBC_CONTRACT_ADDRESS, "amount": str(amount)},
        "from": account.address,
        "spender": X402_PROXY_ADDRESS,
        "nonce": str(nonce),
        "deadline": str(deadline),
        "witness": {"to": PAY_TO_ADDRESS, "validAfter": "0"},
    }
    return signature_hex, authorization


def _build_facilitator_request(signature: str, authorization: dict) -> dict:
    """Shared body shape for both /verify and /settle. `paymentRequirements` takes
    no caller-supplied amount by design — it must come from our own config, never
    be echoed back from the client's payload. See REPORT.md §10."""
    requirements = build_payment_requirements()
    payment_payload = {
        "x402Version": 2,
        "resource": {
            "url": RESOURCE_URL,
            "description": "One LLM inference call",
            "mimeType": "application/json",
        },
        "accepted": requirements,
        "payload": {
            "signature": signature,
            "permit2Authorization": authorization,
        },
    }
    return {
        "x402Version": 2,
        "paymentPayload": payment_payload,
        "paymentRequirements": requirements,
    }


def verify_payment(signature: str, authorization: dict) -> dict:
    """POST to the facilitator's /verify. Validity is signaled by `isValid` in
    the response BODY, not by HTTP status — a bad signature still returns 200."""
    body = _build_facilitator_request(signature, authorization)
    resp = requests.post(f"{FACILITATOR_URL}/verify", json=body, timeout=10)
    resp.raise_for_status()
    return resp.json()


def settle_payment(signature: str, authorization: dict) -> dict:
    """POST to the facilitator's /settle. On success, `transaction` is the
    on-chain settlement tx hash. A replayed payload returns the SAME cached
    tx hash rather than settling again — the facilitator is idempotent, so
    the gateway's own replay guard (not this call) is what must catch reuse."""
    body = _build_facilitator_request(signature, authorization)
    resp = requests.post(f"{FACILITATOR_URL}/settle", json=body, timeout=10)
    resp.raise_for_status()
    return resp.json()


# --- Milestone 2: InferenceEscrow — deposit once, sign+settle per prompt ---


def build_escrow_requirements(amount: str = PRICE_BASE_UNITS) -> dict:
    """The second `accepts[]` entry — same product, settled via the gateway's
    own contract instead of the Radius facilitator. Note there's no `payTo`
    beyond the contract address itself: InferenceEscrow's `provider` is
    immutable, set once at deploy, not chosen per-request."""
    return {
        "scheme": "exact",
        "network": f"eip155:{CHAIN_ID}",
        "amount": amount,
        "payTo": ESCROW_CONTRACT_ADDRESS,
        "asset": SBC_CONTRACT_ADDRESS,
        "maxTimeoutSeconds": 300,
        "extra": {
            "assetTransferMethod": "inference-escrow",
            "contractAddress": ESCROW_CONTRACT_ADDRESS,
        },
    }


def sign_escrow_authorization(amount: int, deadline_seconds: int = 300) -> tuple[str, dict]:
    """Sign an InferenceEscrow Authorization{settler, amount, nonce, deadline}.
    Requires the caller has already deposit()-ed enough balance into the contract —
    this only authorizes a draw-down against that existing deposit, it does
    not move any funds itself.

    `settler` names the only address allowed to submit this authorization, so a
    third party who obtains the signature cannot redeem it. Nonces are random and
    unordered (like the Permit2 path), so concurrent prompts can't collide — and
    no on-chain read is needed before signing."""
    account = Account.from_key(WALLET_KEY)
    nonce = int.from_bytes(secrets.token_bytes(32), "big")
    deadline = int(time.time()) + deadline_seconds

    domain = {
        "name": "InferenceEscrow",
        "version": "1",
        "chainId": CHAIN_ID,
        "verifyingContract": ESCROW_CONTRACT_ADDRESS,
    }
    types = {
        "Authorization": [
            {"name": "settler", "type": "address"},
            {"name": "amount", "type": "uint256"},
            {"name": "nonce", "type": "uint256"},
            {"name": "deadline", "type": "uint256"},
        ],
    }
    message = {
        "settler": GATEWAY_OPERATOR_ADDRESS,
        "amount": amount,
        "nonce": nonce,
        "deadline": deadline,
    }

    signable = encode_typed_data(domain_data=domain, message_types=types, message_data=message)
    signed = account.sign_message(signable)

    signature_hex = "0x" + signed.signature.hex()
    authorization = {
        "settler": GATEWAY_OPERATOR_ADDRESS,
        "amount": str(amount),
        "nonce": str(nonce),
        "deadline": str(deadline),
    }
    return signature_hex, authorization


def _escrow_settle_fn(signature: str, authorization: dict):
    auth_tuple = (
        Web3.to_checksum_address(authorization["settler"]),
        int(authorization["amount"]),
        int(authorization["nonce"]),
        int(authorization["deadline"]),
    )
    return _escrow.functions.settle(auth_tuple, bytes.fromhex(signature[2:]))


def simulate_escrow_settlement(signature: str, authorization: dict) -> dict:
    """Free pre-flight — this path's equivalent of the facilitator's /verify.
    Asks the EVM whether settle() *would* succeed without spending any gas, so a
    bad nonce / expired deadline / insufficient balance is a structured error
    instead of a doomed transaction. Also lets the gateway confirm the payment is
    good BEFORE doing the work it's charging for (REPORT.md §10.3)."""
    account = Account.from_key(GATEWAY_OPERATOR_KEY)
    try:
        _escrow_settle_fn(signature, authorization).call({"from": account.address})
    except ContractLogicError as exc:
        return {"ok": False, "error": _decode_revert_reason(exc)}
    return {"ok": True}


def submit_escrow_settlement(signature: str, authorization: dict) -> dict:
    """Actually settle on-chain — the gateway IS the facilitator for this path.
    Uses GATEWAY_OPERATOR_KEY, a separate wallet from the payer's WALLET_KEY, so
    msg.sender here is genuinely not the payer (and must equal auth.settler)."""
    account = Account.from_key(GATEWAY_OPERATOR_KEY)
    receipt = _send(account, _escrow_settle_fn(signature, authorization))
    return {"success": receipt.status == 1, "transaction": Web3.to_hex(receipt.transactionHash)}


def settle_escrow_payment(signature: str, authorization: dict) -> dict:
    """Simulate-then-submit convenience wrapper for scripts. The gateway itself
    drives the two phases separately so it can serve the inference in between."""
    simulated = simulate_escrow_settlement(signature, authorization)
    if not simulated["ok"]:
        return {"success": False, "error": simulated["error"]}
    return submit_escrow_settlement(signature, authorization)
