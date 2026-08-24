import json
import re
import secrets
import threading
import time
from pathlib import Path

import requests
from eth_abi import decode as abi_decode
from eth_account import Account
from eth_account.messages import encode_typed_data
from web3 import Web3
from web3.exceptions import ContractLogicError

from config import settings

_ERROR_STRING_SELECTOR = bytes.fromhex("08c379a0")


def _decode_revert_reason(exc: ContractLogicError) -> str:
    """This RPC returns Error(string) revert data as a raw hex blob in the
    exception repr rather than decoding it, so unwrap it by hand."""
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

_w3 = Web3(Web3.HTTPProvider(settings.rpc_url))


def _payer_account():
    return Account.from_key(settings.wallet_key.get_secret_value())


def _operator_account():
    """Submits settle(). Must differ from the payer: the escrow requires
    msg.sender == auth.settler."""
    return Account.from_key(settings.gateway_operator_key.get_secret_value())

# Committed, not read from the untracked out/ dir. Regenerate: contracts/sync-abi.sh
# (tests/test_abi_sync.py fails on drift). Addresses are checksummed by config.
_ESCROW_ABI_PATH = Path(__file__).parent / "contracts" / "abi" / "InferenceEscrow.json"
ESCROW_ABI = json.loads(_ESCROW_ABI_PATH.read_text())
_escrow = _w3.eth.contract(address=settings.escrow_contract_address, abi=ESCROW_ABI)

# Hand-written, unlike ESCROW_ABI: ERC-20 is frozen and external, and we use 3 calls.
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
_sbc = _w3.eth.contract(address=settings.sbc_contract_address, abi=_ERC20_ABI)


def sbc_balance(address: str) -> int:
    return _sbc.functions.balanceOf(Web3.to_checksum_address(address)).call()


def native_balance(address: str) -> int:
    """Gas is paid in the chain's native currency, NOT in SBC — so this is the
    balance that moves when the operator submits settle()."""
    return _w3.eth.get_balance(Web3.to_checksum_address(address))


def explorer_link(tx_hash: str) -> str:
    return f"{settings.explorer_base_url}/tx/{tx_hash}"


_nonce_lock = threading.Lock()
_next_nonce: dict[str, int] = {}


def _send(account, fn):
    # Reading the "pending" nonce per request races: two concurrent sends pick the
    # same number and one is dropped. Serialise assignment, and hold the lock only
    # until the tx is broadcast — waiting for the receipt under it would cap
    # throughput at one settlement per block. Per-process only: two workers sharing
    # one key still collide.
    address = account.address
    with _nonce_lock:
        nonce = max(_w3.eth.get_transaction_count(address, "pending"), _next_nonce.get(address, 0))
        _next_nonce[address] = nonce + 1
        try:
            tx = fn.build_transaction(
                {
                    "from": address,
                    "chainId": settings.chain_id,
                    "nonce": nonce,
                    "gas": settings.settlement_gas_limit,
                    "gasPrice": _w3.eth.gas_price,
                }
            )
            signed = account.sign_transaction(tx)
            tx_hash = _w3.eth.send_raw_transaction(signed.raw_transaction)
        except Exception:
            # Unused nonce: drop local state so the next call resyncs from chain
            # rather than leaving a gap that stalls every later tx.
            _next_nonce.pop(address, None)
            raise

    return _w3.eth.wait_for_transaction_receipt(
        tx_hash,
        timeout=settings.receipt_timeout_seconds,
        poll_latency=settings.receipt_poll_latency_seconds,
    )


def deposit_to_escrow(amount: int) -> dict:
    """Fund the payer's tab, returning the tx hash so a caller can show the receipt.
    The only gas-paying step the payer makes — everything after is just signing."""
    account = _payer_account()

    allowance = _sbc.functions.allowance(account.address, settings.escrow_contract_address).call()
    if allowance < amount:
        _send(account, _sbc.functions.approve(Web3.to_checksum_address(settings.escrow_contract_address), amount))

    receipt = _send(account, _escrow.functions.deposit(amount))
    return {
        "transaction": Web3.to_hex(receipt.transactionHash),
        "balance": _escrow.functions.balances(account.address).call(),
    }


def ensure_escrow_deposit(min_amount: int, top_up: int = 20_000) -> int:
    """Idempotent variant: top up only if below `min_amount`. Returns the balance."""
    account = _payer_account()
    current = _escrow.functions.balances(account.address).call()
    if current >= min_amount:
        return current
    return deposit_to_escrow(top_up)["balance"]


def build_payment_requirements(amount: int | None = None) -> dict:
    """One `accepts[]` entry, reused as `paymentRequirements` for the facilitator.
    int in, decimal string out — JSON cannot hold a uint256."""
    return {
        "scheme": "exact",
        "network": f"eip155:{settings.chain_id}",
        "amount": str(settings.price_base_units if amount is None else amount),
        "payTo": settings.pay_to_address,
        "asset": settings.sbc_contract_address,
        "maxTimeoutSeconds": 300,
        "extra": {
            "assetTransferMethod": "permit2",
            "name": "Stable Coin",
            "version": "1",
        },
    }


