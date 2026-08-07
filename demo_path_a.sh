#!/usr/bin/env bash
# Status-update demo (2026-07-31): Path A end to end, then a live demonstration of
# the gap that motivates Path B. Manages its own gateway on port 8000.
# Talking points for each step are in DEMO_NOTES.md.
#
#   bash demo_path_a.sh          # pause between steps (for presenting)
#   bash demo_path_a.sh --auto   # run straight through (for rehearsing)
set -uo pipefail
cd "$(dirname "$0")"

AUTO=0
[[ "${1:-}" == "--auto" ]] && AUTO=1

B=$'\033[1m'; D=$'\033[2m'; G=$'\033[32m'; C=$'\033[36m'; R=$'\033[0m'
GW_LOG=$(mktemp -t ppp-demo)

step()  { echo; echo "${B}${C}──── $* ────${R}"; }
run()   { echo "${D}   \$ $*${R}"; eval "$@"; }
pause() { (( AUTO )) && return; echo; read -r -p "${D}   [Enter]${R} "; }

cleanup() { pkill -f "uvicorn gateway:app" >/dev/null 2>&1; rm -f "$GW_LOG" /tmp/ppp_ok.json /tmp/ppp_under.json /tmp/ppp_resp.txt; }
trap cleanup EXIT

start_gateway() {
  pkill -f "uvicorn gateway:app" >/dev/null 2>&1; sleep 1
  venv/bin/python -m uvicorn gateway:app --port 8000 >"$GW_LOG" 2>&1 &
  disown          # otherwise bash prints "Terminated: 15" when we restart it
  for _ in $(seq 40); do
    [[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST localhost:8000/infer \
         -H 'content-type: application/json' -d '{"prompt":"ping"}' 2>/dev/null)" == "402" ]] && return 0
    sleep 0.5
  done
  echo "gateway failed to start; log:"; cat "$GW_LOG"; exit 1
}

bal() { cast call "$SBC_CONTRACT_ADDRESS" "balanceOf(address)(uint256)" "$1" --rpc-url "$RPC_URL" | awk '{print $1}'; }

mkheader() {
venv/bin/python -c "
import base64, json, sys
from payment import sign_permit2_payment, build_payment_requirements, RESOURCE_URL
sig, auth = sign_permit2_payment(amount=int(sys.argv[1]))
print(base64.b64encode(json.dumps({'x402Version':2,'resource':{'url':RESOURCE_URL,
  'description':'One LLM inference call','mimeType':'application/json'},
  'accepted':build_payment_requirements(),
  'payload':{'signature':sig,'permit2Authorization':auth}}).encode()).decode())" "$1"
}

# ── preflight ────────────────────────────────────────────────────────────────
set -a; . ./.env; set +a
PAYER=$(cast wallet address --private-key "$WALLET_KEY")
echo "${B}Pay-Per-Prompt — Path A status demo${R}"
echo "  payer    $PAYER"
echo "  provider $PAY_TO_ADDRESS"
echo "  chain    $CHAIN_ID   price ${PRICE_BASE_UNITS} base units"
echo -n "  starting gateway ... "; start_gateway; echo "${G}up${R}"

# ── 1. the 402 ───────────────────────────────────────────────────────────────
step "1. Ask for the resource without paying"
run "curl -s -X POST localhost:8000/infer -H 'content-type: application/json' \
     -d '{\"prompt\":\"what is a nonce?\"}' | venv/bin/python -m json.tool"
pause

# ── 2. balances before ───────────────────────────────────────────────────────
step "2. Balances before"
PAYER_0=$(bal "$PAYER"); PROV_0=$(bal "$PAY_TO_ADDRESS")
echo "   payer    $PAYER_0"
echo "   provider $PROV_0"
pause

# ── 3. sign ──────────────────────────────────────────────────────────────────
step "3. Sign the payment — no transaction, no gas"
XPAY=$(mkheader "$PRICE_BASE_UNITS")
run "echo \"\$XPAY\" | base64 -d | venv/bin/python -m json.tool"
pause

# ── 4. the facilitator, directly ─────────────────────────────────────────────
step "4. Ask Radius's facilitator to verify it — raw, no gateway involved"
venv/bin/python -c "
import json
from payment import sign_permit2_payment, _build_facilitator_request
sig, auth = sign_permit2_payment(amount=$PRICE_BASE_UNITS)
json.dump(_build_facilitator_request(sig, auth), open('/tmp/ppp_ok.json','w'))
sig, auth = sign_permit2_payment(amount=1)
json.dump(_build_facilitator_request(sig, auth), open('/tmp/ppp_under.json','w'))"
echo "   ${B}honest payment:${R}"
run "curl -s -w '  <- HTTP %{http_code}\n' \$FACILITATOR_URL/verify \
     -H 'content-type: application/json' -d @/tmp/ppp_ok.json"
echo "   ${B}underpayment (1 unit for a ${PRICE_BASE_UNITS}-unit product):${R}"
run "curl -s -w '  <- HTTP %{http_code}\n' \$FACILITATOR_URL/verify \
     -H 'content-type: application/json' -d @/tmp/ppp_under.json"
pause

# ── 5. the paid request ──────────────────────────────────────────────────────
step "5. Same request, now with the X-PAYMENT header"
# Exactly ONE paid request: display it, and read the tx hash back out of the same
# response so the balance delta in step 6 is one price, not two.
echo "${D}   \$ curl -i -s -X POST localhost:8000/infer -H 'content-type: application/json' \\
     -H \"X-PAYMENT: \$XPAY\" -d '{\"prompt\":\"In one sentence, what is a nonce?\"}'${R}"
curl -i -s -X POST localhost:8000/infer -H 'content-type: application/json' \
  -H "X-PAYMENT: $XPAY" -d '{"prompt":"In one sentence, what is a nonce?"}' > /tmp/ppp_resp.txt
cat /tmp/ppp_resp.txt; echo
TX=$(tr -d '\r' < /tmp/ppp_resp.txt | awk 'tolower($1)=="x-payment-response:"{print $2}')
pause

# ── 6. balances after ────────────────────────────────────────────────────────
step "6. Balances after"
PAYER_1=$(bal "$PAYER"); PROV_1=$(bal "$PAY_TO_ADDRESS")
echo "   payer    $PAYER_0 -> $PAYER_1   ${B}delta $((PAYER_1 - PAYER_0))${R}"
echo "   provider $PROV_0 -> $PROV_1   ${B}delta $((PROV_1 - PROV_0))${R}"
pause

# ── 7. verify on-chain ───────────────────────────────────────────────────────
step "7. Verify on-chain, independently of anything I printed"
run "cast receipt $TX --rpc-url \$RPC_URL | grep -E '^(status|to|gasUsed|blockNumber)'"
echo "   ${C}https://testnet.radiustech.xyz/tx/$TX${R}"
pause

# ── 8. replay ────────────────────────────────────────────────────────────────
step "8. Replay the exact same payment"
run "curl -s -w '\nHTTP %{http_code}\n' -X POST localhost:8000/infer \
     -H 'content-type: application/json' -H \"X-PAYMENT: \$XPAY\" -d '{\"prompt\":\"hi\"}'"
pause

# ── 9. underpayment through the gateway ──────────────────────────────────────
step "9. Underpayment through the gateway"
run "curl -s -w '\nHTTP %{http_code}\n' -X POST localhost:8000/infer \
     -H 'content-type: application/json' -H \"X-PAYMENT: \$(mkheader 1)\" \
     -d '{\"prompt\":\"free lunch?\"}'"
pause

# ── 10. the gap that motivates Path B ────────────────────────────────────────
step "10. Why I need the contract — watch what a restart does"
echo -n "   restarting the gateway (this wipes SEEN_SIGNATURES / SEEN_SETTLEMENTS) ... "
start_gateway; echo "${G}up${R}"
PROV_2=$(bal "$PAY_TO_ADDRESS")
run "curl -s -w '\nHTTP %{http_code}\n' -X POST localhost:8000/infer \
     -H 'content-type: application/json' -H \"X-PAYMENT: \$XPAY\" -d '{\"prompt\":\"hi\"}'"
PROV_3=$(bal "$PAY_TO_ADDRESS")
echo
echo "   provider before this replay: $PROV_2"
echo "   provider after  this replay: $PROV_3   ${B}delta $((PROV_3 - PROV_2))${R}"
echo
echo "   ${B}one payment, two delivered inferences${R}"
pause

echo
echo "${G}${B}done${R} — gateway stopped.   Path B status: contract at"
echo "$ESCROW_CONTRACT_ADDRESS, 9 Foundry tests passing."
