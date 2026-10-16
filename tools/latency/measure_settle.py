"""Measure Radius testnet settlement latency from this machine.

Throwaway wallets only; the private key lives in this process and is never printed.
Run from the repo root with the legacy venv: venv/bin/python tools/latency/measure_settle.py
"""
import json, secrets, statistics, sys, time
import requests
from eth_account import Account
from eth_account.messages import encode_typed_data
from web3 import Web3

RPC = "https://rpc.testnet.radiustech.xyz"
FAUCET = "https://testnet.radiustech.xyz/api/v1/faucet"
COST_API = "https://testnet.radiustech.xyz/api/v1/network/transaction-cost"
FAC = "https://facilitator.testnet.radiustech.xyz"
CHAIN = 72344
SBC = Web3.to_checksum_address("0x33ad9e4BD16B69B5BFdED37D8B5D9fF9aba014Fb")
PERMIT2 = Web3.to_checksum_address("0x000000000022D473030F116dDEE9F6B43aC78BA3")
PROXY = Web3.to_checksum_address("0x402085c248EeA27D92E8b30b2C58ed07f9E20001")
S = requests.Session()

def rpc(method, params=()):
    r = S.post(RPC, json={"jsonrpc": "2.0", "id": 1, "method": method, "params": list(params)}, timeout=15).json()
    if "error" in r:
        raise RuntimeError(f"{method}: {r['error']}")
    return r["result"]

def ms(t0): return (time.perf_counter() - t0) * 1000
def summary(xs): return f"n={len(xs)} min={min(xs):.0f} p50={statistics.median(xs):.0f} max={max(xs):.0f} ms"

w3 = Web3()
erc20 = w3.eth.contract(address=SBC, abi=[
    {"name": "transfer", "type": "function", "inputs": [{"name": "to", "type": "address"}, {"name": "v", "type": "uint256"}], "outputs": [{"type": "bool"}], "stateMutability": "nonpayable"},
    {"name": "approve", "type": "function", "inputs": [{"name": "s", "type": "address"}, {"name": "v", "type": "uint256"}], "outputs": [{"type": "bool"}], "stateMutability": "nonpayable"},
    {"name": "balanceOf", "type": "function", "inputs": [{"name": "a", "type": "address"}], "outputs": [{"type": "uint256"}], "stateMutability": "view"},
])

buyer = Account.create()
pay_to = Account.create().address
print("buyer", buyer.address, "payTo", pay_to)

def sbc_balance(a):
    data = erc20.encode_abi("balanceOf", [a])
    return int(rpc("eth_call", [{"to": SBC, "data": data}, "latest"]), 16)

# --- fund ---
d = S.post(f"{FAUCET}/drip", json={"address": buyer.address, "token": "SBC"}, timeout=30).json()
err = d.get("error") or {}
if isinstance(err, dict) and err.get("code") == "signature_required":
    from eth_account.messages import encode_defunct
    challenge = (err.get("details") or {}).get("challenge", "")
    if challenge != f"Radius Faucet: drip SBC to {buyer.address}":
        sys.exit(f"unexpected challenge text, not signing: {challenge!r}")
    sig = "0x" + buyer.sign_message(encode_defunct(text=challenge)).signature.hex().removeprefix("0x")
    d = S.post(f"{FAUCET}/drip", json={"address": buyer.address, "token": "SBC", "signature": sig}, timeout=30).json()
print("drip:", {k: d.get(k) for k in ("success", "error", "tx_hash", "retry_after_ms")})
if not d.get("success"):
    sys.exit("faucet did not fund the wallet; stopping")
for _ in range(40):
    if sbc_balance(buyer.address) > 0:
        break
    time.sleep(0.25)
print("buyer SBC balance:", sbc_balance(buyer.address) / 1e6)

# --- baseline: network round trip to the RPC ---
base = []
for _ in range(10):
    t0 = time.perf_counter(); rpc("eth_blockNumber"); base.append(ms(t0))
print("RPC round trip (eth_blockNumber):", summary(base))

gas_price = int(S.get(COST_API, timeout=10).json()["gas_price_wei"], 16)
nonce = int(rpc("eth_getTransactionCount", [buyer.address, "pending"]), 16)

def send_sync(fn_name, args):
    global nonce
    data = erc20.encode_abi(fn_name, args)
    tx = {"to": SBC, "data": data, "value": 0, "gas": 150_000, "gasPrice": gas_price, "nonce": nonce, "chainId": CHAIN}
    raw = "0x" + buyer.sign_transaction(tx).raw_transaction.hex().removeprefix("0x")
    t0 = time.perf_counter()
    rcpt = rpc("eth_sendRawTransactionSync", [raw])
    dt = ms(t0)
    nonce += 1
    return dt, rcpt

# --- A: plain SBC transfer, synchronous send (receipt returned is final) ---
sync = []
for i in range(8):
    dt, r = send_sync("transfer", [pay_to, 1000])
    sync.append(dt)
    print(f"  transfer {i}: {dt:.0f} ms status={r.get('status')} tx={r.get('transactionHash')}")
