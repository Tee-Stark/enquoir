#!/usr/bin/env bash
# End-to-end test of Enquoir Cloud through its HTTP API, against the
# real Airwallex sandbox. Every request is a plain curl, so any step can
# be copied out and run by hand.
#
# Prerequisites (see README.md):
#   make up              Postgres + Temporal
#   make sandbox-setup   funds the sandbox USD account
#   make worker          in one terminal (only one worker)
#   make api             in another
#
# Usage: scripts/e2e.sh        (or: make e2e)
#   API=http://host:port overrides the API address (default :8090).
#
# The flow: a person signs up and creates an agent; the agent, with its
# API key, registers a payee and pays it (twice, as agents retry); the
# sandbox simulator settles the transfer; the person's dashboard shows
# the history, attempts, replay and analytics; a viewer is invited, can
# look but not act, and resets their password; then a second
# organisation proves it can neither pay nor see the first one's payee
# or payment.
# Neither API keys nor the sandbox bearer token are ever printed in full.

set -euo pipefail

cd "$(dirname "$0")/.."
set -a
# shellcheck disable=SC1091
. ./.env
set +a

API="${API:-http://localhost:8090}"
AWX="${AWX_BASE_URL:?AWX_BASE_URL must be set in .env}"
RUN="$(date +%s)" # makes this run's emails and references unique
WAIT_SECONDS=60
MAIL_FILE="${MAIL_FILE:-mail.jsonl}"
# Resend's testing sender only delivers to Resend's test inboxes, so
# every address here is one; the worker keeps a copy of each email it
# sent in MAIL_FILE, which is where links are read from.
addr() { echo "delivered+e2e-$1-$RUN@resend.dev"; }

# Each request leaves its status in $STATUS and its body in $BODY.
STATUS=""
BODY=""
TMP="$(mktemp)"
JAR_A="$(mktemp)"
JAR_B="$(mktemp)"
trap 'rm -f "$TMP" "$JAR_A" "$JAR_B"' EXIT

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

# req METHOD PATH [BODY] [curl args...]: sends one JSON request to the API.
req() {
	local method="$1" path="$2" data="${3:-}"
	shift 3 || shift $#
	local args=(-s -o "$TMP" -w '%{http_code}' -X "$method" "$API$path" "$@")
	[ -n "$data" ] && args+=(-H 'Content-Type: application/json' -d "$data")
	STATUS=$(curl "${args[@]}")
	BODY=$(cat "$TMP")
}

# as_agent KEY METHOD PATH [BODY]: a request authenticated with an agent key.
as_agent() {
	local key="$1"
	shift
	req "$1" "$2" "${3:-}" -H "Authorization: Bearer $key"
}

# wait_for_state KEY STATE: polls GET /v1/intents/KEY as agent A until it reports STATE.
wait_for_state() {
	local key="$1" want="$2" state=""
	for _ in $(seq 1 "$WAIT_SECONDS"); do
		as_agent "$KEY_A" GET "/v1/intents/$key"
		if [ "$STATUS" = 200 ]; then
			state=$(json .state)
			[ "$state" = "$want" ] && { pass "intent reached $want"; return; }
		fi
		sleep 1
	done
	fail "intent $key did not reach $want within ${WAIT_SECONDS}s (last state: ${state:-none})"
}

# token_for EMAIL PATH: waits for the link to PATH last emailed to EMAIL; prints its token.
token_for() {
	local t=""
	for _ in $(seq 1 "$WAIT_SECONDS"); do
		t=$(grep -F "\"to\":\"$1\"" "$MAIL_FILE" 2>/dev/null | tail -1 | jq -r .text | grep -o "$2?token=[^ ]*" | sed 's/.*token=//' || true)
		[ -n "$t" ] && { echo "$t"; return; }
		sleep 1
	done
	fail "no email with a $2 link reached $1 within ${WAIT_SECONDS}s (is the worker running with MAIL_FILE?)"
}

# verify EMAIL: follows the verification link emailed to EMAIL.
verify() {
	req POST /v1/auth/verify-email "{\"token\":\"$(token_for "$1" /verify-email)\"}"
	got
	expect_status 204
}

# sign_up JAR EMAIL ORG: signs up, keeping the session cookie in JAR.
sign_up() {
	req POST /v1/auth/signup "{\"email\":\"$2\",\"password\":\"correct horse battery\",\"organisation_name\":\"$3\"}" -c "$1"
}

