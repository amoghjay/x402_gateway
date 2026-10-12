# InferenceEscrow

The Path B settlement contract for the x402 gateway: **deposit once, draw down per
prompt.** The payer funds a tab, then signs an EIP-712 `Authorization` per inference;
the gateway operator submits it. Path A (Permit2 + a third-party facilitator) needs no
contract at all — this exists to compare *owning* the settlement rail against
*renting* it.

Live on Radius testnet at
[`0x76c03C8b763a0e2f3594bABfb71847e8a6d502D8`](https://testnet.radiustech.xyz/address/0x76c03C8b763a0e2f3594bABfb71847e8a6d502D8),
deployed with `token = SBC`, `provider = PAY_TO_ADDRESS` (both verified against the
contract's own getters).

## Why the design looks like this

- **`settler` is signed over.** Without it `settle()` would accept any submitter, so
  a third party holding a copy of the payer's signature could redeem it — burning the
  payer's nonce and funds without the payer ever being served.
- **Unordered nonces.** The payer picks 32 random bytes and the contract records it as
  spent. A monotonic counter is cheaper but serialises the payer: two prompts signed
  before the first settles would claim the same nonce, and the second would revert as
  a "replay" despite being legitimate. Permit2 uses a packed bitmap; a plain mapping
  costs more gas and is far easier to audit.
- **`deposit()` credits what arrived, not what was requested.** A fee-on-transfer
  token delivers less than `amount`, and crediting the request would leave the
  contract owing more than it holds.

Full threat analysis, including a bug deliberately shipped open, is in
[`../REPORT.md`](../REPORT.md) §10.

## Layout

```
src/InferenceEscrow.sol      the contract
test/InferenceEscrow.t.sol   9 tests
script/Deploy.s.sol          reproducible deployment
abi/InferenceEscrow.json     generated; what payment.py reads at import
sync-abi.sh                  regenerates abi/
foundry.lock                 dependency pins
```

## Build and test

```bash
forge build
forge test
forge snapshot            # gas baseline; via_ir = true, so this is worth tracking
```

## The ABI is generated but committed

`payment.py` reads `abi/InferenceEscrow.json`, **not** Foundry's `out/` directory.
`out/` is build output and untracked, so reading from it made a fresh clone raise
`FileNotFoundError` at import — before uvicorn bound.

Committing a generated file trades that for drift risk, so the drift is enforced away
rather than left to discipline:

```bash
./sync-abi.sh             # after any change to src/InferenceEscrow.sol
pytest ../tests/test_abi_sync.py
```

`test_abi_sync.py` re-runs `forge inspect` and fails if the committed copy is stale.
Same shape as `hack/verify-codegen.sh` in Kubernetes or `make manifests` in
kubebuilder: regenerate, diff, fail on mismatch.

## Deploy

```bash
forge script script/Deploy.s.sol \
  --rpc-url "$RPC_URL" \
  --private-key "$GATEWAY_OPERATOR_KEY" \
  --broadcast
```

The signer comes from the command line, not the environment, so no key needs to sit
in a file the script can read. Afterwards update `ESCROW_CONTRACT_ADDRESS` in `.env`
and `.env.example`, and re-run `./sync-abi.sh` if the interface changed.

## Dependencies are pinned to released tags

```
forge-std              v1.16.2   bf647bd6
openzeppelin-contracts v5.6.1    5fd1781b
```

Git submodules, recorded in `foundry.lock`. Clone with `--recursive`, or run
`git submodule update --init --recursive`.

**These replaced 1,085 vendored files that were committed directly.** That copy
reported `"version": "5.6.1"` in its `package.json` but was **not** the v5.6.1 tag —
114 files differed, and the tree was *newer* than the tag (`ERC4337Utils.sol` had
already graduated out of draft). It was an unreleased `master` snapshot with no
provenance recorded anywhere.

Of the 19 OpenZeppelin files in `InferenceEscrow`'s compilation unit, 4 differed: a
variable rename in `ECDSA.sol`, an unused `tryGetDecimals()` in `SafeERC20.sol`, line
wrapping in `Bytes.sol`, and a doc typo in `ShortStrings.sol`. Executable bytecode is
byte-identical across the swap and `forge snapshot --check` shows no gas change, so
none of it reached contract behaviour.

**One consequence worth stating plainly:** the trailing solc metadata hash *does*
change, so this repository no longer reproduces the exact bytecode of the currently
deployed contract. The executable portion still matches; a block-explorer verify would
be a partial match. This was accepted deliberately — pinning a payments contract to an
unreleased `master` commit is worse than losing metadata parity on a deployment that
`reserve/capture` supersedes.
