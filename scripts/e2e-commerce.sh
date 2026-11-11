#!/usr/bin/env bash
# End-to-end test of merchant-delegated commerce, phase 1 (merchant
# integration and identity), through the HTTP API. Needs no Airwallex
# sandbox. Every request is a plain curl, so any step can be copied out
# and run by hand.
#
# Prerequisites (see README.md):
#   make up       Postgres, Temporal, Redis
#   make worker   sends the verification email (MAIL_FILE keeps a copy)
#   make api      the API
#
# Usage: scripts/e2e-commerce.sh   (or: make e2e-commerce)
#   API=http://host:port overrides the API address (default :8090).
#
# The flow: an owner signs up, verifies, and connects a merchant; the
# merchant's backend, with its integration key, registers customers and
# agents (repeats resolve, changes conflict) and binds a saved card by
# its own reference (card data refused); a customer approves a mandate
# from the link Enquoir emailed them, rejects another, and the merchant
# revokes the first; the key is shown to be
# the only credential commerce accepts, and accepted nowhere else; a
# second storefront and a second organisation can't see the first's
# records; disabling, rotating and revoking end the key's access at
# once, cache included; the audit log names the merchant that acted.
# Keys are never printed in full.

set -euo pipefail

cd "$(dirname "$0")/.."
set -a
# shellcheck disable=SC1091
. ./.env
set +a

API="${API:-http://localhost:8090}"
RUN="$(date +%s)"
WAIT_SECONDS=60
MAIL_FILE="${MAIL_FILE:-mail.jsonl}"
# Resend test inboxes only: a run never emails a real person.
addr() { echo "delivered+e2e-commerce-$1-$RUN@resend.dev"; }

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
check() { if eval "$1"; then pass "$2"; else fail "$2"; fi; }
json() { jq -r "$1" <<<"$BODY"; }
redact() { echo "${1:0:12}…"; }

req() {
	local method="$1" path="$2" data="${3:-}"
	shift 3 || shift $#
	local args=(-s -o "$TMP" -w '%{http_code}' -X "$method" "$API$path" "$@")
	[ -n "$data" ] && args+=(-H 'Content-Type: application/json' -d "$data")
	STATUS=$(curl "${args[@]}")
	BODY=$(cat "$TMP")
}
# with_key KEY METHOD PATH [BODY]: a request with a bearer key.
with_key() {
	local key="$1"
	shift
	req "$1" "$2" "${3:-}" -H "Authorization: Bearer $key"
}

token_for() {
	local t=""
	for _ in $(seq 1 "$WAIT_SECONDS"); do
		t=$(grep -F "\"to\":\"$1\"" "$MAIL_FILE" 2>/dev/null | jq -r .text | grep -o "$2?token=[^ ]*" | tail -1 | sed 's/.*token=//' || true)
		[ -n "$t" ] && { echo "$t"; return; }
		sleep 1
	done
	fail "no email with a $2 link reached $1 within ${WAIT_SECONDS}s (is the worker running with MAIL_FILE?)"
}

# owner JAR LABEL: signs up and verifies an owner, session in JAR.
owner() {
	local email
	email=$(addr "$2")
	req POST /v1/auth/signup "{\"email\":\"$email\",\"password\":\"correct horse battery\",\"organisation_name\":\"E2E Commerce $2\"}" -c "$1"
	expect_status 201
	req POST /v1/auth/verify-email "{\"token\":\"$(token_for "$email" /verify-email)\"}"
	expect_status 204
}

# integration JAR NAME: connects a merchant; leaves its ID in $NEW_ID and key in $NEW_KEY.
integration() {
	req POST /v1/merchant-integrations "{\"name\":\"$2\"}" -b "$1"
	NEW_ID=$(json .integration.id)
	NEW_KEY=$(json .key.secret)
	BODY=$(jq -c '.key.secret = (.key.secret[0:12] + "…<shown once, redacted here>")' "$TMP")
}

