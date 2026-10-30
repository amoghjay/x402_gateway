# Latency probes

Scripts behind the numbers in [docs/design/request-path-budget.md](../../docs/design/request-path-budget.md).
Rerun them from inside the GKE cluster (`kubectl run` with a Python image) to replace the
laptop numbers.

| Script | What it measures | Side effects |
|---|---|---|
| `measure_settle.py` | SBC transfer to final receipt (`eth_sendRawTransactionSync`), facilitator `/verify` and `/settle` for Permit2 and EIP-3009 | **Testnet only.** Creates two throwaway wallets, funds one from the faucet (signed drip), sends about 0.02 SBC. The key never leaves the process. |
| `measure_checks.py` | `balanceOf` and Permit2 `allowance` over RPC, single and batched, on testnet and mainnet. EIP-712 recover in Python. | Read-only |
| `verifybench/` | EIP-712 Permit2 hash + `ecrecover` in Go (go-ethereum) | None. Its own Go module, so it stays out of the root build. |
| `measure_rpc_window.py` | `balanceOf` once a second per network for 10 minutes: p50/p90/p99/max, the share over 100/140/200 ms, and errors (429s, timeouts), for kept-alive and new connections. Standard library only. | Read-only |
| `k8s/rpc-window-job.yaml` | Runs `measure_rpc_window.py` as a Job on the system pool | A Job and a ConfigMap in `x402-system` |
| `k8s/redis-latency-job.yaml` | Redis `PING` and the replay guard's `SET … NX EX` round trip from another pod on the system pool | Writes `probe:*` keys that expire after 60 s |

**In the cluster** (from the repo root, with direnv's identity):

```bash
kubectl -n x402-system create configmap rpc-window --from-file=tools/latency/measure_rpc_window.py
kubectl apply -f tools/latency/k8s/
kubectl -n x402-system logs -f job/redis-latency   # about a minute
kubectl -n x402-system logs -f job/rpc-window      # 10 minutes; the last line is JSON
kubectl -n x402-system delete job rpc-window redis-latency   # or wait: they clean up after a day
kubectl -n x402-system delete configmap rpc-window
```

```bash
venv/bin/python tools/latency/measure_checks.py
venv/bin/python tools/latency/measure_settle.py
(cd tools/latency/verifybench && CGO_ENABLED=0 go test -bench . -run x)
```

Use `CGO_ENABLED=0` because `make build` ships the filter that way. It uses go-ethereum's
pure-Go signature library, which is about twice as slow as the cgo one and still well
under a millisecond.

The testnet faucet now requires the signed drip. `measure_settle.py` signs the challenge
only if it reads exactly `Radius Faucet: drip SBC to <its own address>`.
