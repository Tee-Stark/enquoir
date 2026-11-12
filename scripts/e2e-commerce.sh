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
# got prints the last response, with any grant secret cut to its prefix.
got() { printf '   -> %s %s\n' "$STATUS" "$(sed -E 's/(enq_gr_[A-Za-z0-9_-]{5})[A-Za-z0-9_-]+/\1…/g' <<<"$BODY")"; }
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
expect_status 202
check '[ "$(json .status)" = revocation_pending ]' "paused at once, awaiting the customer"
REVOKE_TOKEN=$(token_for "$SHOPPER" /consent/revoke) || fail "no revocation email reached the customer"
pass "the confirmation link reached the customer's inbox"
req POST /v1/consent/revocation/confirm "{\"token\":\"$REVOKE_TOKEN\"}"
got
expect_status 200
check '[ "$(json .status)" = revoked ]' "revoked once the customer confirmed"

step "5d. Checkouts: evaluated against the mandate, decided under its lock"
# fresh_token EMAIL PATH OLD: the newest PATH link emailed to EMAIL, once it differs from OLD.
fresh_token() {
	local t=""
	for _ in $(seq 1 "$WAIT_SECONDS"); do
		t=$(token_for "$1" "$2") || true
		[ -n "$t" ] && [ "$t" != "$3" ] && { echo "$t"; return; }
		sleep 1
	done
	return 1
}
approved_mandate() { # EXTERNAL_ID MODE: leaves the mandate's ID in $NEW_MANDATE
	local last
	last=$(token_for "$SHOPPER" /consent) || true
	with_key "$KEY_A" POST /v1/commerce/mandate-requests "$(mandate_request "$1" | sed "s/\"autonomous\"/\"$2\"/")"
	expect_status 201
	local hash token
	hash=$(json .terms.terms_hash)
	token=$(fresh_token "$SHOPPER" /consent "$last") || fail "no consent email for $1"
	req POST /v1/consent/approve "{\"token\":\"$token\",\"terms_hash\":\"$hash\"}"
	expect_status 200
	NEW_MANDATE=$(json .mandate.id)
}
checkout() { # EXTERNAL_ID MANDATE CURRENCY UNIT_PRICE CATEGORY
	echo "{\"external_id\":\"$1\",\"mandate_id\":\"$2\",\"customer_id\":\"$SHOPPER_ID\",\"agent_id\":\"$AGENT\",\"funding_method_id\":\"$SHOPPER_CARD\",\"checkout\":{\"currency\":\"$3\",\"items\":[{\"product_id\":\"laptop_001\",\"name\":\"Lenovo ThinkPad\",\"category\":\"$5\",\"quantity\":1,\"unit_price_minor\":$4}],\"shipping_minor\":500,\"tax_minor\":0,\"fees_minor\":0,\"total_minor\":$(($4 + 500))}}"
}
approved_mandate mreq_auto autonomous
AUTO=$NEW_MANDATE
show "curl -s -H 'Authorization: Bearer $(redact "$KEY_A")' -X POST $API/v1/commerce/checkouts -d '$(checkout chk_1 "$AUTO" USD 130000 electronics)'"
with_key "$KEY_A" POST /v1/commerce/checkouts "$(checkout chk_1 "$AUTO" USD 130000 electronics)"
got
expect_status 201
CHK_1=$(json .id)
check '[ "$(json .status)" = authorized ] && [ "$(json .decision.allowed)" = true ]' "within the mandate: authorized at once"
with_key "$KEY_A" POST /v1/commerce/checkouts "$(checkout chk_1 "$AUTO" USD 130000 electronics)"
expect_status 200
with_key "$KEY_A" POST /v1/commerce/checkouts "$(checkout chk_2 "$AUTO" EUR 160000 books)"
got
expect_status 201
check '[ "$(json "[.decision.reasons[].code] | join(\",\")")" = "currency_mismatch,over_transaction_limit,category_not_allowed" ]' "outside it: denied, with every reason"
with_key "$KEY_A" POST /v1/commerce/checkouts "$(checkout chk_3 "$AUTO" USD 149500 electronics)"
check '[ "$(json .status)" = authorized ]' "a second purchase: 2,805 of the 3,000 now authorized"
with_key "$KEY_A" POST /v1/commerce/checkouts "$(checkout chk_3b "$AUTO" USD 149500 electronics)"
check '[ "$(json .status)" = denied ] && [ "$(json ".decision.reasons[0].code")" = over_budget ]' "authorized checkouts count: the third is over budget"
with_key "$KEY_A" POST "/v1/commerce/checkouts/$CHK_1/cancel" ""
expect_status 200
with_key "$KEY_A" POST /v1/commerce/checkouts "$(checkout chk_4 "$AUTO" USD 149500 electronics)"
check '[ "$(json .status)" = authorized ]' "cancelling released it"
CHK_4=$(json .id)
CHK_4_HASH=$(json .checkout_hash)
GRANT_4=$(json .authorization.grant)
check '[[ "$GRANT_4" == enq_gr_* ]] && [ "$(json .authorization.status)" = active ]' "the authorization comes with its one-time grant"
with_key "$KEY_A" POST /v1/commerce/checkouts "$(checkout chk_5 "$AUTO" USD 1000 electronics | sed 's/"quantity":1,/"quantity":1,"agent_price":1,/')"
expect_status 400
check '[ "$(json .field)" = agent_price ]' "a price the agent asserts is refused"