# The sign-up limit is 5 an hour per IP; clear it so repeated runs work.
docker compose -f deploy/docker-compose.yml exec -T redis sh -c "redis-cli --scan --pattern 'enq:rl:signup-ip:*' | xargs -r redis-cli del" >/dev/null 2>&1 || true

# ---------------------------------------------------------------------
step "1. Health check"
req GET /healthz ""
expect_status 200

step "2. An owner signs up and verifies their email"
owner "$JAR_A" a

step "3. The owner connects a merchant; the key is shown once"
show "curl -s -b cookies -X POST $API/v1/merchant-integrations -d '{\"name\":\"Acme Electronics\"}'"
integration "$JAR_A" "Acme Electronics"
got
expect_status 201
INTEGRATION_A=$NEW_ID KEY_A=$NEW_KEY
check '[[ "$KEY_A" == enq_mk_* ]]' "the key is an enq_mk_ integration key"
req GET "/v1/merchant-integrations/$INTEGRATION_A" "" -b "$JAR_A"
got
expect_status 200
check '! grep -qF "$KEY_A" <<<"$BODY" && [ "$(json .key.prefix)" = "${KEY_A:0:12}" ]' "afterwards only its prefix is shown"

step "4. The merchant registers a customer; repeating it resolves the same one"
show "curl -s -H 'Authorization: Bearer $(redact "$KEY_A")' -X POST $API/v1/commerce/customers -d '{\"external_id\":\"cust_42\",\"email\":\"Ada@Example.com\"}'"
with_key "$KEY_A" POST /v1/commerce/customers '{"external_id":"cust_42","email":"Ada@Example.com"}'
got
expect_status 201
CUSTOMER=$(json .id)
check '[ "$(json .email)" = ada@example.com ]' "the email is normalised"
with_key "$KEY_A" POST /v1/commerce/customers '{"external_id":"cust_42","email":"ada@example.com"}'
got
expect_status 200
check '[ "$(json .id)" = "$CUSTOMER" ]' "the same customer"
with_key "$KEY_A" POST /v1/commerce/customers '{"external_id":"cust_42","email":"eve@example.com"}'
got
expect_status 409
check '[ "$(json .code)" = registration_conflict ]' "different details are a conflict, not an update"

step "5. The merchant registers its shopping agent"
with_key "$KEY_A" POST /v1/commerce/agents '{"external_id":"shopping-assistant","name":"Acme Shopping Assistant"}'
got
expect_status 201
AGENT=$(json .id)
with_key "$KEY_A" POST /v1/commerce/agents '{"external_id":"shopping-assistant","name":"Acme Shopping Assistant"}'
expect_status 200
with_key "$KEY_A" GET "/v1/commerce/customers?external_id=cust_42" ""
got
check '[ "$(json ".data | length")" = 1 ] && [ "$(json ".data[0].id")" = "$CUSTOMER" ]' "found by its external ID"

