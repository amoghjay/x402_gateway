# Architecture

x402 payment enforcement in the data plane: a company whose backend already runs on
Kubernetes sells its existing endpoints per call, with **no app code changes and no
sidecar**. An app team applies one `PaymentPolicy` next to its `HTTPRoute`; from then on
the route answers 402 until a buyer pays, buyers pay with a signature (no account, no
API key), and the money lands on Radius. The backend is never changed or restarted.

This page is the map: what runs, what each part reads and writes, how a request moves,
and why the pieces are shaped the way they are. The measurements behind the numbers are
in [design/request-path-budget.md](design/request-path-budget.md).

## The map

```
                                   control plane
  app team ──kubectl apply──► PaymentPolicy (demo)
                                     │ watched by
                                     ▼
                              x402-controller (x402-system, 1 replica, leader lease)
                                     │ writes, Server-Side Apply
             ┌───────────────────────┼─────────────────────────┐
             ▼                       ▼                         ▼
   EnvoyExtensionPolicy        ConfigMap x402-prices      ReferenceGrant
   on the route (demo)         (price table, JSON)        (x402-system → demo's EEP
   owned by the policy         watched by the filter       may name the filter Service)
             │                       │
  ═══════════╪═══════════════════════╪═══════════ data plane ═══════════════════════════
             ▼                       ▼
  buyer ─► GCP L4 LB ─► Envoy Gateway ─ext_proc gRPC (one stream per request)─► x402-filter (2 replicas + PDB)
              34.173.224.114      │                                              │  verify sig ─ local, ~0.1 ms
                                  │ route has no EEP → backend, free             │  balance    ─ Redis cache / RPC
                                  │                                              │  reserve    ─ Lua: nonce + in-flight
                                  ▼                                              ▼
                               backend (httpbin, unchanged)                   Redis (x402-system, AOF)
                                  x-x402-payer, x-x402-payment-id set            │ stream "settle"
                                                                                 ▼
                                                                         x402-settler (1 replica)
                                                                                 │ /settle
                                                                                 ▼
                                                                      Radius facilitator ─► SBC on Radius testnet
```

The request path (buyer → Envoy → filter → backend) never calls the facilitator, and the
filter never calls the Kubernetes API on a request: its only per-request dependencies
are Redis and, on a cache miss, one `balanceOf` read from the RPC.

## Components

| Component | Where | What it reads | What it writes | Talks to Kubernetes? |
|---|---|---|---|---|
| **Envoy Gateway** v1.9.2 + Envoy 1.39 | `envoy-gateway-system`, proxies on the system pool | Gateway API objects, `EnvoyExtensionPolicy` | xDS to Envoy | yes (its own controller) |
| **`PaymentPolicy`** CRD `x402.dev/v1alpha1` | the app team's namespace | — | — | the object itself |
| **x402-controller** | `x402-system`, 1 replica, leader lease | `PaymentPolicy` (all namespaces), `HTTPRoute` metadata, the EEP's status | one owned EEP per policy, the price-table ConfigMap, the ReferenceGrant | yes: ClusterRole on policies/routes/EEPs; Role in `x402-system` on the one ConfigMap, grant and Lease by name |
| **x402-filter** | `x402-system`, 2 replicas, PDB, system pool | the price table (API watch, one ConfigMap by name), Redis, the RPC on a miss | Redis (nonce, in-flight, balance cache, settle stream) | only the ConfigMap watch (`list/watch`, `resourceNames`) |
| **Redis** 8.10 | `x402-system`, StatefulSet, 1 replica, AOF `everysec` on a PVC, system pool | — | — | no |
| **x402-settler** | `x402-system`, 1 replica, system pool | the settle stream, the RPC (`authorizationState`) | `XACK`, the dead-letter stream, in-flight releases | no: a ServiceAccount with no bindings and no token |
| **Radius facilitator** | hosted by Radius | — | the transfer on chain | — |
| **backend** (httpbin) | `demo`, spot pool | the headers the filter sets | — | — |

## Three flows

