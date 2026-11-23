#!/usr/bin/env bash
# The Phase 1 exit test for a route governed by a Radius Platform project, against a live
# gateway. Prints one PASS or FAIL line per check, with what was expected and what came
# back, and exits non-zero if any check failed.
#
#   tools/drills/platform.sh routes       BASE   # no money: free, denied, charged, metered, rate limited
#   tools/drills/platform.sh pay  NETWORK BASE   # one paid request in NETWORK's option, then its replay
#   tools/drills/platform.sh all          BASE   # routes, then pay in every EIP-3009 option offered
#
# BASE is the route's prefix, e.g. http://GATEWAY/anything/platform. The project needs the
# five endpoints of deploy/k8s/demo-platform/README.md; the defaults below are the paths
# and limits the design note's runs used (docs/design/request-path-budget.md, "Policy
# routes"). Override any of them:
#
#   FREE_PATH=/free          allow                                  → 200
#   DENIED_PATH=/blocked     deny 403 DENIED_CODE=not_for_sale      → 403 {"code":"not_for_sale"}
#   CHARGED_PATH=/paid/42    charge                                 → 402 listing OPTIONS=3 options
#   METERED_PATH=/metered    METERED_FREE=3 free a minute per IP    → 200 × 3, then 402
#   LIMITED_PATH=/limited    LIMIT=5 a minute, one shared counter   → 200 × 5, then 429 + Retry-After
#
# The quota and the limit count per minute, and the limit counts everyone's requests: run
# `routes` again only after a minute, and with no other traffic on LIMITED_PATH.
#
# NETWORK is testnet or base-sepolia; the buyer key is ~/.x402/buyer-<network>.key and must
# hold one price of the option's token. `pay` spends one price; `all` one per EIP-3009
# option. Needs bin/x402-buyer (make build), curl and python3.
#
# Not covered here: the exit test's policy edit in the Platform (change a rule, see the
# gate follow within a minute with no kubectl). That one is done by hand.
set -euo pipefail

BUYER=${BUYER:-bin/x402-buyer}
FREE_PATH=${FREE_PATH:-/free}
DENIED_PATH=${DENIED_PATH:-/blocked}
DENIED_CODE=${DENIED_CODE:-not_for_sale}
CHARGED_PATH=${CHARGED_PATH:-/paid/42}
OPTIONS=${OPTIONS:-3}
METERED_PATH=${METERED_PATH:-/metered}
METERED_FREE=${METERED_FREE:-3}
LIMITED_PATH=${LIMITED_PATH:-/limited}
LIMIT=${LIMIT:-5}

cmd=${1:?routes, pay or all}
case "$cmd" in
  pay) net=${2:?network}; base=${3:?base URL} ;;
  routes | all) base=${2:?base URL} ;;
  *) echo "unknown drill $cmd" >&2; exit 2 ;;
esac
base=${base%/}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
checks=0
fails=0

# check NAME EXPECTED GOT
check() {
  checks=$((checks + 1))
  if [ "$2" = "$3" ]; then
    echo "PASS  $1: expected $2, got $3"
  else
    echo "FAIL  $1: expected $2, got $3"
    fails=$((fails + 1))
  fi
}
note() { echo "      $*"; }

# get URL [curl args...]: sets code; the headers go to $tmp/h, the body to $tmp/b.
get() {
  local url=$1
  shift
  : >"$tmp/h"
  : >"$tmp/b"
  if ! code=$(curl -sS -m 30 -o "$tmp/b" -D "$tmp/h" -w '%{http_code}' "$@" "$url" 2>"$tmp/curl"); then
    code=no-answer
    note "$(head -1 "$tmp/curl")" >&2
  fi
}

# hdr NAME: that header's value in the last response, or nothing.
hdr() {
  awk -v n="$(tr '[:upper:]' '[:lower:]' <<<"$1")" '
    { line = $0; sub(/\r$/, "", line); i = index(line, ":") }
    i > 0 && tolower(substr(line, 1, i - 1)) == n { v = substr(line, i + 1); sub(/^[ \t]+/, "", v); print v; exit }
  ' "$tmp/h"
}

# b64json FIELD HEADER-VALUE: one field of a base64 JSON header (PAYMENT-REQUIRED or
# PAYMENT-RESPONSE), as the checks below compare it.
b64json() {
  python3 -c '
import base64, json, sys
what, h = sys.argv[1], sys.argv[2]
if not h:
    print("none"); sys.exit()
d = json.loads(base64.b64decode(h + "=" * (-len(h) % 4)))
method = lambda a: (a.get("extra") or {}).get("assetTransferMethod") or "eip3009"
if what == "count":       # PAYMENT-REQUIRED: how many options
    print(len(d.get("accepts") or []))
elif what == "reason":    # PAYMENT-REQUIRED: the refusal code before "payment refused"
    e = d.get("error") or ""
    print(e.split(":", 1)[0] if ": payment refused" in e else "none")
elif what == "options":   # PAYMENT-REQUIRED: one line per option
    for a in d.get("accepts") or []:
        print(a.get("network"), method(a), a.get("asset"), a.get("amount"))
elif what == "eip3009":   # PAYMENT-REQUIRED: the networks of the EIP-3009 options
    for a in d.get("accepts") or []:
        if method(a) == "eip3009":
            print(a.get("network"))
elif what == "success":   # PAYMENT-RESPONSE
    print("success" if d.get("success") else "failure " + str(d.get("errorReason") or ""))
elif what == "settlement":
    print(d.get("network"), d.get("transaction") or "(no transaction)")
' "$1" "$2"
}

