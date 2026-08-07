#!/usr/bin/env bash
# Final demo (2026-08-07): the whole arc in one script.
#
#   Path A works and settles real money
#     -> here is a gap in it I CANNOT close at the application layer
#       -> that gap is why I wrote InferenceEscrow
#         -> and here is the same attack failing against the contract
#
# Talking points per step are in DEMO_NOTES.md. Manages its own gateway on :8000.
#
#   bash demo_final.sh              # pause between steps (presenting)
#   bash demo_final.sh --auto       # straight through (rehearsing)
#   bash demo_final.sh --list       # show the steps and exit
#   bash demo_final.sh --from 6     # start at step 6
#   bash demo_final.sh --only 5     # run exactly one step
#   bash demo_final.sh --only 1,2,5,8,10,12   # the short version, if time is tight
#
# Every step is independently runnable: steps that need a spent payment from an
# earlier step will quietly create one first (see need_* helpers).
set -uo pipefail
cd "$(dirname "$0")"

TOTAL_STEPS=12

AUTO=0; FROM=1; ONLY=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --auto) AUTO=1 ;;
    --from) FROM="$2"; shift ;;
    --only) ONLY="$2"; shift ;;
    --list) LIST=1 ;;
    *) echo "unknown arg: $1"; exit 2 ;;
  esac
  shift
done

B=$'\033[1m'; D=$'\033[2m'; G=$'\033[32m'; Y=$'\033[33m'; C=$'\033[36m'; R=$'\033[0m'
GW_LOG=$(mktemp -t ppp-final)
STATE=$(mktemp -d -t ppp-state)

step()  { echo; echo "${B}${C}──── $* ────${R}"; }
run()   { echo "${D}   \$ $*${R}"; eval "$@"; }
note()  { echo "   ${Y}$*${R}"; }
pause() { (( AUTO )) && return; echo; read -r -p "${D}   [Enter]${R} "; }

cleanup() { pkill -f "uvicorn gateway:app" >/dev/null 2>&1; rm -rf "$GW_LOG" "$STATE"; }
trap cleanup EXIT

# ── step titles (also used by --list) ────────────────────────────────────────
title_1="The 402 — both schemes are live now"
title_2="Path A: pay with a Permit2 signature"
title_3="Path A: prove it on-chain, independently"
title_4="Path A: replay the same payment"
title_5="THE GAP: restart the gateway, replay the spent payment"
title_6="The contract: what settle() actually enforces"
title_7="Path B: one deposit funds many prompts"
title_8="Path B: three prompts, three nonces, operator settles"
title_9="Path B: prove it on-chain, independently"
title_10="THE PAYOFF: restart, replay — the CHAIN refuses"
title_11="Adversarial probes: every attack that used to work"
title_12="Path A vs Path B — the honest comparison"

if [[ -n "${LIST:-}" ]]; then
  echo "${B}Pay-Per-Prompt — final demo steps${R}"
  for i in $(seq 1 $TOTAL_STEPS); do
    eval "t=\$title_$i"; printf '  %2d. %s\n' "$i" "$t"
  done
  echo
  echo "  short version if time is tight:  --only 1,2,5,8,10,12"
  exit 0
fi

# ── infrastructure ───────────────────────────────────────────────────────────
start_gateway() {
  pkill -f "uvicorn gateway:app" >/dev/null 2>&1; sleep 1
  venv/bin/python -m uvicorn gateway:app --port 8000 >"$GW_LOG" 2>&1 &
  disown          # otherwise bash prints "Terminated: 15" when we restart it
  for _ in $(seq 40); do
    [[ "$(curl -s -o /dev/null -w '%{http_code}' -X POST localhost:8000/infer \
         -H 'content-type: application/json' -d '{"prompt":"ping"}' 2>/dev/null)" == "402" ]] && return 0
    sleep 0.5
  done
  echo "${R}gateway failed to start; log:"; cat "$GW_LOG"; exit 1
}

bal()  { cast call "$SBC_CONTRACT_ADDRESS" "balanceOf(address)(uint256)" "$1" --rpc-url "$RPC_URL" | awk '{print $1}'; }
tab()  { cast call "$ESCROW_CONTRACT_ADDRESS" "balances(address)(uint256)" "$1" --rpc-url "$RPC_URL" | awk '{print $1}'; }

# Path A payment header for `amount` base units.
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

