#!/usr/bin/env bash
# Regenerate the committed ABI that payment.py loads at import.
#
# contracts/out/ is build output and is not tracked, so the Python side reads
# contracts/abi/InferenceEscrow.json instead. That copy can drift from the source
# contract, so run this after any change to src/InferenceEscrow.sol.
#
# The drift is not left to discipline: tests/test_abi_sync.py re-runs this
# extraction and fails if the committed file differs, so a stale ABI breaks CI
# rather than silently mis-encoding a settle() call at runtime.
set -euo pipefail

cd "$(dirname "$0")"
mkdir -p abi
forge inspect InferenceEscrow abi --json > abi/InferenceEscrow.json
echo "wrote contracts/abi/InferenceEscrow.json"
