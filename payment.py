import json
import re
import secrets
import threading
import time
from pathlib import Path

import requests
from eth_abi import decode as abi_decode
from eth_abi import encode as abi_encode
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
            except Exception:  # noqa: S110 - not Error(string); fall through
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


def rpc_reachable() -> bool:
    """Path B needs this. Path A does not — the facilitator submits its own txs."""
    try:
        return _w3.is_connected()
    except Exception:
        return False


def facilitator_reachable(timeout: float = 2.0) -> bool:
    """Path A needs this. Any HTTP reply counts: only a connection failure means
    unreachable, and /verify rejects the bodyless probe a HEAD would send."""
    try:
        requests.head(settings.facilitator_url, timeout=timeout)
        return True
    except requests.RequestException:
        return False


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


# --- Milestone 2: InferenceEscrow — deposit once, auth-capture per prompt ---
#
# The escrow implements the x402 `auth-capture` scheme. `PaymentInfo` is the
# scheme's payment identity: 12 fields hashed (with chainId and the escrow
# address) into a `paymentInfoHash` that keys all on-chain state. The gateway
# reconstructs that struct from its own config plus the two fields only the
# client supplies — `payer` and `salt` — exactly as the spec's facilitator does,
# so no per-payment state is stored on either side.

# Field order is load-bearing: it must match InferenceEscrow.PaymentInfo and
# PAYMENT_INFO_TYPEHASH exactly, or every hash silently diverges.
PAYMENT_INFO_FIELDS = (
    ("operator", "address"),
    ("payer", "address"),
    ("receiver", "address"),
    ("token", "address"),
    ("maxAmount", "uint120"),
    ("preApprovalExpiry", "uint48"),
    ("authorizationExpiry", "uint48"),
    ("refundExpiry", "uint48"),
    ("minFeeBps", "uint16"),
    ("maxFeeBps", "uint16"),
    ("feeReceiver", "address"),
    ("salt", "uint256"),
)
_PAYMENT_INFO_TUPLE_TYPE = "(" + ",".join(t for _, t in PAYMENT_INFO_FIELDS) + ")"
PAYMENT_INFO_TYPEHASH = Web3.keccak(
    text="PaymentInfo(" + ",".join(f"{t} {n}" for n, t in PAYMENT_INFO_FIELDS) + ")"
)

# Fees are declared out of scope for this implementation: the scheme's fee split
# is real surface deliberately not built, so every payment pins them to zero.
NO_FEE = {"minFeeBps": 0, "maxFeeBps": 0, "feeReceiver": "0x" + "00" * 20}


def build_escrow_requirements(amount: int | None = None) -> dict:
    """Second `accepts[]` entry. Carries everything the client needs to rebuild
    `PaymentInfo` itself — the deadlines are absolute, so the client echoes them
    back and the gateway re-derives the same hash without storing anything."""
    price = settings.price_base_units if amount is None else amount
    now = int(time.time())
    return {
        # Still "exact" with a homegrown discriminator: moving to
        # scheme: "auth-capture" is Phase 1.5's transport work, deliberately
        # separate from this contract change.
        "scheme": "exact",
        "network": f"eip155:{settings.chain_id}",
        "amount": str(price),
        "payTo": settings.pay_to_address,
        "asset": settings.sbc_contract_address,
        "maxTimeoutSeconds": settings.pre_approval_expiry_seconds,
        "extra": {
            "assetTransferMethod": "inference-escrow",
            "contractAddress": settings.escrow_contract_address,
            "tokenCollector": settings.escrow_collector_address,
            "captureAuthorizer": settings.gateway_operator_address,
            "preApprovalExpiry": now + settings.pre_approval_expiry_seconds,
            "captureDeadline": now + settings.authorization_expiry_seconds,
            "refundDeadline": now + settings.refund_expiry_seconds,
            **NO_FEE,
        },
    }


def build_payment_info(payer: str, salt: int, requirements: dict) -> dict:
    """Assemble the PaymentInfo the escrow will hash. Every field except `payer`
    and `salt` comes from the requirements the gateway itself issued."""
    extra = requirements["extra"]
    return {
        "operator": Web3.to_checksum_address(extra["captureAuthorizer"]),
        "payer": Web3.to_checksum_address(payer),
        "receiver": Web3.to_checksum_address(requirements["payTo"]),
        "token": Web3.to_checksum_address(requirements["asset"]),
        "maxAmount": int(requirements["amount"]),
        "preApprovalExpiry": int(extra["preApprovalExpiry"]),
        "authorizationExpiry": int(extra["captureDeadline"]),
        "refundExpiry": int(extra["refundDeadline"]),
        "minFeeBps": int(extra["minFeeBps"]),
        "maxFeeBps": int(extra["maxFeeBps"]),
        "feeReceiver": Web3.to_checksum_address(extra["feeReceiver"]),
        "salt": int(salt),
    }


def payment_info_tuple(info: dict) -> tuple:
    """Solidity calldata ordering. web3.py takes structs as plain tuples."""
    return tuple(
        Web3.to_checksum_address(info[name]) if solidity_type == "address" else int(info[name])
        for name, solidity_type in PAYMENT_INFO_FIELDS
    )


