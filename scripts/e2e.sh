#!/usr/bin/env bash
# End-to-end test of Enquoir through its HTTP API, against the real
# Airwallex sandbox. Every request is a plain curl, so any step can be
# copied out and run by hand.
#
# Prerequisites (see README.md):
#   make up              Postgres + Temporal
#   make sandbox-setup   funds the sandbox USD account
#   make worker          in one terminal
#   make api             in another
#
# Usage: scripts/e2e.sh        (or: make e2e)
#   API=http://host:port overrides the API address (default :8090).
#
# Steps 1-4 need only Enquoir itself. Steps 5 onward talk to the
# sandbox: registering the payee, paying it, and driving the transfer
# to PAID through the simulation endpoint. The bearer token is never
# printed.

set -euo pipefail

cd "$(dirname "$0")/.."
set -a
# shellcheck disable=SC1091
. ./.env
set +a

API="${API:-http://localhost:8090}"
AWX="${AWX_BASE_URL:?AWX_BASE_URL must be set in .env}"
RUN="$(date +%s)" # makes this run's references unique
WAIT_SECONDS=60

# Each request leaves its status in $STATUS and its body in $BODY.
STATUS=""
BODY=""
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT

step() { printf '\n\033[1m== %s\033[0m\n' "$*"; }
pass() { printf '   \033[32mPASS\033[0m %s\n' "$*"; }
fail() {
	printf '   \033[31mFAIL\033[0m %s\n' "$*"
	[ -n "$BODY" ] && printf '   last response: %s\n' "$BODY"
	exit 1
}
show() { printf '   $ %s\n' "$*"; }
got() { printf '   -> %s %s\n' "$STATUS" "$BODY"; }

expect_status() {
	[ "$STATUS" = "$1" ] || fail "expected HTTP $1, got $STATUS"
	pass "HTTP $1"
}

json() { jq -r "$1" <<<"$BODY"; }

# wait_for_state KEY STATE: polls GET /v1/intents/KEY until it reports STATE.
wait_for_state() {
	local key="$1" want="$2" state=""
	for _ in $(seq 1 "$WAIT_SECONDS"); do
		STATUS=$(curl -s -o "$TMP" -w '%{http_code}' "$API/v1/intents/$key")
		BODY=$(cat "$TMP")
		if [ "$STATUS" = 200 ]; then
			state=$(json .state)
			[ "$state" = "$want" ] && { pass "intent reached $want"; return; }
		fi
		sleep 1
	done
	fail "intent $key did not reach $want within ${WAIT_SECONDS}s (last state: ${state:-none})"
}

# ---------------------------------------------------------------------
step "1. Health check"
show "curl -s $API/healthz"
STATUS=$(curl -s -o "$TMP" -w '%{http_code}' "$API/healthz")
BODY=$(cat "$TMP")
got
expect_status 200

# ---------------------------------------------------------------------
step "2. Invalid input is rejected at the edge, before anything starts"

show "curl -s -X POST $API/v1/beneficiaries -H 'Content-Type: application/json' -d '{... \"routing_number\":\"021000022\" ...}'"
STATUS=$(curl -s -o "$TMP" -w '%{http_code}' -X POST "$API/v1/beneficiaries" \
	-H 'Content-Type: application/json' \
	-d '{"name":"Acme Ltd","account_number":"50001121","routing_number":"021000022","account_category":"Checking",
	     "address":{"street":"412 5th Avenue","city":"Seattle","state":"WA","postcode":"98104"}}')
BODY=$(cat "$TMP")
got
expect_status 400

show "curl -s -X POST $API/v1/intents -H 'Content-Type: application/json' -d '{... \"amount\":\"10.505\" ...}'"
STATUS=$(curl -s -o "$TMP" -w '%{http_code}' -X POST "$API/v1/intents" \
	-H 'Content-Type: application/json' \
	-d '{"payee":"any","amount":"10.505","currency":"USD","reference":"E2E-BAD","reason":"bad amount"}')
BODY=$(cat "$TMP")
got
expect_status 400

# ---------------------------------------------------------------------
step "3. Over-limit intent is BLOCKED by policy, without touching the rail"
BLOCKED_INTENT="{\"payee\":\"any\",\"amount\":\"2000000.00\",\"currency\":\"USD\",\"reference\":\"E2E-BLOCK-$RUN\",\"reason\":\"over limit\"}"
show "curl -s -X POST $API/v1/intents -H 'Content-Type: application/json' -d '$BLOCKED_INTENT'"
STATUS=$(curl -s -o "$TMP" -w '%{http_code}' -X POST "$API/v1/intents" \
	-H 'Content-Type: application/json' -d "$BLOCKED_INTENT")