# create_agent JAR NAME: creates an agent with the session in JAR; leaves its key in $NEW_KEY.
create_agent() {
	req POST /v1/agents "{\"name\":\"$2\"}" -b "$1"
	NEW_KEY=$(json .api_key.secret)
	BODY=$(jq -c '.api_key.secret = (.api_key.secret[0:12] + "…<shown once, redacted here>")' "$TMP")
}

PAYEE='{"name":"Enquoir E2E Payee","account_number":"50001121","routing_number":"021000021",
        "account_category":"Checking",
        "address":{"street":"412 5th Avenue","city":"Seattle","state":"WA","postcode":"98104"}}'

# ---------------------------------------------------------------------
step "1. Health check"
show "curl -s $API/healthz"
req GET /healthz ""
got
expect_status 200

# The sign-up limit is 5 an hour per IP; repeated runs would hit it.
# This is a test harness against a development Redis: clear that one
# counter so the run can sign up.
docker compose -f deploy/docker-compose.yml exec -T redis sh -c "redis-cli --scan --pattern 'enq:rl:signup-ip:*' | xargs -r redis-cli del" >/dev/null 2>&1 || true

# ---------------------------------------------------------------------
step "2. Sign up an organisation (sets the dashboard session cookie)"
EMAIL_A="$(addr a)"
show "curl -s -c cookies -X POST $API/v1/auth/signup -d '{\"email\":\"$EMAIL_A\",\"password\":\"…\",\"organisation_name\":\"E2E Org A\"}'"
sign_up "$JAR_A" "$EMAIL_A" "E2E Org A"
got
expect_status 201
grep -q enquoir_session "$JAR_A" || fail "no session cookie set"
pass "session cookie set"

show "curl -s -b cookies -X POST $API/v1/agents -d '{\"name\":\"Invoice bot\"}'   (before verifying)"
req POST /v1/agents '{"name":"too soon"}' -b "$JAR_A"
got
expect_status 403
[ "$(json .code)" = email_unverified ] || fail "code = $(json .code), want email_unverified"
pass "an unverified account cannot issue agent keys"

show "curl -s -X POST $API/v1/auth/verify-email -d '{\"token\":\"<from the emailed link>\"}'"
verify "$EMAIL_A"
pass "email verified from the link sent through Resend"

# ---------------------------------------------------------------------
step "3. Create an agent; its API key is shown once"
show "curl -s -b cookies -X POST $API/v1/agents -d '{\"name\":\"Invoice bot\"}'"
create_agent "$JAR_A" "Invoice bot"
got
expect_status 201
KEY_A="$NEW_KEY"
[[ "$KEY_A" == enq_sk_* ]] || fail "no API key returned"
pass "agent key issued (${KEY_A:0:12}…)"

# ---------------------------------------------------------------------
step "4. The payment routes refuse a request without a valid agent key"
show "curl -s -X POST $API/v1/intents -d '{...}'   (no Authorization header)"
req POST /v1/intents '{"payee":"x","amount":"1.00","currency":"USD","reference":"NOAUTH"}'
got
expect_status 401
show "curl -s -X POST $API/v1/intents -H 'Authorization: Bearer enq_sk_made_up' -d '{...}'"
as_agent enq_sk_made_up POST /v1/intents '{"payee":"x","amount":"1.00","currency":"USD","reference":"NOAUTH"}'
got
expect_status 401

# ---------------------------------------------------------------------
step "5. Invalid input is rejected at the edge, before anything starts"
show "curl -s -X POST $API/v1/beneficiaries -H 'Authorization: Bearer \$KEY_A' -d '{... \"routing_number\":\"021000022\" ...}'"
as_agent "$KEY_A" POST /v1/beneficiaries '{"name":"Acme Ltd","account_number":"50001121","routing_number":"021000022","account_category":"Checking",
     "address":{"street":"412 5th Avenue","city":"Seattle","state":"WA","postcode":"98104"}}'
got
expect_status 400
show "curl -s -X POST $API/v1/intents -H 'Authorization: Bearer \$KEY_A' -d '{... \"amount\":\"10.505\" ...}'"
as_agent "$KEY_A" POST /v1/intents '{"payee":"any","amount":"10.505","currency":"USD","reference":"E2E-BAD"}'
got
expect_status 400