approved_mandate mreq_per per_transaction
PER=$NEW_MANDATE
LAST=$(token_for "$SHOPPER" /consent/checkout) || true
with_key "$KEY_A" POST /v1/commerce/checkouts "$(checkout chk_6 "$PER" USD 100000 electronics)"
got
expect_status 201
check '[ "$(json .status)" = awaiting_customer_approval ] && [ "$(json .decision.allowed)" = false ]' "per transaction: awaits the customer"
CHK_HASH=$(json .checkout_hash)
CHK_TOKEN=$(fresh_token "$SHOPPER" /consent/checkout "$LAST") || fail "no checkout approval email reached the customer"
pass "the approval link reached the customer's inbox"
req POST /v1/consent/checkout/view "{\"token\":\"$CHK_TOKEN\"}"
check '[ "$(json .checkout_hash)" = "$CHK_HASH" ]' "the page shows that exact checkout"
req POST /v1/consent/checkout/approve "{\"token\":\"$CHK_TOKEN\",\"checkout_hash\":\"$CHK_HASH\"}"
got
expect_status 200
check '[ "$(json .status)" = authorized ]' "approved by the customer: authorized"

step "5e. Grants: redeemed once before charging, never twice"
redeem_body() { echo "{\"grant\":\"$1\",\"external_checkout_id\":\"$2\",\"checkout_hash\":\"$3\"}"; }
with_key "$KEY_A" GET "/v1/commerce/checkouts/$CHK_4" ""
check '! grep -qF "$GRANT_4" <<<"$BODY" && [ "$(json .authorization.grant)" = null ]' "the grant is never shown again"
with_key "$KEY_A" POST /v1/commerce/grants/redeem "$(redeem_body "$GRANT_4" chk_9 "$CHK_4_HASH")"
expect_status 409
check '[ "$(json .code)" = redemption_mismatch ]' "a grant redeems only its own checkout"
show "curl -s -H 'Authorization: Bearer $(redact "$KEY_A")' -X POST $API/v1/commerce/grants/redeem -d '{\"grant\":\"$(redact "$GRANT_4")\",\"external_checkout_id\":\"chk_4\",\"checkout_hash\":\"$CHK_4_HASH\"}'"
with_key "$KEY_A" POST /v1/commerce/grants/redeem "$(redeem_body "$GRANT_4" chk_4 "$CHK_4_HASH")"
got
expect_status 201
EXECUTION=$(json .id)
check '[ "$(json .amount_minor)" = 150000 ] && [ "$(json .funding_method.id)" = "$SHOPPER_CARD" ] && [ "$(json .funding_method.provider_method_reference)" = pm_1ShopperCard ]' "the execution says what to charge, and with which saved method"
for _ in 1 2 3; do
	with_key "$KEY_A" POST /v1/commerce/grants/redeem "$(redeem_body "$GRANT_4" chk_4 "$CHK_4_HASH")"
	[ "$STATUS" = 200 ] && [ "$(json .id)" = "$EXECUTION" ] || fail "a repeated redemption made something new"
