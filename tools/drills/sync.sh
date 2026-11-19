#!/usr/bin/env bash
# Drills for captureMode: Sync routes, against a live gateway. Each one tries to get a
# response without paying for it and reports what the gateway answered.
#
#   tools/drills/sync.sh burst   NETWORK URL   # one signed payment, 12 copies at once
#   tools/drills/sync.sh replay  NETWORK URL   # settle outside the gate, then send it through
#   tools/drills/sync.sh sample  NETWORK URL N # N paid requests, one after another
#
# NETWORK is testnet or base-sepolia; the buyer key is ~/.x402/buyer-<network>.key and
# must hold enough of the network's token (every drill spends at most one price, sample
# spends N). Needs bin/x402-buyer (make build), curl and python3.
set -euo pipefail

BUYER=${BUYER:-bin/x402-buyer}
cmd=${1:?burst, replay or sample}
net=${2:?network}
url=${3:?url}

balance() { "$BUYER" balance -network "$net" 2>&1 | tail -1 | awk '{print $1, $2}'; }
sign() { "$BUYER" sign -network "$net" "$url" 2>/dev/null | tail -1; }
facilitator() {
  case "$net" in
    testnet) echo https://facilitator.testnet.radiustech.xyz ;;
    base-sepolia) echo https://x402.org/facilitator ;;
    *) echo "no facilitator known for $net" >&2; exit 2 ;;
  esac
}

case "$cmd" in
  burst)
    # Pass: exactly one 200. The Redis nonce claim is what decides between the copies.
    sig=$(sign)
    [ -n "$sig" ] || { echo "could not sign (is the route priced? is the wallet funded?)" >&2; exit 1; }
    echo "balance before: $(balance)"
    for _ in $(seq 1 12); do
      curl -sS -m 30 -o /dev/null -w '%{http_code}\n' -H "PAYMENT-SIGNATURE: $sig" "$url" &
    done | sort | uniq -c
    wait
    sleep 3
    echo "balance after:  $(balance)   (pass: exactly one 200, and one price spent)"
    ;;
  replay)
    # Pass: 402 from the gate, with no backend body. The payment is spent on chain before
    # the gate ever sees it, so its Redis claim does not exist: only the chain read stops it.
    sig=$(sign)
    [ -n "$sig" ] || { echo "could not sign" >&2; exit 1; }
    body=$(python3 -c 'import sys,base64,json; pp=json.loads(base64.b64decode(sys.argv[1]+"==")); print(json.dumps({"x402Version":2,"paymentPayload":pp,"paymentRequirements":pp["accepted"]}))' "$sig")
    echo "settle outside the gate:"
    curl -sS -m 30 -X POST "$(facilitator)/settle" -H 'content-type: application/json' --data "$body" | cut -c1-160; echo
    echo "the same settle again (does the facilitator refuse it?):"
    curl -sS -m 30 -X POST "$(facilitator)/settle" -H 'content-type: application/json' --data "$body" | cut -c1-160; echo
    echo "through the gate:"
    curl -sS -m 30 -o /dev/null -w '%{http_code}   (pass: 402)\n' -H "PAYMENT-SIGNATURE: $sig" "$url"
    ;;
  sample)
    n=${4:?how many}
    echo "balance before: $(balance)"
    for _ in $(seq 1 "$n"); do
      # Capture first: piping into grep -m1 lets grep exit before the body is written, and
      # under pipefail the SIGPIPE turned a served request into an extra "no answer" line.
      out=$("$BUYER" pay -network "$net" "$url" 2>&1) || true
      grep -m1 '^HTTP' <<<"$out" || echo "no answer"
    done | sort | uniq -c
    echo "balance after:  $(balance)"
    echo "settle times are in the filter's log: decision=settled, field ms"
    ;;
  *) echo "unknown drill $cmd" >&2; exit 2 ;;
esac