# ---------------------------------------------------------------------
step "6. Log in to the Airwallex sandbox (for the simulation calls below)"
show "curl -s -X POST $AWX/api/v1/authentication/login -H 'x-client-id: \$AWX_CLIENT_ID' -H 'x-api-key: \$AWX_API_KEY'"
STATUS=$(curl -s -o "$TMP" -w '%{http_code}' -X POST "$AWX/api/v1/authentication/login" \
	-H "x-client-id: $AWX_CLIENT_ID" -H "x-api-key: $AWX_API_KEY")
TOKEN=$(jq -r '.token // empty' "$TMP")
BODY=$(jq -c 'if .token then .token = "<redacted>" else . end' "$TMP")
got
# The sandbox answers a successful login with 201 Created.
[[ "$STATUS" == 2?? ]] && [ -n "$TOKEN" ] || fail "sandbox login refused - check the keys, and that this IP is allowlisted for them"
pass "HTTP $STATUS, token received"

# ---------------------------------------------------------------------
step "7. The agent registers the payee"
show "curl -s -X POST $API/v1/beneficiaries -H 'Authorization: Bearer \$KEY_A' -d '$(jq -c . <<<"$PAYEE")'"
as_agent "$KEY_A" POST /v1/beneficiaries "$PAYEE"
got
expect_status 200
PAYEE_ID=$(json .id)

# ---------------------------------------------------------------------
step "8. Registering the same payee again returns the same beneficiary"
as_agent "$KEY_A" POST /v1/beneficiaries "$PAYEE"
got
expect_status 200
[ "$(json .id)" = "$PAYEE_ID" ] || fail "second registration returned a different id"
pass "same id: $PAYEE_ID"

# ---------------------------------------------------------------------
step "9. An over-limit intent is BLOCKED by policy, without touching the rail"
BLOCKED_INTENT="{\"payee\":\"$PAYEE_ID\",\"amount\":\"2000000.00\",\"currency\":\"USD\",\"reference\":\"E2E-BLOCK-$RUN\",\"reason\":\"over limit\"}"
show "curl -s -X POST $API/v1/intents -H 'Authorization: Bearer \$KEY_A' -d '$BLOCKED_INTENT'"
as_agent "$KEY_A" POST /v1/intents "$BLOCKED_INTENT"
got
expect_status 200
BLOCKED_KEY=$(json .key)
show "curl -s $API/v1/intents/$BLOCKED_KEY -H 'Authorization: Bearer \$KEY_A'   (polled)"
wait_for_state "$BLOCKED_KEY" BLOCKED

# ---------------------------------------------------------------------
step "10. Resubmitting a finished intent starts nothing new"
as_agent "$KEY_A" POST /v1/intents "$BLOCKED_INTENT"
got
expect_status 200
[ "$(json .key)" = "$BLOCKED_KEY" ] || fail "resubmission returned a different key"
[ "$(json .state)" = BLOCKED ] || fail "resubmission reported $(json .state), want BLOCKED"
pass "same key, reports BLOCKED"
sleep 2
as_agent "$KEY_A" GET "/v1/intents/$BLOCKED_KEY"
[ "$(json '[.events[].type] | join(",")')" = "PROPOSED,BLOCKED" ] || fail "history changed after resubmission"
pass "history is still PROPOSED,BLOCKED"

# ---------------------------------------------------------------------
step "11. Pay the payee, then send the same intent again, reason reworded (an agent retrying)"
INTENT="{\"payee\":\"$PAYEE_ID\",\"amount\":\"10.50\",\"currency\":\"USD\",\"reference\":\"E2E-PAY-$RUN\",\"reason\":\"e2e test payment\"}"
RETRY="{\"payee\":\"$PAYEE_ID\",\"amount\":\"10.50\",\"currency\":\"USD\",\"reference\":\"E2E-PAY-$RUN\",\"reason\":\"retrying E2E-PAY-$RUN after a timeout\"}"
for body in "$INTENT" "$RETRY"; do
	show "curl -s -X POST $API/v1/intents -H 'Authorization: Bearer \$KEY_A' -d '$body'"
	as_agent "$KEY_A" POST /v1/intents "$body"
	got
	expect_status 200
	if [ "$body" = "$INTENT" ]; then KEY=$(json .key); fi
	[ "$(json .key)" = "$KEY" ] || fail "retry returned a different key"
done
pass "both submissions share key $KEY"