# Path B header. Writes the state files directly rather than returning them:
# nonces are 256-bit, and bash printf overflows trying to hex-format one.
mkescrow() { # mkescrow <amount> -> $STATE/{b_header,b_nonce,b_nonce_short}
venv/bin/python - "$1" "$STATE" <<'PY'
import base64, json, sys
from payment import sign_escrow_authorization, build_escrow_requirements, RESOURCE_URL

amount, state = int(sys.argv[1]), sys.argv[2]
sig, auth = sign_escrow_authorization(amount=amount)
header = base64.b64encode(json.dumps({
    "x402Version": 2,
    "resource": {"url": RESOURCE_URL, "description": "One LLM inference call",
                 "mimeType": "application/json"},
    "accepted": build_escrow_requirements(),
    "payload": {"signature": sig, "escrowAuthorization": auth},
}).encode()).decode()

open(f"{state}/b_header", "w").write(header)
open(f"{state}/b_nonce", "w").write(auth["nonce"])
open(f"{state}/b_nonce_short", "w").write(f"0x{int(auth['nonce']):064x}"[:14] + "…")
PY
}

completion_of() { # completion_of <response-file> -> the completion string
  # Read bytes, not text: text mode translates \r\n to \n, so the header/body
  # split has to happen before Python normalises the line endings.
  venv/bin/python - "$1" <<'PY'
import json, sys

raw = open(sys.argv[1], "rb").read().replace(b"\r\n", b"\n")
body = raw.split(b"\n\n", 1)[-1]
try:
    print(json.loads(body)["completion"])
except (ValueError, KeyError):
    print(f"(no completion in response: {body[:120]!r})")
PY
}

pay() { # pay <header> <prompt> -> full response (headers+body) on stdout
  curl -i -s -X POST localhost:8000/infer -H 'content-type: application/json' \
    -H "X-PAYMENT: $1" -d "{\"prompt\":\"$2\"}"
}

txof() { tr -d '\r' < "$1" | awk 'tolower($1)=="x-payment-response:"{print $2}'; }

# ── lazy dependencies, so any step can run standalone ────────────────────────
need_path_a_spent() {
  [[ -f "$STATE/a_header" ]] && return
  echo -n "   ${D}(setting up: signing and spending a Path A payment first) ...${R} "
  mkheader "$PRICE_BASE_UNITS" > "$STATE/a_header"
  pay "$(cat "$STATE/a_header")" "setup" > "$STATE/a_resp"
  txof "$STATE/a_resp" > "$STATE/a_tx"
  echo "${G}done${R}"
}

need_escrow_spent() {
  [[ -f "$STATE/b_header" ]] && return
  echo -n "   ${D}(setting up: signing and spending an escrow authorization first) ...${R} "
  mkescrow "$PRICE_BASE_UNITS"
  pay "$(cat "$STATE/b_header")" "setup" > "$STATE/b_resp"
  txof "$STATE/b_resp" > "$STATE/b_tx"
  echo "${G}done${R}"
}

# ── steps ────────────────────────────────────────────────────────────────────
step_1() {
  run "curl -s -X POST localhost:8000/infer -H 'content-type: application/json' \
       -d '{\"prompt\":\"what is a nonce?\"}' | venv/bin/python -m json.tool"
  note "Two entries in accepts[]. Last week the second one was a placeholder."
  note "Today both settle real money. The caller chooses."
}

step_2() {
  PAYER_0=$(bal "$PAYER"); PROV_0=$(bal "$PAY_TO_ADDRESS")
  echo "   payer    $PAYER_0"
  echo "   provider $PROV_0"
  echo
  mkheader "$PRICE_BASE_UNITS" > "$STATE/a_header"
  note "The payer signs typed data. No transaction, no gas."
  echo
  echo "${D}   \$ curl -i -s ... -H \"X-PAYMENT: \$XPAY\" -d '{\"prompt\":\"...\"}'${R}"
  pay "$(cat "$STATE/a_header")" "In one sentence, what is a nonce?" > "$STATE/a_resp"
  cat "$STATE/a_resp"; echo
  txof "$STATE/a_resp" > "$STATE/a_tx"
  PAYER_1=$(bal "$PAYER"); PROV_1=$(bal "$PAY_TO_ADDRESS")
  echo
  echo "   payer    $PAYER_0 -> $PAYER_1   ${B}delta $((PAYER_1 - PAYER_0))${R}"
  echo "   provider $PROV_0 -> $PROV_1   ${B}delta $((PROV_1 - PROV_0))${R}"
  note "Exactly the advertised price. No fee skimmed, nothing approximate."
}