step "5b. The merchant binds the customer's saved card - by its own reference, never the card"
show "curl -s -H 'Authorization: Bearer $(redact "$KEY_A")' -X POST $API/v1/commerce/funding-methods -d '{\"customer_id\":\"$CUSTOMER\",\"provider\":\"stripe\",\"merchant_funding_method_id\":\"mfm_123\",\"display\":{\"brand\":\"visa\",\"last_four\":\"4242\"}}'"
FM_BODY="{\"customer_id\":\"$CUSTOMER\",\"provider\":\"stripe\",\"merchant_funding_method_id\":\"mfm_123\",\"display\":{\"brand\":\"visa\",\"last_four\":\"4242\"}}"
with_key "$KEY_A" POST /v1/commerce/funding-methods "$FM_BODY"
got
expect_status 201
BINDING=$(json .id)
check '[[ "$BINDING" == fmb_* ]]' "Enquoir returns its own binding ID"
with_key "$KEY_A" POST /v1/commerce/funding-methods "$FM_BODY"
expect_status 200
check '[ "$(json .id)" = "$BINDING" ]' "the same binding on a repeat"
with_key "$KEY_A" POST /v1/commerce/funding-methods "{\"customer_id\":\"$CUSTOMER\",\"provider\":\"stripe\",\"merchant_funding_method_id\":\"mfm_124\",\"display\":{\"brand\":\"visa\",\"last_four\":\"4242\"},\"card_number\":\"4111111111111111\"}"
got
expect_status 400
check '[ "$(json .field)" = card_number ] && ! grep -q 4111 <<<"$BODY"' "a card_number field is refused, by name, and not repeated"
with_key "$KEY_A" POST /v1/commerce/funding-methods "{\"customer_id\":\"$CUSTOMER\",\"provider\":\"stripe\",\"merchant_funding_method_id\":\"4111-1111-1111-1111\",\"display\":{\"brand\":\"visa\"}}"
expect_status 400
check '[ "$(json .field)" = merchant_funding_method_id ]' "a card number as the reference is refused"
with_key "$KEY_A" GET "/v1/commerce/customers/$CUSTOMER/funding-methods" ""
got
expect_status 200
check '[ "$(json ".data | length")" = 1 ] && [ "$(json ".data[0].id")" = "$BINDING" ]' "the customer has exactly the one binding"

step "5c. A mandate: the merchant asks, the customer decides from the emailed link"
SHOPPER=$(addr shopper)
with_key "$KEY_A" POST /v1/commerce/customers "{\"external_id\":\"cust_shopper\",\"email\":\"$SHOPPER\"}"
expect_status 201
SHOPPER_ID=$(json .id)
with_key "$KEY_A" POST /v1/commerce/funding-methods "{\"customer_id\":\"$SHOPPER_ID\",\"provider\":\"stripe\",\"provider_method_reference\":\"pm_1ShopperCard\",\"display\":{\"brand\":\"visa\",\"last_four\":\"4242\"}}"
expect_status 201
SHOPPER_CARD=$(json .id)
check '[ "$(json .provider_method_reference)" = pm_1ShopperCard ] && [ "$(json .merchant_funding_method_id)" = null ]' "bound by the PSP's own ID, with no reference of the merchant's"
EXPIRES=$(jq -rn 'now + 30*86400 | strftime("%Y-%m-%dT%H:%M:%SZ")')
mandate_request() {
	echo "{\"external_id\":\"$1\",\"customer_id\":\"$SHOPPER_ID\",\"agent_id\":\"$AGENT\",\"funding_method_id\":\"$SHOPPER_CARD\",\"approval_mode\":\"autonomous\",\"scope\":{\"currency\":\"USD\",\"max_transaction_minor\":150000,\"max_cumulative_minor\":300000,\"max_transactions\":3,\"allowed_categories\":[\"electronics\"]},\"expires_at\":\"$EXPIRES\"}"
}
show "curl -s -H 'Authorization: Bearer $(redact "$KEY_A")' -X POST $API/v1/commerce/mandate-requests -d '$(mandate_request mreq_1)'"
with_key "$KEY_A" POST /v1/commerce/mandate-requests "$(mandate_request mreq_1)"
got
expect_status 201
REQUEST=$(json .id)
HASH=$(json .terms.terms_hash)
check '[ "$(json .status)" = pending ] && ! grep -q "token" <<<"$BODY"' "pending, and the merchant gets no link"
with_key "$KEY_A" POST /v1/commerce/mandate-requests "$(mandate_request mreq_1)"
expect_status 200
TOKEN=$(token_for "$SHOPPER" /consent) || fail "no consent email reached the customer within ${WAIT_SECONDS}s (is a current worker the only one running?)"
pass "the consent link reached the customer's inbox (through Resend)"
req POST /v1/consent/view "{\"token\":\"$TOKEN\"}"
got
expect_status 200
check '[ "$(json .terms.terms_hash)" = "$HASH" ] && [ "$(json .terms.merchant.name)" = "Acme Electronics" ]' "the page shows the frozen terms"
req POST /v1/consent/approve "{\"token\":\"$TOKEN\",\"terms_hash\":\"sha256:not-these\"}"
expect_status 409
req POST /v1/consent/approve "{\"token\":\"$TOKEN\",\"terms_hash\":\"$HASH\"}" -H "User-Agent: e2e-consent-page"
got
expect_status 200
MANDATE=$(json .mandate.id)
check '[ "$(json .mandate.status)" = active ]' "approved: the mandate is active"
req POST /v1/consent/approve "{\"token\":\"$TOKEN\",\"terms_hash\":\"$HASH\"}"
expect_status 400
with_key "$KEY_A" GET "/v1/commerce/mandate-requests/$REQUEST" ""
check '[ "$(json .status)" = approved ] && [ "$(json .mandate_id)" = "$MANDATE" ]' "the merchant sees the customer's approval"
with_key "$KEY_A" POST /v1/commerce/funding-methods/$SHOPPER_CARD/disable ""
with_key "$KEY_A" GET "/v1/commerce/mandates/$MANDATE" ""
check '[ "$(json .status)" = suspended ]' "suspended while the card is disabled"
with_key "$KEY_A" POST /v1/commerce/funding-methods/$SHOPPER_CARD/enable ""
with_key "$KEY_A" GET "/v1/commerce/mandates?customer_id=$SHOPPER_ID&status=active" ""
check '[ "$(json ".data | length")" = 1 ]' "listed as active again"