print("A. SBC transfer, submit -> final receipt:", summary(sync))

# --- B: x402 exact via Permit2 through the facilitator (the filter's path) ---
dt, r = send_sync("approve", [PERMIT2, 2**256 - 1])
print(f"Permit2 approve: {dt:.0f} ms status={r.get('status')}")

def req(method, amount=1000):
    return {"scheme": "exact", "network": f"eip155:{CHAIN}", "amount": str(amount), "payTo": pay_to,
            "asset": SBC, "maxTimeoutSeconds": 300,
            "extra": {"assetTransferMethod": method, "name": "Stable Coin", "version": "1"}}

def body(method, payload):
    rq = req(method)
    return {"x402Version": 2, "paymentRequirements": rq,
            "paymentPayload": {"x402Version": 2, "resource": {"url": "https://demo.example/search", "description": "latency probe", "mimeType": "application/json"},
                               "accepted": rq, "payload": payload}}

def permit2_payload(amount=1000):
    n = int.from_bytes(secrets.token_bytes(32), "big"); dl = int(time.time()) + 300
    signable = encode_typed_data(
        domain_data={"name": "Permit2", "chainId": CHAIN, "verifyingContract": PERMIT2},
        message_types={
            "PermitWitnessTransferFrom": [{"name": "permitted", "type": "TokenPermissions"}, {"name": "spender", "type": "address"},
                                          {"name": "nonce", "type": "uint256"}, {"name": "deadline", "type": "uint256"}, {"name": "witness", "type": "Witness"}],
            "TokenPermissions": [{"name": "token", "type": "address"}, {"name": "amount", "type": "uint256"}],
            "Witness": [{"name": "to", "type": "address"}, {"name": "validAfter", "type": "uint256"}]},
        message_data={"permitted": {"token": SBC, "amount": amount}, "spender": PROXY, "nonce": n, "deadline": dl,
                      "witness": {"to": pay_to, "validAfter": 0}})
    sig = "0x" + buyer.sign_message(signable).signature.hex().removeprefix("0x")
    return {"signature": sig, "permit2Authorization": {
        "permitted": {"token": SBC, "amount": str(amount)}, "from": buyer.address, "spender": PROXY,
        "nonce": str(n), "deadline": str(dl), "witness": {"to": pay_to, "validAfter": "0"}}}

def eip3009_payload(amount=1000):
    n = "0x" + secrets.token_hex(32); vb = int(time.time()) + 300
    signable = encode_typed_data(
        domain_data={"name": "Stable Coin", "version": "1", "chainId": CHAIN, "verifyingContract": SBC},
        message_types={"TransferWithAuthorization": [
            {"name": "from", "type": "address"}, {"name": "to", "type": "address"}, {"name": "value", "type": "uint256"},
            {"name": "validAfter", "type": "uint256"}, {"name": "validBefore", "type": "uint256"}, {"name": "nonce", "type": "bytes32"}]},
        message_data={"from": buyer.address, "to": pay_to, "value": amount, "validAfter": 0, "validBefore": vb, "nonce": bytes.fromhex(n[2:])})
    sig = "0x" + buyer.sign_message(signable).signature.hex().removeprefix("0x")
    return {"signature": sig, "authorization": {"from": buyer.address, "to": pay_to, "value": str(amount),
                                                "validAfter": "0", "validBefore": str(vb), "nonce": n}}

def facilitator_round(method, make, rounds):
    ver, stl = [], []
    for i in range(rounds):
        b = body(method, make())
        t0 = time.perf_counter(); v = S.post(f"{FAC}/verify", json=b, timeout=15); dv = ms(t0)
        vj = v.json() if v.headers.get("content-type", "").startswith("application/json") else {"raw": v.text[:200]}
        if not vj.get("isValid"):
            print(f"  {method} {i}: verify HTTP {v.status_code} {json.dumps(vj)[:300]}")
            return ver, stl
        t0 = time.perf_counter(); s = S.post(f"{FAC}/settle", json=b, timeout=15); ds = ms(t0)
        sj = s.json() if s.headers.get("content-type", "").startswith("application/json") else {"raw": s.text[:200]}
        ok = sj.get("success")
        print(f"  {method} {i}: verify {dv:.0f} ms, settle {ds:.0f} ms, success={ok} tx={sj.get('transaction')}"
              + ("" if ok else f" body={json.dumps(sj)[:300]}"))
        if ok:
            ver.append(dv); stl.append(ds)
    return ver, stl

v, s = facilitator_round("permit2", permit2_payload, 6)
if s:
    print("B. facilitator /verify (permit2):", summary(v))
    print("B. facilitator /settle (permit2):", summary(s))
v, s = facilitator_round("eip3009", eip3009_payload, 4)
if s:
    print("C. facilitator /verify (eip3009):", summary(v))
    print("C. facilitator /settle (eip3009):", summary(s))
print("buyer SBC left:", sbc_balance(buyer.address) / 1e6, " payTo SBC:", sbc_balance(pay_to) / 1e6)
