import os
import secrets
import time

import requests
from dotenv import load_dotenv
from eth_account import Account
from eth_account.messages import encode_typed_data
from web3 import Web3

load_dotenv()

CHAIN_ID = int(os.environ["CHAIN_ID"])
PERMIT2_CONTRACT_ADDRESS = os.environ["PERMIT2_CONTRACT_ADDRESS"]
X402_PROXY_ADDRESS = os.environ["X402_PROXY_ADDRESS"]
SBC_CONTRACT_ADDRESS = os.environ["SBC_CONTRACT_ADDRESS"]
PAY_TO_ADDRESS = os.environ["PAY_TO_ADDRESS"]
WALLET_KEY = os.environ["WALLET_KEY"]
FACILITATOR_URL = os.environ["FACILITATOR_URL"]
PRICE_BASE_UNITS = os.environ["PRICE_BASE_UNITS"]
RESOURCE_URL = "http://localhost:8000/infer"


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

    sig = bytes(signed.signature)
    if sig[64] < 27:
        sig = sig[:64] + bytes([sig[64] + 27])

    signature_hex = "0x" + sig.hex()
    authorization = {
        "permitted": {"token": SBC_CONTRACT_ADDRESS, "amount": str(amount)},
        "from": account.address,
        "spender": X402_PROXY_ADDRESS,
        "nonce": str(nonce),
        "deadline": str(deadline),
        "witness": {"to": PAY_TO_ADDRESS, "validAfter": "0"},
    }
    return signature_hex, authorization


def _build_facilitator_request(signature: str, authorization: dict, amount: str) -> dict:
    """Shared body shape for both /verify and /settle."""
    requirements = build_payment_requirements(amount)
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


def verify_payment(signature: str, authorization: dict, amount: str) -> dict:
    """POST to the facilitator's /verify. Validity is signaled by `isValid` in
    the response BODY, not by HTTP status — a bad signature still returns 200."""
    body = _build_facilitator_request(signature, authorization, amount)
    resp = requests.post(f"{FACILITATOR_URL}/verify", json=body, timeout=10)
    resp.raise_for_status()
    return resp.json()


def settle_payment(signature: str, authorization: dict, amount: str) -> dict:
    """POST to the facilitator's /settle. On success, `transaction` is the
    on-chain settlement tx hash. A replayed payload returns the SAME cached
    tx hash rather than settling again — the facilitator is idempotent, so
    the gateway's own replay guard (not this call) is what must catch reuse."""
    body = _build_facilitator_request(signature, authorization, amount)
    resp = requests.post(f"{FACILITATOR_URL}/settle", json=body, timeout=10)
    resp.raise_for_status()
    return resp.json()