BODY=$(cat "$TMP")
got
expect_status 200
BLOCKED_KEY=$(json .key)
show "curl -s $API/v1/intents/$BLOCKED_KEY   (polled)"
wait_for_state "$BLOCKED_KEY" BLOCKED

# ---------------------------------------------------------------------
step "4. Resubmitting a finished intent starts nothing new"
show "curl -s -X POST $API/v1/intents -H 'Content-Type: application/json' -d '$BLOCKED_INTENT'"
STATUS=$(curl -s -o "$TMP" -w '%{http_code}' -X POST "$API/v1/intents" \
	-H 'Content-Type: application/json' -d "$BLOCKED_INTENT")
BODY=$(cat "$TMP")
got
expect_status 200
[ "$(json .key)" = "$BLOCKED_KEY" ] || fail "resubmission returned a different key"
[ "$(json .state)" = BLOCKED ] || fail "resubmission reported $(json .state), want BLOCKED"
pass "same key, reports BLOCKED"
sleep 2
STATUS=$(curl -s -o "$TMP" -w '%{http_code}' "$API/v1/intents/$BLOCKED_KEY")
BODY=$(cat "$TMP")
[ "$(json '[.events[].type] | join(",")')" = "PROPOSED,BLOCKED" ] || fail "history changed after resubmission"
pass "history is still PROPOSED,BLOCKED"

# ---------------------------------------------------------------------
step "5. Log in to the Airwallex sandbox (for the simulation calls below)"
show "curl -s -X POST $AWX/api/v1/authentication/login -H 'x-client-id: \$AWX_CLIENT_ID' -H 'x-api-key: \$AWX_API_KEY'"
STATUS=$(curl -s -o "$TMP" -w '%{http_code}' -X POST "$AWX/api/v1/authentication/login" \
	-H "x-client-id: $AWX_CLIENT_ID" -H "x-api-key: $AWX_API_KEY")
TOKEN=$(jq -r '.token // empty' "$TMP")
BODY=$(jq -c 'if .token then .token = "<redacted>" else . end' "$TMP")
got
# The sandbox answers a successful login with 201 Created.
[[ "$STATUS" == 2?? ]] && [ -n "$TOKEN" ] || fail "sandbox login refused - check AWX_CLIENT_ID/AWX_API_KEY are sandbox keys"
pass "HTTP $STATUS, token received"

# ---------------------------------------------------------------------
step "6. Register the payee"
PAYEE='{"name":"Enquoir E2E Payee","account_number":"50001121","routing_number":"021000021",
        "account_category":"Checking",
        "address":{"street":"412 5th Avenue","city":"Seattle","state":"WA","postcode":"98104"}}'
show "curl -s -X POST $API/v1/beneficiaries -H 'Content-Type: application/json' -d '$(jq -c . <<<"$PAYEE")'"
STATUS=$(curl -s -o "$TMP" -w '%{http_code}' -X POST "$API/v1/beneficiaries" \
	-H 'Content-Type: application/json' -d "$PAYEE")
BODY=$(cat "$TMP")
got
expect_status 200
PAYEE_ID=$(json .id)

# ---------------------------------------------------------------------
step "7. Registering the same payee again returns the same beneficiary"
show "curl -s -X POST $API/v1/beneficiaries -H 'Content-Type: application/json' -d '$(jq -c . <<<"$PAYEE")'"
STATUS=$(curl -s -o "$TMP" -w '%{http_code}' -X POST "$API/v1/beneficiaries" \
	-H 'Content-Type: application/json' -d "$PAYEE")
BODY=$(cat "$TMP")
got
expect_status 200
[ "$(json .id)" = "$PAYEE_ID" ] || fail "second registration returned a different id"
pass "same id: $PAYEE_ID"