done
pass "repeated redemptions return the same execution"
with_key "$KEY_A" POST "/v1/commerce/checkouts/$CHK_4/cancel" ""
expect_status 409
with_key "$KEY_A" POST "/v1/commerce/checkouts/$CHK_4/grant" ""
check '[ "$STATUS" = 409 ] && [ "$(json .code)" = grant_redeemed ]' "a redeemed checkout can't be cancelled or re-granted"
with_key "$KEY_A" POST /v1/commerce/grants/redeem "$(redeem_body enq_gr_made_up chk_4 "$CHK_4_HASH")"
check '[ "$STATUS" = 422 ] && [ "$(json .code)" = grant_invalid ]' "a made-up grant is invalid"
# The customer approved chk_6: the merchant has its grant issued, then redeems it.
with_key "$KEY_A" POST /v1/commerce/checkouts "$(checkout chk_6 "$PER" USD 100000 electronics)"
CHK_6=$(json .id)
check '[ "$(json .authorization.grant)" = null ]' "the customer's approval issued no grant"
with_key "$KEY_A" POST "/v1/commerce/checkouts/$CHK_6/grant" ""
expect_status 200
GRANT_6=$(json .authorization.grant)
with_key "$KEY_A" POST /v1/commerce/grants/redeem "$(redeem_body "$GRANT_6" chk_6 "$CHK_HASH")"
expect_status 201
pass "issued to the merchant after approval, then redeemed"

step "5f. Outcomes: succeeded stays spent, failed is released, unknown waits for reconciliation"
outcome_body() { # OUTCOME REF AMOUNT [EXTRA]
	local ref=""
	[ -n "$2" ] && ref="\"provider_payment_reference\":\"$2\","
	echo "{\"outcome\":\"$1\",${ref}\"provider\":\"stripe\",\"amount_minor\":$3,\"currency\":\"USD\"${4:-}}"
}
with_key "$KEY_A" GET "/v1/commerce/executions/$EXECUTION" ""
check '[ "$(json .idempotency_key)" = "enquoir-$EXECUTION" ] && [ "$(json .status)" = pending ]' "the execution gives an idempotency key for the PSP request"
show "curl -s -H 'Authorization: Bearer $(redact "$KEY_A")' -X POST $API/v1/commerce/executions/$EXECUTION/outcome -d '$(outcome_body succeeded pi_e2e_1 150000)'"
with_key "$KEY_A" POST "/v1/commerce/executions/$EXECUTION/outcome" "$(outcome_body succeeded pi_e2e_1 150000)"
got
expect_status 200
check '[ "$(json .status)" = succeeded ] && [ "$(json .result)" = applied ]' "succeeded"
HOOK=",\"provider_event_id\":\"evt_e2e_$RUN\""
with_key "$KEY_A" POST "/v1/commerce/executions/$EXECUTION/outcome" "$(outcome_body succeeded pi_e2e_1 150000 "$HOOK")"
with_key "$KEY_A" POST "/v1/commerce/executions/$EXECUTION/outcome" "$(outcome_body succeeded pi_e2e_1 150000 "$HOOK")"
check '[ "$STATUS" = 200 ] && [ "$(json .result)" = duplicate ]' "a redelivered webhook changes nothing"
with_key "$KEY_A" POST "/v1/commerce/executions/$EXECUTION/reconcile" "$(outcome_body failed pi_e2e_1 150000)"
check '[ "$STATUS" = 409 ] && [ "$(json .code)" = outcome_conflict ]' "a finished payment can't be contradicted"
with_key "$KEY_A" POST "/v1/commerce/executions/$EXECUTION/outcome" "$(outcome_body succeeded pi_e2e_1 1)"
check '[ "$STATUS" = 422 ] && [ "$(json .code)" = report_mismatch ]' "another amount is refused"
with_key "$KEY_A" GET "/v1/commerce/executions/$EXECUTION" ""
check '[ "$(json "[.history[].kind] | join(\",\")")" = "redeemed,reported,duplicate,duplicate,refused,refused" ]' "every report is in the history, refused ones too"
with_key "$KEY_A" GET "/v1/commerce/checkouts/$CHK_4" ""
check '[ "$(json .status)" = paid ]' "its checkout is paid"

