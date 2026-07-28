"""One-off script: real deposit -> settle -> withdraw cycle against the
deployed InferenceEscrow on Radius testnet. Not part of the app; just proves
the contract works end to end on-chain, mirroring the Foundry test but for real."""
import json
import os
import secrets
import time

from dotenv import load_dotenv
from eth_account import Account
from eth_account.messages import encode_typed_data
from web3 import Web3

load_dotenv("../.env")

RPC_URL = os.environ["RPC_URL"]
CHAIN_ID = int(os.environ["CHAIN_ID"])
SBC_CONTRACT_ADDRESS = os.environ["SBC_CONTRACT_ADDRESS"]
WALLET_KEY = os.environ["WALLET_KEY"]
ESCROW_ADDRESS = "0xe629C40907f0cB5f93BaeD5F0a97f6E94bD44d7E"

w3 = Web3(Web3.HTTPProvider(RPC_URL))
account = Account.from_key(WALLET_KEY)

with open("/tmp/escrow_abi.json") as f:
    escrow_abi = json.load(f)

erc20_abi = [
    {"type": "function", "name": "approve", "stateMutability": "nonpayable",
     "inputs": [{"name": "spender", "type": "address"}, {"name": "value", "type": "uint256"}],
     "outputs": [{"type": "bool"}]},
    {"type": "function", "name": "balanceOf", "stateMutability": "view",
     "inputs": [{"name": "a", "type": "address"}], "outputs": [{"type": "uint256"}]},
]

sbc = w3.eth.contract(address=Web3.to_checksum_address(SBC_CONTRACT_ADDRESS), abi=erc20_abi)
escrow = w3.eth.contract(address=Web3.to_checksum_address(ESCROW_ADDRESS), abi=escrow_abi)

DEPOSIT_AMOUNT = 5000  # 0.005 SBC
CHARGE_AMOUNT = 1000   # 0.001 SBC


def send(fn):
    tx = fn.build_transaction({
        "from": account.address,
        "chainId": CHAIN_ID,
        "nonce": w3.eth.get_transaction_count(account.address, "pending"),
        "gas": 300_000,
        "gasPrice": w3.eth.gas_price,
    })
    signed = account.sign_transaction(tx)
    tx_hash = w3.eth.send_raw_transaction(signed.raw_transaction)
    receipt = w3.eth.wait_for_transaction_receipt(tx_hash, timeout=60)
    return receipt


# 1. approve + deposit
print("approving SBC allowance for escrow...")
r = send(sbc.functions.approve(Web3.to_checksum_address(ESCROW_ADDRESS), DEPOSIT_AMOUNT))
print("  approve status:", r.status, "tx:", Web3.to_hex(r.transactionHash))

print("depositing", DEPOSIT_AMOUNT, "base units...")
r = send(escrow.functions.deposit(DEPOSIT_AMOUNT))
print("  deposit status:", r.status, "tx:", Web3.to_hex(r.transactionHash))

balance_after_deposit = escrow.functions.balances(account.address).call()
print("  escrow balance after deposit:", balance_after_deposit)

# 2. sign an Authorization and settle
next_nonce = escrow.functions.nextNonce(account.address).call()
deadline = int(time.time()) + 300

domain = {"name": "InferenceEscrow", "version": "1", "chainId": CHAIN_ID, "verifyingContract": Web3.to_checksum_address(ESCROW_ADDRESS)}
types = {
    "EIP712Domain": [
        {"name": "name", "type": "string"},
        {"name": "version", "type": "string"},
        {"name": "chainId", "type": "uint256"},
        {"name": "verifyingContract", "type": "address"},
    ],
    "Authorization": [
        {"name": "amount", "type": "uint256"},
        {"name": "nonce", "type": "uint256"},
        {"name": "deadline", "type": "uint256"},
    ],
}
message = {"amount": CHARGE_AMOUNT, "nonce": next_nonce, "deadline": deadline}
signable = encode_typed_data(domain_data=domain, message_types={"Authorization": types["Authorization"]}, message_data=message)
signed_msg = account.sign_message(signable)
sig = bytes(signed_msg.signature)
if sig[64] < 27:
    sig = sig[:64] + bytes([sig[64] + 27])

print("settling", CHARGE_AMOUNT, "base units, nonce", next_nonce, "...")
r = send(escrow.functions.settle((CHARGE_AMOUNT, next_nonce, deadline), sig))
print("  settle status:", r.status, "tx:", Web3.to_hex(r.transactionHash))

balance_after_settle = escrow.functions.balances(account.address).call()
print("  escrow balance after settle:", balance_after_settle, "(expected", balance_after_deposit - CHARGE_AMOUNT, ")")

# 3. withdraw remainder
print("withdrawing remainder...")
r = send(escrow.functions.withdraw())
print("  withdraw status:", r.status, "tx:", Web3.to_hex(r.transactionHash))

balance_after_withdraw = escrow.functions.balances(account.address).call()
print("  escrow balance after withdraw:", balance_after_withdraw, "(expected 0)")