step_3() {
  need_path_a_spent
  local tx; tx=$(cat "$STATE/a_tx")
  run "cast receipt $tx --rpc-url \$RPC_URL | grep -E '^(status|to|from|gasUsed|blockNumber)'"
  echo "   ${C}https://testnet.radiustech.xyz/tx/$tx${R}"
  note "to = $X402_PROXY_ADDRESS, the x402ExactPermit2Proxy."
  note "Not my gateway, not the payer. The facilitator submitted it and paid the gas."
}

step_4() {
  need_path_a_spent
  run "curl -s -w '\nHTTP %{http_code}\n' -X POST localhost:8000/infer \
       -H 'content-type: application/json' -H \"X-PAYMENT: \$(cat $STATE/a_header)\" \
       -d '{\"prompt\":\"hi\"}'"
  note "409. But be precise about where that came from:"
  note "the facilitator is IDEMPOTENT — replay a payload and it returns the same"
  note "cached tx hash and reports success. It sees no error at all."
  note "So this 409 came from a Python set() in my process. Step 5 pays that off."
}

step_5() {
  need_path_a_spent
  echo "   The 409 you just saw lived entirely in my process's memory."
  echo "   So let me restart the server and replay the same, already-spent payment."
  echo
  echo -n "   restarting the gateway (wipes SEEN_SIGNATURES / SEEN_SETTLEMENTS) ... "
  start_gateway; echo "${G}up${R}"
  local p0 p1
  p0=$(bal "$PAY_TO_ADDRESS")
  run "curl -s -w '\nHTTP %{http_code}\n' -X POST localhost:8000/infer \
       -H 'content-type: application/json' -H \"X-PAYMENT: \$(cat $STATE/a_header)\" \
       -d '{\"prompt\":\"hi\"}'"
  p1=$(bal "$PAY_TO_ADDRESS")
  echo
  echo "   provider before this replay: $p0"
  echo "   provider after  this replay: $p1   ${B}delta $((p1 - p0))${R}"
  echo
  echo "   ${B}${Y}One payment. Two delivered inferences. The provider was paid once.${R}"
  note "I don't even need a restart — a second instance behind a load balancer has"
  note "an empty set too. A database moves this problem into my infrastructure;"
  note "it does not remove it. That is the argument for putting it on-chain."
}

step_6() {
  note "56 lines of logic. The four require()s are the whole security argument."
  run "sed -n '69,87p' contracts/src/InferenceEscrow.sol"
  echo
  note "msg.sender == auth.settler  — only the wallet the payer named can redeem it"
  note "block.timestamp <= deadline — a leaked signature does not stay live"
  note "ECDSA.recover(digest, sig)  — the PAYER is derived from the signature,"
  note "                              never passed in, so it cannot be spoofed"
  note "!nonceUsed[payer][nonce]    — replay protection as ON-CHAIN state"
  echo
  # --root, not `cd`: run() evals in this shell, so a cd here would leak into
  # every later step and break the relative venv/ paths.
  run "forge test --root contracts 2>&1 | tail -14"
}

