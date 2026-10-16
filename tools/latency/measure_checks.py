"""Time the request-path checks: balanceOf and Permit2 allowance over RPC (single call and
one JSON-RPC batch) on testnet and mainnet, and EIP-712 verification in Python.
Read-only. Run from the repo root: venv/bin/python tools/latency/measure_checks.py
"""
import time, statistics, requests, secrets
from eth_account import Account
from eth_account.messages import encode_typed_data
S = requests.Session()
SBC = "0x33ad9e4BD16B69B5BFdED37D8B5D9fF9aba014Fb"; PERMIT2 = "0x000000000022D473030F116dDEE9F6B43aC78BA3"
HOLDER = "0x402085c248EeA27D92E8b30b2C58ed07f9E20001"
bal = "0x70a08231" + HOLDER[2:].lower().rjust(64, "0")
alw = "0xdd62ed3e" + HOLDER[2:].lower().rjust(64, "0") + PERMIT2[2:].lower().rjust(64, "0")
def summ(x): x=sorted(x); return f"n={len(x)} p50={statistics.median(x):.0f} p90={x[int(len(x)*0.9)-1]:.0f} max={x[-1]:.0f} ms"
for name, rpc in [("testnet", "https://rpc.testnet.radiustech.xyz"), ("mainnet", "https://rpc.radiustech.xyz")]:
    one, batch = [], []
    S.post(rpc, json={"jsonrpc":"2.0","id":0,"method":"eth_blockNumber","params":[]})  # warm connection
    for _ in range(20):
        t=time.perf_counter(); r=S.post(rpc, json={"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":SBC,"data":bal},"latest"]}, timeout=10).json(); one.append((time.perf_counter()-t)*1000)
        assert "result" in r, r
        t=time.perf_counter(); r=S.post(rpc, json=[{"jsonrpc":"2.0","id":1,"method":"eth_call","params":[{"to":SBC,"data":bal},"latest"]},{"jsonrpc":"2.0","id":2,"method":"eth_call","params":[{"to":SBC,"data":alw},"latest"]}], timeout=10).json(); batch.append((time.perf_counter()-t)*1000)
        assert isinstance(r, list) and all("result" in x for x in r), r
        time.sleep(0.1)
    print(f"{name} balanceOf eth_call: {summ(one)}")
    print(f"{name} balanceOf+allowance, one JSON-RPC batch: {summ(batch)}")
# local verification cost in Python (upper bound; Go is faster)
acct = Account.create()
dom = {"name":"Permit2","chainId":72344,"verifyingContract":PERMIT2}
types = {"PermitWitnessTransferFrom":[{"name":"permitted","type":"TokenPermissions"},{"name":"spender","type":"address"},{"name":"nonce","type":"uint256"},{"name":"deadline","type":"uint256"},{"name":"witness","type":"Witness"}],
         "TokenPermissions":[{"name":"token","type":"address"},{"name":"amount","type":"uint256"}],"Witness":[{"name":"to","type":"address"},{"name":"validAfter","type":"uint256"}]}
msg = {"permitted":{"token":SBC,"amount":1000},"spender":HOLDER,"nonce":int.from_bytes(secrets.token_bytes(32),"big"),"deadline":int(time.time())+300,"witness":{"to":HOLDER,"validAfter":0}}
sig = acct.sign_message(encode_typed_data(domain_data=dom, message_types=types, message_data=msg)).signature
N=300; t=time.perf_counter()
for _ in range(N):
    assert Account.recover_message(encode_typed_data(domain_data=dom, message_types=types, message_data=msg), signature=sig) == acct.address
print(f"python EIP-712 hash + recover: {(time.perf_counter()-t)/N*1000:.2f} ms each")