def payment_info_hash(info: dict) -> bytes:
    """Recompute the escrow's `getHash` off-chain, so the payer can sign the
    payment's identity without an RPC round trip.

    Mirrors InferenceEscrow.getHash: the struct is hashed with its typehash,
    then again with chainId and the escrow address — which is what stops a
    signature crossing chains or escrow deployments. tests/test_payment_info_hash.py
    asserts this equals the contract's own getHash() against a live anvil, rather
    than trusting that this reimplementation stayed correct.
    """
    struct_hash = Web3.keccak(
        abi_encode(
            ["bytes32", _PAYMENT_INFO_TUPLE_TYPE],
            [PAYMENT_INFO_TYPEHASH, payment_info_tuple(info)],
        )
    )
    return Web3.keccak(
        abi_encode(
            ["uint256", "address", "bytes32"],
            [settings.chain_id, settings.escrow_contract_address, struct_hash],
        )
    )


def sign_escrow_collect(amount: int, requirements: dict | None = None) -> tuple[str, dict]:
    """Payer side. Signs `Collect(paymentInfoHash, amount)` in the collector's
    own EIP-712 domain — consent for the collector to debit this payer's tab for
    this one payment, and nothing else.

    Returns (signature, payload) where payload carries the full PaymentInfo, so
    the gateway can rebuild the identical struct without keeping state.
    """
    account = _payer_account()
    requirements = build_escrow_requirements() if requirements is None else requirements
    # The client's only entropy contribution, and what keeps concurrent prompts
    # from the same payer distinct — same role as the scheme's `salt`.
    salt = int.from_bytes(secrets.token_bytes(32), "big")
    info = build_payment_info(account.address, salt, requirements)

    domain = {
        "name": "InferenceEscrowCollector",
        "version": "1",
        "chainId": settings.chain_id,
        "verifyingContract": settings.escrow_collector_address,
    }
    types = {
        "Collect": [
            {"name": "paymentInfoHash", "type": "bytes32"},
            {"name": "amount", "type": "uint256"},
        ],
    }
    message = {"paymentInfoHash": payment_info_hash(info), "amount": amount}

    signable = encode_typed_data(domain_data=domain, message_types=types, message_data=message)
    signed = account.sign_message(signable)

    payload = {
        "paymentInfo": {k: str(v) for k, v in info.items()},
        "amount": str(amount),
    }
    return "0x" + signed.signature.hex(), payload


def _collector_data(signature: str) -> bytes:
    """The collector reads its payer signature as an abi-encoded `bytes`, so the
    opaque `collectorData` blob the scheme passes through is that encoding."""
    return abi_encode(["bytes"], [bytes.fromhex(signature[2:])])


def _authorize_fn(info: dict, amount: int, signature: str):
    return _escrow.functions.authorize(
        payment_info_tuple(info),
        amount,
        settings.escrow_collector_address,
        _collector_data(signature),
    )


def _capture_fn(info: dict, amount: int):
    return _escrow.functions.capture(
        payment_info_tuple(info), amount, 0, NO_FEE["feeReceiver"]
    )


def _charge_fn(info: dict, amount: int, signature: str):
    return _escrow.functions.charge(
        payment_info_tuple(info),
        amount,
        settings.escrow_collector_address,
        _collector_data(signature),
        0,
        NO_FEE["feeReceiver"],
    )


def _void_fn(info: dict):
    return _escrow.functions.void(payment_info_tuple(info))


def _simulate(fn) -> dict:
    account = _operator_account()
    try:
        fn.call({"from": account.address})
    except ContractLogicError as exc:
        return {"ok": False, "error": _decode_revert_reason(exc)}
    return {"ok": True}


def _submit(fn) -> dict:
    receipt = _send(_operator_account(), fn)
    return {
        "success": receipt.status == 1,
        "transaction": Web3.to_hex(receipt.transactionHash),
        "gas_used": receipt.gasUsed,
    }


def simulate_escrow_authorization(signature: str, payload: dict) -> dict:
    """Free pre-flight: would placing the hold succeed? Catches a spent payment,
    an underfunded tab, a bad signature, and expired deadlines, all without gas."""
    info, amount = payload["paymentInfo"], int(payload["amount"])
    return _simulate(_authorize_fn(info, amount, signature))


def submit_escrow_authorization(signature: str, payload: dict) -> dict:
    """Place the hold on-chain. Unlike the v2 single-shot settle(), this happens
    BEFORE the inference runs — the funds are reserved but not yet the
    provider's, which is the whole point of the two-phase flow."""
    info, amount = payload["paymentInfo"], int(payload["amount"])
    return _submit(_authorize_fn(info, amount, signature))


def submit_escrow_capture(payload: dict) -> dict:
    """Pay the receiver out of an existing hold, after the inference succeeded."""
    info, amount = payload["paymentInfo"], int(payload["amount"])
    return _submit(_capture_fn(info, amount))


def submit_escrow_void(payload: dict) -> dict:
    """Release a hold we will not capture — the inference failed, so the payer
    must get their tab credit back rather than wait for `authorizationExpiry`."""
    return _submit(_void_fn(payload["paymentInfo"]))


def simulate_escrow_charge(signature: str, payload: dict) -> dict:
    info, amount = payload["paymentInfo"], int(payload["amount"])
    return _simulate(_charge_fn(info, amount, signature))


def submit_escrow_charge(signature: str, payload: dict) -> dict:
    """Single-shot autoCapture: debit and pay in one transaction. Kept deployed
    and reachable as the deliberately racy control for the amplification A/B —
    both arms are conformant `auth-capture` modes of the same contract."""
    info, amount = payload["paymentInfo"], int(payload["amount"])
    return _submit(_charge_fn(info, amount, signature))