# chk_6's execution: the merchant's PSP call timed out.
with_key "$KEY_A" GET "/v1/commerce/executions?mandate_id=$PER" ""
EXEC_6=$(json ".data[0].id")
with_key "$KEY_A" POST "/v1/commerce/executions/$EXEC_6/outcome" "$(outcome_body unknown "" 100500)"
check '[ "$(json .status)" = unknown ]' "a timeout is reported unknown, not failed"
with_key "$KEY_A" POST "/v1/commerce/executions/$EXEC_6/outcome" "$(outcome_body failed "" 100500)"
check '[ "$STATUS" = 409 ] && [ "$(json .code)" = reconciliation_required ]' "unknown is resolved only by reconciling"
with_key "$KEY_A" GET "/v1/commerce/executions?status=unknown" ""
check '[ "$(json ".data | map(.id) | index(\"$EXEC_6\")")" != null ]' "it is in the reconciliation queue"
with_key "$KEY_A" GET "/v1/commerce/mandates/$PER" ""
check '[ "$(json .usage.held_minor)" = 100500 ]' "unknown keeps its amount held"
with_key "$KEY_A" POST "/v1/commerce/executions/$EXEC_6/reconcile" "$(outcome_body failed pi_e2e_6 100500)"
check '[ "$(json .status)" = failed ]' "reconciled: it failed"
with_key "$KEY_A" GET "/v1/commerce/mandates/$PER" ""
check '[ "$(json .usage.spent_minor)" = 0 ]' "the failed payment is released"
with_key "$KEY_A" GET "/v1/commerce/mandates/$AUTO" ""
got
check '[ "$(json .usage.paid_minor)" = 150000 ] && [ "$(json .usage.purchases)" -ge 1 ]' "the mandate shows its usage"
req GET "/v1/merchant-integrations/$INTEGRATION_A/checkouts?status=paid" "" -b "$JAR_A"
check '[ "$STATUS" = 200 ] && [ "$(json ".data | length")" = 1 ]' "the dashboard lists the merchant's checkouts"

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
with_key "$KEY_B" GET "/v1/commerce/checkouts/$CHK_1" ""
expect_status 404
with_key "$KEY_B" GET "/v1/commerce/executions/$EXECUTION" ""
expect_status 404
with_key "$KEY_B" POST "/v1/commerce/executions/$EXECUTION/outcome" "$(outcome_body failed "" 150000)"
expect_status 404
req GET "/v1/merchant-integrations/$INTEGRATION_A/executions" "" -b "$JAR_B"
expect_status 404
with_key "$KEY_B" POST /v1/commerce/grants/redeem "$(redeem_body "$GRANT_6" chk_6 "$CHK_HASH")"
expect_status 422
with_key "$KEY_B" POST /v1/commerce/checkouts "$(checkout chk_steal "$AUTO" USD 1000 electronics)"
expect_status 422
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
	commerce.mandate.rejected commerce.mandate.revoked commerce.checkout.authorized commerce.checkout.denied \
	commerce.checkout.awaiting_approval commerce.checkout.approved commerce.checkout.canceled \
	commerce.authorization.grant_issued commerce.authorization.redeemed commerce.execution.succeeded \
	commerce.execution.unknown commerce.execution.reconciled commerce.execution.report_refused; do
	check "grep -qF '\"$action\"' <<<\"\$BODY\"" "recorded: $action"
done

printf '\n\033[32mAll commerce steps passed.\033[0m\n'