# ---------------------------------------------------------------------
step "12. Find the transfer on the rail by its request ID (the intent key)"
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
step "13. Drive the transfer to PAID through the sandbox simulator (SENT first)"
# The simulator can answer 500 yet apply the transition moments later,
# so each transition is confirmed by reading the transfer back.
for to in SENT PAID; do
	show "curl -s -X POST $AWX/api/v1/simulation/transfers/$TRANSFER_ID/transition -H 'Authorization: Bearer \$TOKEN' -d '{\"next_status\":\"$to\"}'"
	reached=""
	for attempt in 1 2 3 4 5 6; do
		STATUS=$(curl -s -o "$TMP" -w '%{http_code}' "$AWX/api/v1/transfers/$TRANSFER_ID" -H "Authorization: Bearer $TOKEN")
		now=$(jq -r .status "$TMP")
		if [ "$now" = "$to" ] || { [ "$to" = SENT ] && [ "$now" = PAID ]; }; then reached=1; break; fi
		if [ "$attempt" = 1 ] || [ $((attempt % 2)) = 1 ]; then
			STATUS=$(curl -s -o "$TMP" -w '%{http_code}' -X POST "$AWX/api/v1/simulation/transfers/$TRANSFER_ID/transition" \
				-H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d "{\"next_status\":\"$to\"}")
			if [ "$STATUS" = 200 ]; then BODY=$(jq -c '{id, status}' "$TMP"); else BODY=$(cat "$TMP"); fi
			got
		fi
		sleep 3
	done
	[ -n "$reached" ] || fail "transfer never reached $to"
	pass "transfer is $to"
done

# ---------------------------------------------------------------------
step "14. The intent settles to APPLIED"
show "curl -s $API/v1/intents/$KEY -H 'Authorization: Bearer \$KEY_A'   (polled)"
wait_for_state "$KEY" APPLIED

# ---------------------------------------------------------------------
step "15. Resubmitting the paid intent starts nothing new"
as_agent "$KEY_A" POST /v1/intents "$INTENT"
got
expect_status 200
[ "$(json .state)" = APPLIED ] || fail "resubmission reported $(json .state), want APPLIED"
pass "reports APPLIED"

# ---------------------------------------------------------------------
step "16. Full recorded history"
show "curl -s $API/v1/intents/$KEY -H 'Authorization: Bearer \$KEY_A'"
sleep 2
as_agent "$KEY_A" GET "/v1/intents/$KEY"
jq -r '.events[] | "   \(.sequence_number). \(.type)\t\(.payload)"' <<<"$BODY"
[ "$(json '[.events[].type] | join(",")')" = "PROPOSED,AUTHORIZED,IN_FLIGHT,APPLIED" ] ||
	fail "unexpected history"
pass "PROPOSED -> AUTHORIZED -> IN_FLIGHT -> APPLIED, nothing repeated"

# ---------------------------------------------------------------------
step "17. The dashboard: the payment in history, with its agent and payee"
as_user() { req "$1" "$2" "${3:-}" -b "$JAR_A"; }
show "curl -s -b cookies '$API/v1/payments?reference=E2E-PAY-$RUN'"
as_user GET "/v1/payments?reference=E2E-PAY-$RUN"
expect_status 200
BODY=$(jq -c '.data[0] | {state, amount: .amount.amount, payee: .payee.name, agent: .agent.name, submissions, rail_calls, transfer_id}' "$TMP")
got
[ "$(json .state)" = APPLIED ] && [ "$(json .agent)" = "Invoice bot" ] && [ "$(json .payee)" = "Enquoir E2E Payee" ] ||
	fail "history does not show the applied payment with its agent and payee"
[ "$(json .submissions)" = 3 ] || fail "submissions = $(json .submissions), want 3 (sent twice, then resubmitted)"
[ "$(json .transfer_id)" = "$TRANSFER_ID" ] || fail "history shows a different transfer"
pass "APPLIED, by Invoice bot, to Enquoir E2E Payee, 3 submissions, transfer $TRANSFER_ID"

# ---------------------------------------------------------------------
step "18. The payment in full: verified chain and every attempt"
show "curl -s -b cookies $API/v1/payments/$KEY"
as_user GET "/v1/payments/$KEY"
expect_status 200
BODY=$(jq -c '{chain, events: [.events[].type], attempts: [.attempts[] | "\(.kind):\(.operation):\(.outcome)"]}' "$TMP")
got
[ "$(json .chain.valid)" = true ] || fail "the hash chain does not verify"
jq -e '.attempts | map(select(.operation == "create_transfer" and .outcome == "ok")) | length == 1' "$TMP" >/dev/null ||
	fail "want exactly one successful create_transfer call recorded"