### 1. Pricing a route (control plane)

`kubectl apply` a `PaymentPolicy` → the controller validates it with the **filter's own
table parser** (so it can never accept what the filter would refuse), checks the target
`HTTPRoute` exists, and Server-Side Applies three objects: the `EnvoyExtensionPolicy`
that sends that route, and only that route, through the filter (`messageTimeout: 200ms`,
`statusOnError: 503`, `failOpen: false`, the `xds.route_name` attribute); the entry in
the `x402-prices` ConfigMap; and the namespace's entry in the ReferenceGrant. Envoy
Gateway's verdict on the EEP (`Accepted`, `Conflicted`) is mirrored into the policy's
status, which follows the Gateway API `PolicyStatus` shape so `kubectl get pp` reads
like any policy.

`kubectl delete` → Kubernetes garbage-collects the owned EEP (the route is free again)
and the controller's full recompute drops the table and grant entries.

Measured on GKE (2026-11-04): apply → first 402 in **0.34–0.39 s**; delete → first 200
in **≤ 0.13–0.21 s**.

### 2. An unpaid request

Envoy matches the route; the route's EEP sends the request headers to the filter with
the `xds.route_name` attribute. The filter keys the price table on the route's identity
(`httproute/<ns>/<name>`), never on the path, so the path tricks other gateways had to
patch cannot reach a price. No `PAYMENT-SIGNATURE` → an `ImmediateResponse` **402** with
`PAYMENT-REQUIRED` (the spec's header), a JSON body that repeats the terms plus a
`how-to-pay` extension (install, fund, the one command that pays), an HTML page for
browsers, markdown for agents that ask, `Link: rel="help"`, `Cache-Control: no-store`.
The backend is never contacted. A route with no policy has no EEP and never reaches the
filter.

### 3. A paid request

```
request phase (budget 150 ms, under Envoy's 200 ms)
  PAYMENT-SIGNATURE ─► decode ─► verify locally (EIP-712 recover, route's terms, window)
      ─► balance: Redis bal:{payer} (≤5 s fresh; 5-60 s serve last-known and refresh in the background; else RPC)
      ─► reserve (one Lua script): nonce already claimed? → 402 payment_already_used
                                   balance − in-flight < price? → 402 insufficient_funds
                                   else claim nonce (TTL validBefore+60 s), in-flight += price (TTL 30 s)
      ─► CONTINUE: remove payment-signature; set x-x402-payer, x-x402-payment-id (overwrite)
response phase (one Redis call)
  2xx ─► XADD settle {payment as received, terms, payer, amount, id}; PAYMENT-RESPONSE (pending receipt);
         Cache-Control: private, no-store overrides the backend
  else ─► release in-flight; no receipt
stream ends without a response phase ─► release
```

Our own slowness is never a charge: a balance read that overruns the deadline gets the
buyer a **503 + `Retry-After: 1`**, the read finishes anyway and fills the cache, and the
retry is a hit. Redis down → 503 (there is no safe way to serve without the replay
guard). A refused payment is a 402 whose `error` line leads with the reason.

### 4. Settlement

The settler reads the `settle` stream as a consumer group and calls the facilitator's
`/settle`. A job is acknowledged only once its outcome is known: settled (the explorer
link is logged), or refused for good. Anything uncertain, a timeout or a 5xx, is checked
on chain first (`authorizationState(payer, nonce)`: a used nonce means the transfer
happened) and otherwise left pending; `XAUTOCLAIM` brings it back after 60 s to whichever
settler runs, including a restarted pod. A job that can never settle (past `validBefore`,
malformed, five deliveries) goes to `settle:dead` and is logged at error: a served
request the seller was not paid for. The in-flight amount is released here, when the
job is done, so a cached balance is spent at most once before the chain catches up.

## Redis, the ledger

| Key | Holds | TTL | Written by | Read by |
|---|---|---|---|---|
| `nonce:{payer}:<nonce>` | the signature was served | `validBefore` + 60 s, **never released** | filter (reserve) | filter |
| `inflight:{payer}` | accepted-but-unsettled amount | 30 s, refreshed per reserve, deleted at 0 | filter (reserve/release), settler (release) | filter |
| `bal:{payer}` | cached balance + fetched-at | 60 s | filter (after a read) | filter |
| `settle` (stream) | jobs: the signed payment as received + the terms | `MAXLEN ~100000` | filter (capture) | settler (group `settlers`) |
| `settle:dead` (stream) | jobs that can never settle, with the reason | — | settler | people |

`{payer}` is a hash tag: a payer's keys share a slot, so one Lua script touches them
atomically, and the layout survives a move to Redis Cluster.

## Why it is shaped this way

- **No facilitator on the request path.** Its `/verify` takes ~300–400 ms; Envoy's
  per-message budget is 200 ms. The filter does locally everything `/verify` does except
  simulation, which `/settle` runs anyway — the `jwt_authn` pattern: verify locally with
  state fetched remotely and cached.
- **The balance check is mandatory.** Without it an empty wallet with valid signatures
  gets unlimited free calls. The in-flight sum closes the concurrent case: twenty
  payments against a balance worth five serve exactly five.
- **EIP-3009 only.** The spec's default; signature + balance is a complete local check
  (no allowance to revoke), buyers need no approve transaction and no gas, and the chain
  records every nonce, which is what the settler's uncertain-path check reads. Permit2 is
  additive, in the schema already.
- **Settle after the response.** You cannot serve and settle inside the budget (measured
  `/settle`: 0.6–1.1 s). The receipt is the spec's `SettlementResponse` shape with
  `transaction: ""` and a `pending` extension; radius clients report "paid" on it. This
  is the project's deliberate deviation, and its thesis.
- **The nonce is never released.** A refused or failed request costs the buyer a new
  signature, which x402 clients make on every attempt anyway. Replay safety over
  convenience; traefik-x402's issue #1 was the other choice.
- **Fail closed, always, at the Envoy level.** `failOpen: false`, so a hung filter is a
  504, never a free call. Zero ready filter pods is the worst case (Envoy Gateway rewrites
  the route to a 500), hence two replicas and a PDB. The filter's own answers for its own
  dependencies are 503 + `Retry-After`, never a charge.
- **The filter reads the price table through an API watch, never a mounted volume.** A
  mounted ConfigMap refreshes on the kubelet's sync period (up to a minute); a watch
  delivers in under a second — the apply → 402 number above. RBAC is `list/watch` on
  that one ConfigMap by name, which needs the watch to send a `metadata.name` selector.
- **One owned `EnvoyExtensionPolicy` per policy.** Per-route status, ownership and
  deletion through Kubernetes GC; a route with no policy never pays the ext_proc hop.
  The controller also owns the ReferenceGrant: a cross-namespace reference is an opt-in
  by the target's owner, which is us.
- **Server-Side Apply and a full recompute.** The controller states the fields it wants
  and the API server merges; a hand edit to a field it owns is put back, fields it never
  set are left alone, and with no incremental edits there is nothing to drift. If the
  controller is down, the last table and the existing EEPs keep working.
- **Intent in the CRD, knobs in the controller.** A team writes `network: testnet`,
  `price`, `payTo`. The chain ID, the token contract, the Envoy timeouts and the filter
  address are the controller's, so a team cannot weaken enforcement from its own
  namespace. The schema (pattern, enum, bounds) refuses mistakes at `kubectl apply`.
- **The price table is the contract**, validated by one parser on both sides: the
  controller runs it before writing, the filter before loading. A table that fails is
  rejected whole and the last good one stays; a typo never makes a priced route free.

## The request budget

| Step | Measured | Where |
|---|---|---|
| Envoy → filter hop | ~4 ms | M1 drill |
| signature verify (pure Go, `CGO_ENABLED=0`) | ~0.1 ms | M2 |
| Redis `SET NX` / Lua script | p50 0.10 ms, p99 0.79 ms | M1 baseline |
| `balanceOf`, warm connection, keyless | p50 ~60 ms, p99 ~120 ms, 0.74% > 140 ms | M1 baseline |
| `balanceOf`, cold connection | p50 ~265 ms, never fits | M1 baseline → keepalive every 20 s |
| facilitator `/verify` (not on the path) | ~390 ms | M2 |
| facilitator `/settle` (the settler) | 0.6–1.1 s | M2 |

Filter deadline 150 ms under Envoy's 200 ms. A cache hit costs two Redis round trips; a
miss adds one RPC read; a returning payer is a hit.

## What a backend sees

Nothing new to implement. A paid request arrives with `x-x402-payer: 0x…` and
`x-x402-payment-id: 0x…` (only the filter writes them; a client-sent copy is overwritten)
and without `PAYMENT-SIGNATURE`. The response goes back with `PAYMENT-RESPONSE` and
`Cache-Control: private, no-store` added. httpbin's `/get` echoes the two headers, which
is the demo.

## Repository map

| Path | What |
|---|---|
| `api/v1alpha1/` | the `PaymentPolicy` type; the CRD is generated from its markers into `deploy/k8s/crd/` |
| `internal/controller/` | the reconciler (envtest suite against kube-apiserver 1.35 with the real EEP CRD) |
| `internal/pricing/` | the price table: schema, validation, the ConfigMap watch |
| `internal/paywall/` | the 402: header, JSON, HTML, markdown, how-to-pay |
| `internal/x402/`, `internal/evm/` | V2 wire types, header codecs, EIP-3009 and the local verifier, secp256k1 |
| `internal/balance/`, `internal/reserve/`, `internal/rdb/` | the balance cache, the Lua reserve/release and the capture stream, Redis client options |
| `internal/extproc/` | the ext_proc server, both phases |
| `internal/settle/`, `internal/facilitator/`, `internal/radius/` | the settler, the facilitator client, Radius profiles and the RPC client |
| `internal/buyer/`, `cmd/x402-buyer/` | our own x402 buyer: signs, pays, drips from the faucet (the done-checks' client) |
| `cmd/x402-filter/`, `cmd/x402-controller/`, `cmd/x402-settler/` | the three shipped binaries, one Dockerfile |
| `deploy/k8s/`, `deploy/terraform/` | manifests (apply order in its README), the GKE project |
| `docs/design/` | the request-path budget and the measurements; the escrow and batch-settlement notes |

## Status

**Live on GKE against Radius testnet, checked 2026-11-04:**

| Check | Result |
|---|---|
| `kubectl apply` a PaymentPolicy → 402 | 0.13–0.39 s |
| pay with EIP-3009 → 200 | yes; httpbin echoes `x-x402-payer`; `PAYMENT-RESPONSE` pending receipt |
| settlement on chain | the settler logged the explorer link ~1 s after the response; the wallet moved 0.001 per call |
| replay the same signature | 402 `payment_already_used`, every time after the first |
| an unfunded wallet | 402 `insufficient_funds` |
| 10 concurrent payments against a balance worth 5 | exactly 5 served and settled, 5 refused; the wallet ended at 0 |
| `kubectl delete` the policy → 200 | ≤ 0.14 s |
| controller pod deleted | nothing changes; policies applied in the next ~19 s wait for the Lease |

Two findings from the first live run, fixed the same day: the testnet RPC answered in
130–270 ms (the October baseline was 60–80 ms), so a first-time payer gets the one-time
503 + `Retry-After` more often than the 0.74% the budget assumed; and the filter had
waited for a stale entry's refresh until its deadline, starving the reserve step —
a stale entry is now served at once and refreshed in the background. Whether to raise
Envoy's `messageTimeout` and the filter deadline (300/250 ms) is open.

Not yet: a settler killed mid-queue (the `XAUTOCLAIM` re-drive, tested on miniredis
only), a second buyer through `radius-cli`, metrics. After M5: the hardening backlog
(loss limit, metrics, Permit2, TLS on the hop, a receipts endpoint), the showcase, and
the testnet soak that gates mainnet.
