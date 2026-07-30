"""One-off script: real deposit -> settle -> withdraw cycle against the deployed
InferenceEscrow on Radius testnet. Not part of the app; it exists to prove the
full lifecycle works on-chain, and in particular that a payer can always exit —
withdraw() is the one function client.py's demo never exercises.

Reuses payment.py's helpers rather than re-deriving the signing logic, so it
cannot drift from the code the gateway actually runs (it did before: it had a
hardcoded contract address and a stale copy of the Authorization struct).
"""
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from eth_account import Account  # noqa: E402
from web3 import Web3  # noqa: E402

from payment import (  # noqa: E402
    ESCROW_CONTRACT_ADDRESS,
    PAYER_ADDRESS,
    WALLET_KEY,
    _escrow,
    _send,
    ensure_escrow_deposit,
    explorer_link,
    sbc_balance,
    settle_escrow_payment,
    sign_escrow_authorization,
)

CHARGE_AMOUNT = 1_000

print(f"InferenceEscrow: {ESCROW_CONTRACT_ADDRESS}")
print(f"payer          : {PAYER_ADDRESS}")

# 1. deposit
tab = ensure_escrow_deposit(min_amount=CHARGE_AMOUNT)
print(f"\n[1/3] deposit    -> tab balance {tab} base units")

# 2. sign + settle (submitted by the gateway operator, not the payer)
signature, authorization = sign_escrow_authorization(amount=CHARGE_AMOUNT)
result = settle_escrow_payment(signature, authorization)
assert result["success"], result
tab_after = _escrow.functions.balances(PAYER_ADDRESS).call()
print(f"[2/3] settle     -> tab {tab} -> {tab_after} (charged {tab - tab_after})")
print(f"                    settler = {authorization['settler']}")
print(f"                    tx {explorer_link(result['transaction'])}")

# 3. withdraw the remainder back to the payer's wallet
wallet_before = sbc_balance(PAYER_ADDRESS)
receipt = _send(Account.from_key(WALLET_KEY), _escrow.functions.withdraw())
refunded = sbc_balance(PAYER_ADDRESS) - wallet_before
print(f"[3/3] withdraw   -> status {receipt.status}, refunded {refunded} base units to wallet")
print(f"                    tx {explorer_link(Web3.to_hex(receipt.transactionHash))}")

final_tab = _escrow.functions.balances(PAYER_ADDRESS).call()
print(f"\nfinal tab balance: {final_tab} (expected 0)")
assert final_tab == 0, "withdraw() left funds behind"
print("lifecycle verified: deposit -> settle -> full exit")
