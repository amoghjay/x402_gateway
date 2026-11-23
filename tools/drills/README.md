# tools/drills

Scripts for checking a live gateway. Each script's header has its usage. The ones that pay
spend test tokens from `~/.x402/buyer-<network>.key`.

| Path | What |
|---|---|
| [`sync.sh`](sync.sh) | `captureMode: Sync` drills: one payment sent 12 times at once, a payment settled outside the gate then sent through it, N paid requests in a row. |
| [`platform.sh`](platform.sh) | The Phase 1 exit test for a Radius Platform route: free, denied, charged, metered and rate-limited answers, then a payment in each EIP-3009 option and its replay. PASS or FAIL per check. |
| [`wba-sign.mjs`](wba-sign.mjs) | Signs a request with Web Bot Auth, as ChatGPT's agent does, and prints the three headers for `curl -H`. Takes the agent's key file and origin; the origin must serve the public key. |
| [`fac-shim/`](fac-shim/) | A drill-only proxy in front of a facilitator that can delay or hang `/settle`. Setup and teardown are in `shim.yaml`. |
| [`permit2-buyer/`](permit2-buyer/README.md) | Pays a route's Permit2 option with the reference client `@x402/evm`, for the facilitator-only path. |

A Web Bot Auth check against a rule that allows a signed agent on `/blocked`:

```bash
export WBA_KEY=path/to/agent-key.jwk WBA_AGENT=https://agent.example   # yours
curl -s -o /dev/null -w '%{http_code}\n' -H @<(node tools/drills/wba-sign.mjs "$BASE/blocked") "$BASE/blocked"
```

200 when the signature verifies, 403 (the deny rule) when it does not.