def sign_permit2_payment(amount: int, deadline_seconds: int = 300) -> tuple[str, dict]:
    """Sign a Permit2 PermitWitnessTransferFrom for `amount` of SBC to settings.pay_to_address.
    Returns (signature_hex, the `permit2Authorization` dict the facilitator expects)."""
    account = _payer_account()

    domain = {
        "name": "Permit2",
        "chainId": settings.chain_id,
        "verifyingContract": settings.permit2_contract_address,
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
        "permitted": {"token": settings.sbc_contract_address, "amount": amount},
        "spender": settings.x402_proxy_address,
        "nonce": nonce,
        "deadline": deadline,
        "witness": {"to": settings.pay_to_address, "validAfter": 0},
    }

    signable = encode_typed_data(domain_data=domain, message_types=types, message_data=message)
    signed = account.sign_message(signable)

    # hexbytes .hex() is not 0x-prefixed on this version.
    signature_hex = "0x" + signed.signature.hex()
    authorization = {
        "permitted": {"token": settings.sbc_contract_address, "amount": str(amount)},
        "from": account.address,
        "spender": settings.x402_proxy_address,
        "nonce": str(nonce),
        "deadline": str(deadline),
        "witness": {"to": settings.pay_to_address, "validAfter": "0"},
    }
    return signature_hex, authorization


def _build_facilitator_request(signature: str, authorization: dict) -> dict:
    """Body for /verify and /settle. Takes no caller-supplied amount BY DESIGN:
    requirements must come from our config, never the client. See REPORT.md §10.1."""
    requirements = build_payment_requirements()
    payment_payload = {
        "x402Version": 2,
        "resource": {
            "url": settings.resource_url,
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
    resp = requests.post(f"{settings.facilitator_url}/verify", json=body, timeout=10)
    resp.raise_for_status()
    return resp.json()


def settle_payment(signature: str, authorization: dict) -> dict:
    """POST /settle. A replayed payload returns the SAME cached tx hash rather than
    erroring — the facilitator is idempotent, so the gateway must catch reuse."""
    body = _build_facilitator_request(signature, authorization)
    resp = requests.post(f"{settings.facilitator_url}/settle", json=body, timeout=10)
    resp.raise_for_status()
    return resp.json()


# --- Milestone 2: InferenceEscrow — deposit once, sign+settle per prompt ---


def build_escrow_requirements(amount: int | None = None) -> dict:
    """Second `accepts[]` entry. `payTo` is just the contract: `provider` is
    immutable, set at deploy, not chosen per request."""
    return {
        "scheme": "exact",
        "network": f"eip155:{settings.chain_id}",
        "amount": str(settings.price_base_units if amount is None else amount),
        "payTo": settings.escrow_contract_address,
        "asset": settings.sbc_contract_address,
        "maxTimeoutSeconds": 300,
        "extra": {
            "assetTransferMethod": "inference-escrow",
            "contractAddress": settings.escrow_contract_address,
        },
    }


def sign_escrow_authorization(amount: int, deadline_seconds: int = 300) -> tuple[str, dict]:
    """Authorize a draw-down against an existing deposit; moves no funds itself.
    `settler` is the only address that may submit it, so an eavesdropper can't
    redeem it. Nonces are random/unordered, so concurrent prompts can't collide."""
    account = _payer_account()
    nonce = int.from_bytes(secrets.token_bytes(32), "big")
    deadline = int(time.time()) + deadline_seconds

    domain = {
        "name": "InferenceEscrow",
        "version": "1",
        "chainId": settings.chain_id,
        "verifyingContract": settings.escrow_contract_address,
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
        "settler": settings.gateway_operator_address,
        "amount": amount,
        "nonce": nonce,
        "deadline": deadline,
    }

    signable = encode_typed_data(domain_data=domain, message_types=types, message_data=message)
    signed = account.sign_message(signable)

    signature_hex = "0x" + signed.signature.hex()
    authorization = {
        "settler": settings.gateway_operator_address,
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
    """Free pre-flight — this path's /verify. Asks the EVM whether settle() would
    succeed, costing no gas, so the gateway can validate before serving."""
    account = _operator_account()
    try:
        _escrow_settle_fn(signature, authorization).call({"from": account.address})
    except ContractLogicError as exc:
        return {"ok": False, "error": _decode_revert_reason(exc)}
    return {"ok": True}


def submit_escrow_settlement(signature: str, authorization: dict) -> dict:
    """Settle on-chain from the operator wallet — a different wallet from the
    payer, so msg.sender genuinely isn't the payer (and must equal auth.settler)."""
    account = _operator_account()
    receipt = _send(account, _escrow_settle_fn(signature, authorization))
    return {"success": receipt.status == 1, "transaction": Web3.to_hex(receipt.transactionHash)}


def settle_escrow_payment(signature: str, authorization: dict) -> dict:
    """Wrapper for scripts. The gateway drives the two phases separately so it can
    serve the inference in between."""
    simulated = simulate_escrow_settlement(signature, authorization)
    if not simulated["ok"]:
        return {"success": False, "error": simulated["error"]}
    return submit_escrow_settlement(signature, authorization)