pass "chain verifies; one create_transfer call, recorded with its outcome"

# ---------------------------------------------------------------------
step "19. Attempts, filtered: this payment's agent submissions"
show "curl -s -b cookies '$API/v1/attempts?intent_key=$KEY&kind=agent_submission'"
as_user GET "/v1/attempts?intent_key=$KEY&kind=agent_submission"
expect_status 200
BODY=$(jq -c '[.data | reverse | .[].outcome]' "$TMP")
got
[ "$(json '.[0]')" = started ] && [ "$(json '[.[1:][] | select(. == "attached" or . == "deduplicated")] | length')" = 2 ] ||
	fail "want started, then two retries recognised as attached or deduplicated"
pass "the first submission started the payment; both retries were recognised, not paid again"

# ---------------------------------------------------------------------
step "20. Replay: every step, ordered and timed"
show "curl -s -b cookies $API/v1/payments/$KEY/replay"
as_user GET "/v1/payments/$KEY/replay"
expect_status 200
jq -r '.steps[] | "   +\(.offset_ms)ms\t\(.actor)\t\(.title)"' "$TMP"
BODY=$(jq -c '{states, steps: (.steps | length), duration_ms}' "$TMP")
[ "$(json '.states | join(",")')" = "PROPOSED,AUTHORIZED,IN_FLIGHT,APPLIED" ] || fail "replay states are wrong"
jq -e '.steps[0].type == "submission" and ([.steps[] | select(.to_state == "APPLIED")] | length == 1)' "$TMP" >/dev/null ||
	fail "replay should open with the submission and reach APPLIED once"
pass "replay opens with the agent's submission and ends APPLIED"

# ---------------------------------------------------------------------
step "21. Analytics, in Lagos time"
show "curl -s -b cookies '$API/v1/analytics/summary?tz=Africa/Lagos'"
as_user GET "/v1/analytics/summary?tz=Africa/Lagos"
expect_status 200
BODY=$(jq -c '{tz: .period.tz, payments: .payments.total, applied: .payments.by_state.APPLIED, blocked: .payments.blocked_by_policy, success_rate: .payments.success_rate, duplicates_prevented: .submissions.duplicates_prevented, invalid: .submissions.by_outcome.invalid, volume: .volume}' "$TMP")
got
[ "$(json .payments)" = 2 ] && [ "$(json .applied)" = 1 ] && [ "$(json .blocked)" = 1 ] && [ "$(json .success_rate)" = 1 ] ||
	fail "summary counts are wrong"
[ "$(json .duplicates_prevented)" = 3 ] && [ "$(json .invalid)" = 1 ] || fail "submission counts are wrong"
pass "2 payments (1 applied, 1 blocked), 3 duplicates prevented, 1 invalid submission"
show "curl -s -b cookies '$API/v1/analytics/timeseries?metric=payments&interval=hour&group_by=state&tz=Africa/Lagos'"
as_user GET "/v1/analytics/timeseries?metric=payments&interval=hour&group_by=state&tz=Africa/Lagos"
expect_status 200
BODY=$(jq -c '{unit, buckets: (.buckets | length), last_bucket: .buckets[-1], series: [.series[] | {group, total: ([.points[].value] | add)}]}' "$TMP")
got
[[ "$(json .last_bucket)" == *"+01:00" ]] || fail "buckets are not in Lagos time"
pass "hourly buckets in Lagos time (+01:00), one series per state"

# ---------------------------------------------------------------------
step "22. The team: invite a viewer; they can look but not issue keys"
EMAIL_V="$(addr viewer)"
JAR_V="$(mktemp)"
show "curl -s -b cookies -X POST $API/v1/invitations -d '{\"email\":\"$EMAIL_V\",\"role\":\"viewer\"}'"
req POST /v1/invitations "{\"email\":\"$EMAIL_V\",\"role\":\"viewer\"}" -b "$JAR_A"
got
expect_status 201
show "curl -s -c cookies-v -X POST $API/v1/invitations/accept -d '{\"token\":\"<from the emailed link>\",\"password\":\"...\"}'"
req POST /v1/invitations/accept "{\"token\":\"$(token_for "$EMAIL_V" /invitations/accept)\",\"password\":\"viewer passphrase here\"}" -c "$JAR_V"
got
expect_status 200
[ "$(json .account_created)" = true ] && [ "$(json .role)" = viewer ] || fail "want a new viewer account"
pass "invitation accepted: a new, verified viewer account"
req GET "/v1/payments?reference=E2E-PAY-$RUN" "" -b "$JAR_V"
expect_status 200
[ "$(json '.data | length')" = 1 ] || fail "the viewer cannot see the organisation's payment"
pass "the viewer sees the organisation's payments"
req POST /v1/agents '{"name":"viewer bot"}' -b "$JAR_V"
got
expect_status 403
[ "$(json .code)" = forbidden ] || fail "code = $(json .code), want forbidden"
pass "a viewer cannot create agents"

