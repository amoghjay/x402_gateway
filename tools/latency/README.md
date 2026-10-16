# Latency probes

Scripts behind the numbers in [docs/design/request-path-budget.md](../../docs/design/request-path-budget.md).
Rerun them from inside the GKE cluster (`kubectl run` with a Python image) to replace the
laptop numbers.

| Script | What it measures | Side effects |
|---|---|---|
| `measure_settle.py` | SBC transfer to final receipt (`eth_sendRawTransactionSync`), facilitator `/verify` and `/settle` for Permit2 and EIP-3009 | **Testnet only.** Creates two throwaway wallets, funds one from the faucet (signed drip), sends about 0.02 SBC. The key never leaves the process. |
| `measure_checks.py` | `balanceOf` and Permit2 `allowance` over RPC, single and batched, on testnet and mainnet. EIP-712 recover in Python. | Read-only |
| `verifybench/` | EIP-712 Permit2 hash + `ecrecover` in Go (go-ethereum) | None. Its own Go module, so it stays out of the root build. |

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