with_key "$KEY_A" POST /v1/commerce/mandate-requests "$(mandate_request mreq_2)"
expect_status 201
REQUEST_2=$(json .id)
# The newest consent email to the shopper is mreq_2's.
sleep 1
for _ in $(seq 1 "$WAIT_SECONDS"); do
	TOKEN_2=$(token_for "$SHOPPER" /consent) || fail "no second consent email reached the customer"
	[ "$TOKEN_2" != "$TOKEN" ] && break
	sleep 1
done
req POST /v1/consent/reject "{\"token\":\"$TOKEN_2\"}"
got
expect_status 200
check '[ "$(json .request.id)" = "$REQUEST_2" ] && [ "$(json .request.status)" = rejected ] && [ "$(json .mandate)" = null ]' "rejected: no mandate"

with_key "$KEY_A" POST "/v1/commerce/mandates/$MANDATE/revoke" ""
got
expect_status 200
check '[ "$(json .status)" = revoked ]' "revoked at once"
for _ in $(seq 1 "$WAIT_SECONDS"); do
	grep -F "\"to\":\"$SHOPPER\"" "$MAIL_FILE" 2>/dev/null | grep -q "has been revoked" && break
	sleep 1
done
check 'grep -F "\"to\":\"$SHOPPER\"" "$MAIL_FILE" | grep -q "has been revoked"' "the customer was told by email"

step "6. Only a merchant key opens commerce, and it opens nothing else"
req POST /v1/agents '{"name":"treasury bot"}' -b "$JAR_A"
expect_status 201
TREASURY_KEY=$(json .api_key.secret)
with_key "$TREASURY_KEY" GET /v1/commerce/customers ""
expect_status 401
req GET /v1/commerce/customers "" -b "$JAR_A"
expect_status 401
with_key "$KEY_A" POST /v1/intents '{"payee":"x","amount":"1.00","currency":"USD","reference":"INV-1"}'
expect_status 401
with_key "$KEY_A" GET /v1/merchant-integrations ""
expect_status 401
pass "an agent key and a session are refused on commerce; the merchant key on treasury and the dashboard"