step_7() {
  local before after dep tx
  before=$(tab "$PAYER")
  echo "   payer's on-chain tab before: $before base units"
  echo
  # Deposit for real every run, so the room sees an actual receipt rather than a
  # cached balance. Nothing is lost: withdraw() returns whatever goes unspent.
  dep=$(venv/bin/python -c "
import json
from payment import deposit_to_escrow
print(json.dumps(deposit_to_escrow($((PRICE_BASE_UNITS * 5)))))")
  tx=$(venv/bin/python -c "import json,sys; print(json.loads(sys.argv[1])['transaction'])" "$dep")
  after=$(tab "$PAYER")
  echo "   ${B}deposit tx${R} ${C}https://testnet.radiustech.xyz/tx/$tx${R}"
  run "cast receipt $tx --rpc-url \$RPC_URL | grep -E '^(status|to|from)'"
  echo
  echo "   payer's on-chain tab now   : $before -> ${B}$after${R}   ${B}(+$((after - before)))${R}"
  note "from = the PAYER, to = the escrow contract. This is the payer's ONLY"
  note "gas-paying transaction — everything after this is signing, no txs."
  note "That balance is read out of the contract's storage, not from my process."
}

step_8() {
  local prov_0 op_sbc_0 op_gas_0 tab_0 prov_1 op_sbc_1 op_gas_1 tab_1 tx
  prov_0=$(bal "$PAY_TO_ADDRESS"); op_sbc_0=$(bal "$GATEWAY_OPERATOR_ADDRESS")
  op_gas_0=$(cast balance "$GATEWAY_OPERATOR_ADDRESS" --rpc-url "$RPC_URL")
  tab_0=$(tab "$PAYER")
  echo "   payer    $PAYER"
  echo "   operator $GATEWAY_OPERATOR_ADDRESS  ${B}<- different wallet, submits settle()${R}"
  echo "   provider $PAY_TO_ADDRESS"
  echo
  : > "$STATE/b_nonces"
  local i=1
  for prompt in "In one sentence, what is mitosis?" \
                "In one sentence, what is entropy?" \
                "In one sentence, what is a blockchain?"; do
    mkescrow "$PRICE_BASE_UNITS"
    pay "$(cat "$STATE/b_header")" "$prompt" > "$STATE/b_resp"
    tx=$(txof "$STATE/b_resp")
    echo "$tx" > "$STATE/b_tx"
    echo "   ${B}[$i/3]${R} $prompt"
    echo "        nonce  $(cat "$STATE/b_nonce_short")  ${D}(random 256-bit, unordered)${R}"
    echo "        answer $(completion_of "$STATE/b_resp")"
    echo "        tx     ${C}https://testnet.radiustech.xyz/tx/$tx${R}"
    cat "$STATE/b_nonce_short" >> "$STATE/b_nonces"; echo >> "$STATE/b_nonces"
    i=$((i + 1))
  done
  prov_1=$(bal "$PAY_TO_ADDRESS"); op_sbc_1=$(bal "$GATEWAY_OPERATOR_ADDRESS")
  op_gas_1=$(cast balance "$GATEWAY_OPERATOR_ADDRESS" --rpc-url "$RPC_URL")
  tab_1=$(tab "$PAYER")
  echo
  echo "   payer's tab    $tab_0 -> $tab_1   ${B}delta $((tab_1 - tab_0))${R}"
  echo "   provider SBC   ${B}delta +$((prov_1 - prov_0))${R}   = 3 x $PRICE_BASE_UNITS"
  echo "   operator SBC   ${B}delta $((op_sbc_1 - op_sbc_0))${R}   ${D}<- note: ZERO${R}"
  echo "   operator gas   ${B}delta $((op_gas_1 - op_gas_0))${R} wei (native, not SBC)"
  note "The operator paid gas in the native currency and its SBC did not move at all."
  note "It never takes custody: settle() moves funds from the payer's tab straight"
  note "to the provider. The operator is only allowed to *trigger* that, and only"
  note "for an authorization that names it as settler."
  note "Three distinct random nonces, none collided. No facilitator anywhere."
}

step_9() {
  need_escrow_spent
  local tx; tx=$(cat "$STATE/b_tx")
  run "cast receipt $tx --rpc-url \$RPC_URL | grep -E '^(status|to|from|gasUsed|blockNumber)'"
  echo "   ${C}https://testnet.radiustech.xyz/tx/$tx${R}"
  note "to   = $ESCROW_CONTRACT_ADDRESS — my contract"
  note "from = $GATEWAY_OPERATOR_ADDRESS — my operator wallet, NOT the payer"
  note "msg.sender != payer is demonstrably true, not asserted. That is the whole"
  note "reason auth.settler has to be a signed field."
}

step_10() {
  need_escrow_spent
  local n; n=$(cat "$STATE/b_nonce")
  echo "   Same attack as step 5. Same spent payment. Same restart."
  echo
  echo -n "   restarting the gateway (wipes any in-process state) ... "
  start_gateway; echo "${G}up${R}"
  local p0 p1
  p0=$(bal "$PAY_TO_ADDRESS")
  run "curl -s -w '\nHTTP %{http_code}\n' -X POST localhost:8000/infer \
       -H 'content-type: application/json' -H \"X-PAYMENT: \$(cat $STATE/b_header)\" \
       -d '{\"prompt\":\"hi\"}'"
  p1=$(bal "$PAY_TO_ADDRESS")
  echo
  echo "   provider delta across the replay: $((p1 - p0))"
  echo
  echo "   ${B}And this 409 is not from my process — ask the chain directly:${R}"
  run "cast call \$ESCROW_CONTRACT_ADDRESS 'nonceUsed(address,uint256)(bool)' \
       $PAYER $n --rpc-url \$RPC_URL"
  note "true. The nonce is spent in contract storage."
  echo
  echo "   ${B}And the revert reason, decoded from the EVM:${R}"
  run "venv/bin/python -c \"
import json,base64
from payment import simulate_escrow_settlement
env = json.loads(base64.b64decode(open('$STATE/b_header').read()))
print(simulate_escrow_settlement(env['payload']['signature'], env['payload']['escrowAuthorization']))\""
  echo
  echo "   ${B}${G}The gateway restarted and the replay still failed. It holds no state to lose.${R}"
}

step_11() {
  note "Each of these is an attack that USED to succeed against my own code."
  note "Assertions are checked against live on-chain state, not printed output."
  run "venv/bin/python security_probes.py"
}

step_12() {
  cat <<EOF

                        ${B}Path A (Permit2 + facilitator)${R}   ${B}Path B (InferenceEscrow)${R}
   contract code I wrote        none — Permit2 + proxy      56 lines, 9 tests
   who submits the tx           Radius's facilitator        my operator wallet
   who pays gas                 facilitator (sponsored)     me
   payer's up-front cost        nothing                     one deposit tx
   txs per prompt               1                           1
   replay protection lives in   ${Y}my process's memory${R}         ${G}contract storage${R}
   survives a restart           ${Y}no  (step 5)${R}                ${G}yes (step 10)${R}
   survives horizontal scaling  ${Y}no${R}                          ${G}yes${R}
   worst case if I am wrong     facilitator rejects it      ${Y}I lose people's money${R}

   ${B}I am not claiming mine is better.${R} Path A has audited code, no capital
   lock-up, and no hot wallet for me to operate. Mine needs a deposit up front
   and makes me the party responsible for being correct.

   ${B}They fail in opposite directions: Path A's weakness is dependency.
   Mine is responsibility.${R}

   Which one wins depends on volume per payer. One prompt: Path A, obviously.
   Thousands from the same payer: the deposit amortises and the state problem
   you saw in step 5 stops being something I can operate my way out of.
EOF
}

# ── driver ───────────────────────────────────────────────────────────────────
set -a; . ./.env; set +a
# .env carries the keys; the addresses are derived (payment.py does the same).
PAYER=$(cast wallet address --private-key "$WALLET_KEY")
GATEWAY_OPERATOR_ADDRESS=$(cast wallet address --private-key "$GATEWAY_OPERATOR_KEY")

should_run() {
  if [[ -n "$ONLY" ]]; then [[ ",$ONLY," == *",$1,"* ]]; return; fi
  (( $1 >= FROM ))
}

echo "${B}Pay-Per-Prompt — final demo${R}   ${D}INFO7500, 2026-08-07${R}"
echo "  payer    $PAYER"
echo "  operator $GATEWAY_OPERATOR_ADDRESS"
echo "  provider $PAY_TO_ADDRESS"
echo "  escrow   $ESCROW_CONTRACT_ADDRESS"
echo "  chain    $CHAIN_ID   price $PRICE_BASE_UNITS base units"

# Preflight: fail loudly HERE, not in front of the room.
echo -n "  preflight: escrow tab ... "
PRE_TAB=$(tab "$PAYER")
if (( PRE_TAB < PRICE_BASE_UNITS * 8 )); then
  echo -n "${Y}low ($PRE_TAB), topping up${R} ... "
  venv/bin/python -c "from payment import ensure_escrow_deposit; ensure_escrow_deposit(min_amount=$((PRICE_BASE_UNITS * 20)))" \
    || { echo "${R}top-up FAILED"; exit 1; }
  PRE_TAB=$(tab "$PAYER")
fi
echo "${G}$PRE_TAB${R}"
echo -n "  preflight: gateway ... "; start_gateway; echo "${G}up${R}"

for i in $(seq 1 $TOTAL_STEPS); do
  should_run "$i" || continue
  eval "t=\$title_$i"
  step "$i. $t"
  "step_$i"
  [[ $i -lt $TOTAL_STEPS ]] && pause
done

echo
echo "${G}${B}done${R} — gateway stopped."