# ---------------------------------------------------------------------
step "23. The audit log records who did what"
show "curl -s -b cookies $API/v1/audit-log"
req GET /v1/audit-log "" -b "$JAR_A"
expect_status 200
BODY=$(jq -c '[.data[] | "\(.action) by \(.actor.type // "-") \(.actor.email // .actor.name // "-")"] | reverse' "$TMP")
got
for a in agent.created payee.registered member.invited invitation.accepted; do
	jq -e --arg a "$a" '[.data[].action] | index($a)' "$TMP" >/dev/null || fail "no $a in the audit log"
done
[ "$(jq -r '[.data[] | select(.action == "payee.registered")][0].actor | "\(.type) \(.name)"' "$TMP")" = "agent Invoice bot" ] ||
	fail "the payee registration does not name the agent that did it"
pass "agent, payee (by the Invoice bot agent), invitation and acceptance all recorded"
req GET /v1/audit-log "" -b "$JAR_V"
expect_status 403
pass "a viewer cannot read the audit log"

# ---------------------------------------------------------------------
step "24. Forgot password: a reset link signs the viewer out everywhere"
req POST /v1/auth/password/forgot "{\"email\":\"$EMAIL_V\"}"
expect_status 202
req POST /v1/auth/password/reset "{\"token\":\"$(token_for "$EMAIL_V" /reset-password)\",\"password\":\"a brand new passphrase\"}"
got
expect_status 204
req GET /v1/auth/session "" -b "$JAR_V"
expect_status 401
req POST /v1/auth/login "{\"email\":\"$EMAIL_V\",\"password\":\"a brand new passphrase\"}"
expect_status 200
pass "reset via the emailed link; the old session is gone; the new password works"
rm -f "$JAR_V"

# ---------------------------------------------------------------------
step "25. A second organisation is isolated from the first"
EMAIL_B="$(addr b)"
sign_up "$JAR_B" "$EMAIL_B" "E2E Org B"
expect_status 201
verify "$EMAIL_B"
create_agent "$JAR_B" "Other bot"
expect_status 201
KEY_B="$NEW_KEY"
pass "org B signed up with its own agent"

show "curl -s -X POST $API/v1/intents -H 'Authorization: Bearer \$KEY_B' -d '{\"payee\":\"$PAYEE_ID\", ...}'   (org A's payee)"
as_agent "$KEY_B" POST /v1/intents "{\"payee\":\"$PAYEE_ID\",\"amount\":\"10.50\",\"currency\":\"USD\",\"reference\":\"E2E-STEAL-$RUN\"}"
got
expect_status 422
[ "$(json .code)" = payee_unknown ] || fail "code = $(json .code), want payee_unknown"
pass "org B cannot pay org A's payee"

show "curl -s $API/v1/intents/$KEY -H 'Authorization: Bearer \$KEY_B'   (org A's intent)"
as_agent "$KEY_B" GET "/v1/intents/$KEY"
got
expect_status 404
pass "org B cannot see org A's payment"

show "curl -s -b cookies-b $API/v1/payments/$KEY   (org A's payment, from org B's dashboard)"
req GET "/v1/payments/$KEY" "" -b "$JAR_B"
got
expect_status 404
pass "org B's dashboard cannot see org A's payment either"

show "curl -s -X POST $API/v1/beneficiaries -H 'Authorization: Bearer \$KEY_B' -d '<the same payee details>'"
as_agent "$KEY_B" POST /v1/beneficiaries "$PAYEE"
got
expect_status 200
[ "$(json .id)" != "$PAYEE_ID" ] || fail "org B was handed org A's beneficiary"
pass "the same details are a separate beneficiary for org B: $(json .id)"

printf '\n\033[32mEnd-to-end run passed.\033[0m intent %s paid by transfer %s\n' "$KEY" "$TRANSFER_ID"
