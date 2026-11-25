#!/usr/bin/env bash
# Checks a running compose gate (compose.yaml, fixed prices) without spending anything:
# the paid route asks for a payment, the free route skips the filter, an unlisted route is
# refused, and the filter's metrics counted each. CI runs it on every change to the gate.
#
#   docker compose up -d --build && ./smoke.sh
set -euo pipefail

base=${BASE:-http://localhost:8080}
metrics=${METRICS:-http://localhost:9090/metrics}
fails=0

check() { # NAME EXPECTED GOT
  if [ "$2" = "$3" ]; then
    echo "PASS  $1"
  else
    echo "FAIL  $1: expected $2, got $3"
    fails=$((fails + 1))
  fi
}

# Envoy answers 503 until the filter is up and has read the price table.
code=000
for _ in $(seq 1 60); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "$base/anything/paid" || true)
  [ "$code" = 402 ] && break
  sleep 2
done
check "paid route asks for a payment" 402 "$code"

required=$(curl -s -D - -o /dev/null "$base/anything/paid" | tr -d '\r' | awk 'tolower($1) == "payment-required:" { print $2 }')
network=$(printf '%s' "$required" | base64 -d | python3 -c 'import json, sys; print(json.load(sys.stdin)["accepts"][0]["network"])')
check "the 402 offers SBC on Radius testnet" eip155:72344 "$network"

check "free route is served without the filter" 200 "$(curl -s -o /dev/null -w '%{http_code}' "$base/anything/free")"
check "unlisted route is refused" "x402 filter: route not priced|503" "$(curl -s -w '|%{http_code}' "$base/get" | tr -d '\n')"

m=$(curl -s "$metrics")
grep -q 'x402_filter_decisions_total{decision="payment_required",phase="request",reason="",route="paid"}' <<<"$m" && got=yes || got=no
check "metrics count the 402 on route paid" yes "$got"
grep -q 'x402_filter_decisions_total{decision="unpriced_refused",phase="request",reason="",route="unlisted"}' <<<"$m" && got=yes || got=no
check "metrics count the refusal on route unlisted" yes "$got"

echo "$fails failed"
[ "$fails" = 0 ]