step "7. A second storefront and a second organisation have their own namespaces"
integration "$JAR_A" "Acme Outlet"
expect_status 201
KEY_A2=$NEW_KEY
with_key "$KEY_A2" GET "/v1/commerce/customers/$CUSTOMER" ""
expect_status 404
with_key "$KEY_A2" POST /v1/commerce/customers '{"external_id":"cust_42","email":"ada@example.com"}'
expect_status 201
check '[ "$(json .id)" != "$CUSTOMER" ]' "the same external ID is a separate customer for the outlet"
owner "$JAR_B" b
integration "$JAR_B" "Other Org Shop"
expect_status 201
KEY_B=$NEW_KEY
with_key "$KEY_B" GET "/v1/commerce/customers/$CUSTOMER" ""
expect_status 404
with_key "$KEY_B" POST "/v1/commerce/agents/$AGENT/disable" ""
expect_status 404
with_key "$KEY_B" GET "/v1/commerce/funding-methods/$BINDING" ""
expect_status 404
with_key "$KEY_B" GET "/v1/commerce/mandates/$MANDATE" ""
expect_status 404
with_key "$KEY_B" GET "/v1/commerce/mandate-requests/$REQUEST" ""
expect_status 404
with_key "$KEY_B" GET "/v1/commerce/customers/$CUSTOMER/funding-methods" ""
expect_status 404
with_key "$KEY_B" POST /v1/commerce/funding-methods "{\"customer_id\":\"$CUSTOMER\",\"provider\":\"stripe\",\"merchant_funding_method_id\":\"mfm_steal\",\"display\":{\"brand\":\"visa\"}}"
expect_status 422
check '[ "$(json .code)" = customer_unknown ]' "another organisation's merchant cannot bind this customer"
req GET "/v1/merchant-integrations/$INTEGRATION_A" "" -b "$JAR_B"
expect_status 404
pass "another merchant's and another organisation's records are not found"

step "8. Disabling the integration stops its key at once, cache included"
with_key "$KEY_A" GET /v1/commerce/customers ""
expect_status 200 # the key is now cached
req POST "/v1/merchant-integrations/$INTEGRATION_A/disable" "" -b "$JAR_A"
got
expect_status 200
with_key "$KEY_A" GET /v1/commerce/customers ""
expect_status 401
req POST "/v1/merchant-integrations/$INTEGRATION_A/enable" "" -b "$JAR_A"
expect_status 200
with_key "$KEY_A" GET /v1/commerce/customers ""
expect_status 200

step "9. Rotating replaces the key; revoking cuts the merchant off"
req POST "/v1/merchant-integrations/$INTEGRATION_A/rotate-key" "" -b "$JAR_A"
expect_status 200
NEW_KEY_A=$(json .key.secret)
with_key "$KEY_A" GET /v1/commerce/customers ""
expect_status 401
with_key "$NEW_KEY_A" GET /v1/commerce/customers ""
expect_status 200
req DELETE "/v1/merchant-integrations/$INTEGRATION_A/key" "" -b "$JAR_A"
expect_status 204
with_key "$NEW_KEY_A" GET /v1/commerce/customers ""
expect_status 401

step "10. The audit log names the merchant that acted, without the customer's email"
req GET "/v1/audit-log?action=commerce.customer.registered" "" -b "$JAR_A"
got
expect_status 200
check '[ "$(json "[.data[] | select(.target.id == \"$CUSTOMER\")][0].actor.type")" = merchant_integration ]' "the actor is the merchant integration"
check '! grep -qi "ada@example.com" <<<"$BODY"' "no customer email in the audit log"
req GET "/v1/audit-log" "" -b "$JAR_A"
for action in merchant_integration.created merchant_integration.disabled merchant_integration.enabled \
	merchant_integration.key_rotated merchant_integration.key_revoked commerce.agent.registered \
	commerce.funding_method.registered commerce.mandate.requested commerce.mandate.approved \
	commerce.mandate.rejected commerce.mandate.revoked; do
	check "grep -qF '\"$action\"' <<<\"\$BODY\"" "recorded: $action"
done

printf '\n\033[32mAll commerce steps passed.\033[0m\n'
