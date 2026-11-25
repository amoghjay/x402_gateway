# deploy/compose

The gate without Kubernetes: a plain Envoy in front of an API, and the x402 filter Envoy
asks about every request. Needs Docker with Compose v2.

```
client ──► Envoy :8080 ──(gRPC, each request)──► x402-filter ──► Redis ◄── settler
             │                                     (402, or verify, reserve, settle)
             └──► your API (httpbin in the demo), only once the filter says so
```

| Service | What it does |
|---|---|
| `envoy` | Envoy v1.40.2 on port 8080 with [`envoy.yaml`](envoy.yaml): named routes, and the ext_proc filter on all but `/anything/free` |
| `filter` | `x402-filter -prices-dir`: answers the 402, verifies payments, holds each paid response until its payment has settled. Metrics on `localhost:9090/metrics` |
| `redis` | replay and overdraft state, shared by every filter replica |
| `settler` | resolves payments whose outcome the filter did not confirm; the refund list |
| `httpbin` | the demo API; it echoes each request, including the `x-x402-payer` header a paid one gets |

## Run it

```bash
cp .env.example .env               # set PAY_TO to a wallet you control
docker compose up --build          # builds the filter and settler images from this repo
curl -i localhost:8080/anything/paid   # 402, PAYMENT-REQUIRED header and how to pay
```

Pay it with [radius-cli](https://docs.radiustech.xyz/reference/radius-cli) (Node 22 or later):

```bash
npx radius-cli@0.8.0 --network testnet wallet new       # a testnet wallet
npx radius-cli@0.8.0 --network testnet wallet faucet    # test SBC
npx radius-cli@0.8.0 --network testnet wallet pay get 'http://localhost:8080/anything/paid' --threshold 0.001
```

The 402 prints that last command for you. Then read what the gate added to the paid
request, in seconds, from the filter's metrics:

```bash
curl -s localhost:9090/metrics | grep -E 'x402_filter_phase_duration_seconds_(sum|count).*route="paid"'
```

`decision="payment_accepted"` is the request phase (checking the payment before the API is
called); `decision="settled"` is the response held while the payment settled.

`./smoke.sh` checks a running stack without paying: the 402, the free route, the refused
route and the metrics. CI runs it on every change to the gate.

## Routes and prices

Every route in `envoy.yaml` has a **name**, and the filter prices a request by its route's
name. The price table is in [`compose.yaml`](compose.yaml) (`configs.prices`):

| Route | Path | What happens |
|---|---|---|
| `paid` | `/anything/paid` | 0.001 SBC on Radius testnet to `PAY_TO`, settled before the response is released |
| `free` | `/anything/free` | the filter is switched off for this route |
| `platform` | `/anything/platform` | governed by a Radius Platform project with `compose.platform.yaml`; refused without it |
| `unlisted` | everything else | not in the table, so the filter answers 503: a route whose name does not match the table is refused, never served free |

Check a table: `docker compose run --rm filter validate /etc/x402/prices`.

## Use it with your API

1. Point the gate at it in `.env`:
   ```
   UPSTREAM_HOST=api.internal         # host.docker.internal for an API on this machine
   UPSTREAM_PORT=3000
   ```
2. Name your routes in `envoy.yaml` (keep `free`'s per-route switch for anything free).
3. Price them in `configs.prices` in `compose.yaml`, keyed by those names.
4. `docker compose up -d`. Terminate TLS where you do today (a load balancer or CDN in
   front of port 8080).

Already running Envoy? Skip this stack's Envoy: add the `envoy.filters.http.ext_proc`
block and the `x402_filter` cluster from `envoy.yaml` to yours, name your routes, and run
the filter, Redis and the settler.

## Radius Platform policies

To enforce a Radius Platform project's policy (allow, deny, rate limits, free quotas,
payment options) instead of fixed prices, set `PLATFORM_ORG`, `PLATFORM_PROJECT` and
`PLATFORM_KEY_FILE` (a file holding the project's connector key) in `.env`, then:

```bash
docker compose -f compose.yaml -f compose.platform.yaml up --build
```

`x402-controller sync` fetches the project's compiled policy every 30 s into a volume the
filter reads, keeping the last good policy if the Platform is unreachable. The project
governs the route `platform`; its rules match paths inside it. Before the first policy
arrives the route answers `503 policy_unavailable`.

## Limits of this setup

- One filter replica, since its metrics port is published on the host. Replicas share
  their state through Redis (that is what the cluster runs, two of them); running more
  here means dropping the host port and scraping each one.
- Images are built from source until public images are published.
- The demo table charges on Radius testnet only. Another network needs it in the filter's
  `-networks` flag and a route priced on it.