# bodycode: the "code" of a JSON body, or what the body starts with.
bodycode() {
  python3 -c '
import json, sys
raw = open(sys.argv[1], "rb").read()
if not raw:
    print("(no body)"); sys.exit()
try:
    print(json.loads(raw).get("code") or "none")
except Exception:
    print(repr(raw[:60]))
' "$tmp/b"
}

# series PATH N: N requests one after another; prints each status, space separated, and
# leaves the last response in $tmp.
series() {
  local out=() i
  for i in $(seq 1 "$2"); do
    get "$base$1"
    out+=("$code")
    [ "$i" -lt "$2" ] && note "#$i: $code, RateLimit-Remaining $(hdr RateLimit-Remaining)" >&2
  done
  echo "${out[*]}"
}

# repeat WORD N: WORD N times, space separated.
repeat() { local i out=(); for i in $(seq 1 "$2"); do out+=("$1"); done; echo "${out[*]}"; }

routes() {
  get "$base$FREE_PATH"
  check "free $FREE_PATH" 200 "$code"

  get "$base$DENIED_PATH"
  check "denied $DENIED_PATH" "403 $DENIED_CODE" "$code $(bodycode)"

  get "$base$CHARGED_PATH"
  check "charged $CHARGED_PATH" "402 listing $OPTIONS options" "$code listing $(b64json count "$(hdr PAYMENT-REQUIRED)") options"
  b64json options "$(hdr PAYMENT-REQUIRED)" | while read -r line; do [ "$line" = none ] || note "option: $line"; done

  local got
  got=$(series "$METERED_PATH" $((METERED_FREE + 1)))
  check "metered $METERED_PATH" "$(repeat 200 "$METERED_FREE") 402" "$got"
  note "last: RateLimit-Limit $(hdr RateLimit-Limit), RateLimit-Remaining $(hdr RateLimit-Remaining)"
  case "$got" in 402*) note "a 402 first: this minute's quota may be spent already; rerun after a minute" ;; esac

  got=$(series "$LIMITED_PATH" $((LIMIT + 1)))
  check "rate-limited $LIMITED_PATH" "$(repeat 200 "$LIMIT") 429" "$got"
  local retry
  retry=$(hdr Retry-After)
  check "rate-limited $LIMITED_PATH Retry-After" "a value" "$([ -n "$retry" ] && echo "a value" || echo none)"
  [ -z "$retry" ] || note "Retry-After: $retry"
  case "$got" in 429*) note "a 429 first: the shared counter may be spent already; rerun after Retry-After" ;; esac
}

# pay NETWORK: one payment in that network's option, then the same payment again.
pay() {
  local net=$1 url="$base$CHARGED_PATH" sig
  sig=$("$BUYER" sign -network "$net" "$url" 2>"$tmp/sign" | tail -1) || true
  if [ -z "$sig" ]; then
    check "pay $net: sign" "a signed payment" "none ($(tail -1 "$tmp/sign"))"
    return
  fi

  get "$url" -H "PAYMENT-SIGNATURE: $sig"
  local receipt
  receipt=$(hdr PAYMENT-RESPONSE)
  check "pay $net $CHARGED_PATH" "200 with PAYMENT-RESPONSE success" "$code with PAYMENT-RESPONSE $(b64json success "$receipt")"
  [ -n "$receipt" ] && note "settled: $(b64json settlement "$receipt")"

  get "$url" -H "PAYMENT-SIGNATURE: $sig"
  local pr
  pr=$(hdr PAYMENT-REQUIRED)
  check "replay $net $CHARGED_PATH" "402 payment_already_used, $OPTIONS options" "$code $(b64json reason "$pr"), $(b64json count "$pr") options"
}

# buyer-network CAIP-2: the x402-buyer -network name for an option's network.
buyer_network() {
  case "$1" in
    eip155:72344) echo testnet ;;
    eip155:84532) echo base-sepolia ;;
    *) echo "" ;; # mainnet and anything else: not paid by a drill
  esac
}

case "$cmd" in
  routes) routes ;;
  pay) pay "$net" ;;
  all)
    routes
    get "$base$CHARGED_PATH"
    nets=$(b64json eip3009 "$(hdr PAYMENT-REQUIRED)")
    [ "$nets" = none ] && nets=""
    [ -n "$nets" ] || check "EIP-3009 options on $CHARGED_PATH" "at least one" "none"
    for n in $nets; do
      bn=$(buyer_network "$n")
      if [ -z "$bn" ]; then
        check "pay $n" "a test network the buyer pays" "$n"
        continue
      fi
      pay "$bn"
    done
    ;;
esac

echo "$checks checks, $fails failed"
[ "$fails" -eq 0 ]