# ---------------------------------------------------------------------
step "8. Submit a payment intent, then the same intent again (an agent retrying)"
INTENT="{\"payee\":\"$PAYEE_ID\",\"amount\":\"10.50\",\"currency\":\"USD\",\"reference\":\"E2E-PAY-$RUN\",\"reason\":\"e2e test payment\"}"
for attempt in 1 2; do
	show "curl -s -X POST $API/v1/intents -H 'Content-Type: application/json' -d '$INTENT'"
	STATUS=$(curl -s -o "$TMP" -w '%{http_code}' -X POST "$API/v1/intents" \
		-H 'Content-Type: application/json' -d "$INTENT")
	BODY=$(cat "$TMP")
	got
	expect_status 200
	if [ "$attempt" = 1 ]; then KEY=$(json .key); fi
	[ "$(json .key)" = "$KEY" ] || fail "retry returned a different key"
done
pass "both submissions share key $KEY"

# ---------------------------------------------------------------------
step "9. Find the transfer on the rail by its request ID (the intent key)"
show "curl -s '$AWX/api/v1/transfers?request_id=$KEY' -H 'Authorization: Bearer \$TOKEN'   (polled)"
TRANSFER_ID=""
for _ in $(seq 1 "$WAIT_SECONDS"); do
	STATUS=$(curl -s -o "$TMP" -w '%{http_code}' "$AWX/api/v1/transfers?request_id=$KEY" \
		-H "Authorization: Bearer $TOKEN")
	BODY=$(cat "$TMP")
	if [ "$STATUS" = 200 ]; then
		COUNT=$(jq --arg k "$KEY" '[.items[] | select(.request_id == $k)] | length' "$TMP")
		[ "$COUNT" -gt 1 ] && fail "the rail holds $COUNT transfers for one request ID"
		if [ "$COUNT" = 1 ]; then
			TRANSFER_ID=$(jq -r --arg k "$KEY" '.items[] | select(.request_id == $k) | .id' "$TMP")
			break
		fi
	fi
	sleep 1
done
[ -n "$TRANSFER_ID" ] || fail "no transfer appeared for request ID $KEY within ${WAIT_SECONDS}s"
BODY=$(jq -c --arg k "$KEY" '[.items[] | select(.request_id == $k) | {id, request_id, status}]' "$TMP")
got
pass "exactly one transfer: $TRANSFER_ID"

# ---------------------------------------------------------------------
step "10. Drive the transfer to PAID through the sandbox simulator (SENT first)"
for to in SENT PAID; do
	show "curl -s -X POST $AWX/api/v1/simulation/transfers/$TRANSFER_ID/transition -H 'Authorization: Bearer \$TOKEN' -H 'Content-Type: application/json' -d '{\"status\":\"$to\"}'"
	STATUS=$(curl -s -o "$TMP" -w '%{http_code}' -X POST "$AWX/api/v1/simulation/transfers/$TRANSFER_ID/transition" \
		-H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "{\"status\":\"$to\"}")
	BODY=$(jq -c '{id, status} // .' "$TMP" 2>/dev/null || cat "$TMP")
	got
	expect_status 200
done

# ---------------------------------------------------------------------
step "11. The intent settles to APPLIED"
show "curl -s $API/v1/intents/$KEY   (polled)"
wait_for_state "$KEY" APPLIED

# ---------------------------------------------------------------------
step "12. Resubmitting the paid intent starts nothing new"
show "curl -s -X POST $API/v1/intents -H 'Content-Type: application/json' -d '$INTENT'"
STATUS=$(curl -s -o "$TMP" -w '%{http_code}' -X POST "$API/v1/intents" \
	-H 'Content-Type: application/json' -d "$INTENT")
BODY=$(cat "$TMP")
got
expect_status 200
[ "$(json .state)" = APPLIED ] || fail "resubmission reported $(json .state), want APPLIED"
pass "reports APPLIED"

# ---------------------------------------------------------------------
step "13. Full recorded history"
show "curl -s $API/v1/intents/$KEY"
sleep 2
STATUS=$(curl -s -o "$TMP" -w '%{http_code}' "$API/v1/intents/$KEY")
BODY=$(cat "$TMP")
jq -r '.events[] | "   \(.sequence_number). \(.type)\t\(.payload)"' <<<"$BODY"
[ "$(json '[.events[].type] | join(",")')" = "PROPOSED,AUTHORIZED,IN_FLIGHT,APPLIED" ] ||
	fail "unexpected history"
pass "PROPOSED -> AUTHORIZED -> IN_FLIGHT -> APPLIED, nothing repeated"

printf '\n\033[32mEnd-to-end run passed.\033[0m intent %s paid by transfer %s\n' "$KEY" "$TRANSFER_ID"
