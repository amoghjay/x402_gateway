#!/usr/bin/env bash
# The five-minute walkthrough against the live gateway: a 402, a free route, a paid call,
# a replay refused, and the settlement on chain. With DEMO_KUBE=1 it also deletes and
# re-applies the demo PaymentPolicy to show a route going free and priced again.
#
#   make build                                   # bin/x402-buyer
#   bin/x402-buyer keygen && bin/x402-buyer drip # a testnet wallet with ~0.5 SBC
#   tools/demo/walkthrough.sh                    # Enter between steps; DEMO_FAST=1 runs straight through
#
# Spends 0.002 testnet SBC per run (one paid call, one replayed signature that pays once).
set -euo pipefail

BASE="${BASE:-http://x402-22ade072.nip.io}"
BUYER="${BUYER:-bin/x402-buyer}"
URL="$BASE/get"

bold() { printf '\n\033[1m%s\033[0m\n' "$*"; }
run() { printf '\033[2m$ %s\033[0m\n' "$*"; "$@"; }
pause() { [[ "${DEMO_FAST:-}" == 1 ]] || read -r -p "  [Enter] " _; }
status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }

[[ -x "$BUYER" ]] || { echo "no $BUYER: run make build" >&2; exit 1; }

bold "1. No payment: the gateway answers 402, the backend never sees the request"
run curl -s -o /dev/null -w 'HTTP %{http_code}, %{time_total}s\n' "$URL"
curl -s "$URL" | jq '{error, accepts: [.accepts[] | {network, amount, payTo}]}'
pause

bold "2. A route without a PaymentPolicy never reaches the filter"
run curl -s -o /dev/null -w 'HTTP %{http_code}\n' "$BASE/anything/free"
pause

bold "3. Pay: sign EIP-3009, retry, get the response"
run "$BUYER" balance
# A first-time payer can get one 503 + Retry-After while the gateway caches its balance;
# nothing is charged, and the retry signs a fresh authorization.
if ! out=$("$BUYER" pay "$URL"); then
  sleep 1
  out=$("$BUYER" pay "$URL")
fi
echo "$out" | jq '.headers | with_entries(select(.key | test("^X-X402"; "i")))'
pause

bold "4. Replay: the same PAYMENT-SIGNATURE twice; the second is refused"
sig=$("$BUYER" sign "$URL")
for i in 1 2; do
  code=$(status -H "PAYMENT-SIGNATURE: $sig" "$URL")
  echo "attempt $i: HTTP $code"
done
curl -s -H "PAYMENT-SIGNATURE: $sig" "$URL" | jq -r '.error'
pause

bold "5. Settled after the response, off the request path"
sleep 2
run "$BUYER" balance

if [[ "${DEMO_KUBE:-}" == 1 ]]; then
  pause
  bold "6. Delete the PaymentPolicy: the route is free; apply it: priced again"
  run kubectl delete -f deploy/k8s/demo/40-paymentpolicy.yaml
  echo "HTTP $(status "$URL")"
  run kubectl apply -f deploy/k8s/demo/40-paymentpolicy.yaml
  for _ in $(seq 20); do
    [[ "$(status "$URL")" == 402 ]] && break
    sleep 0.1
  done
  echo "HTTP $(status "$URL")"
  run kubectl get paymentpolicy -n demo
fi
