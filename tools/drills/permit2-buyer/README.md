# permit2-buyer

Pays the Permit2 option of an x402 route with the reference client (`@x402/evm`), which
takes the filter's facilitator-only path. `radius-cli` 0.8.0 refuses the Platform's
Permit2 option (SBC on Base Sepolia), so this exists for that run.

```bash
cd tools/drills/permit2-buyer && npm install
node pay.mjs ~/.x402/buyer-base-sepolia.key http://<gateway>/anything/platform/paid/1 --status
node pay.mjs ~/.x402/buyer-base-sepolia.key http://<gateway>/anything/platform/paid/1 --approve
node pay.mjs ~/.x402/buyer-base-sepolia.key http://<gateway>/anything/platform/paid/1 --sign-only  # header for replay drills
```

`--status` pays nothing. `--approve` sends the one-time `approve(Permit2, max)` when the
allowance is short; it costs Base Sepolia ETH, because the option declares no gas
sponsoring. The buyer needs the option's token (SBC `0xf9FB20B8…0F16` on Base Sepolia).
The key file is read, never printed. `BASE_SEPOLIA_RPC` overrides the public RPC.

**What has run.** `--status`, and a signed payment for an unfunded buyer sent through the
gate (the facilitator refused it at verify; see the design note's production-Platform
section). `--approve` and a funded payment have not run yet.
