package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"enquoir/internal/domain"
)

func postIntentAs(t *testing.T, f *authFixture, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/intents", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

const validIntentJSON = `{"payee":"payee-1","amount":"10.50","currency":"USD","reference":"INV-1001","reason":"invoice settlement"}`

func TestSubmitIntent_ValidRequestReturnsKeyAndState(t *testing.T) {
	f := newAuthFixture(t, true)
	_, bearer := f.newAgent(t)

	rec := postIntentAs(t, f, bearer, validIntentJSON)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp submitIntentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Key == "" {
		t.Error("expected a non-empty key")
	}
	if resp.State != string(domain.PROPOSED) {
		t.Errorf("state = %q, want %q", resp.State, domain.PROPOSED)
	}
	if f.starter.CallCount() != 1 {
		t.Errorf("Starter called %d times, want 1", f.starter.CallCount())
	}
}

func TestSubmitIntent_WithoutAnAPIKeyIsUnauthenticated(t *testing.T) {
	f := newAuthFixture(t, true)

	rec := postIntentAs(t, f, "", validIntentJSON)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d (body: %s)", rec.Code, http.StatusUnauthorized, rec.Body.String())
	}
	if f.starter.CallCount() != 0 {
		t.Errorf("Starter called %d times for an unauthenticated request, want 0", f.starter.CallCount())
	}
}

func TestSubmitIntent_WithARevokedOrUnknownKeyIsUnauthenticated(t *testing.T) {
	f := newAuthFixture(t, true)

	for _, bad := range []string{"", "enq_sk_made_up"} {
		rec := postIntentAs(t, f, bad, validIntentJSON)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("bearer %q: status = %d, want %d", bad, rec.Code, http.StatusUnauthorized)
		}
	}
}

func TestSubmitIntent_SameIntentTwiceReturnsSameKeyAndStartsNothingNew(t *testing.T) {
	f := newAuthFixture(t, true)
	_, bearer := f.newAgent(t)

	first := postIntentAs(t, f, bearer, validIntentJSON)
	second := postIntentAs(t, f, bearer, validIntentJSON)

	var firstResp, secondResp submitIntentResponse
	json.Unmarshal(first.Body.Bytes(), &firstResp)
	json.Unmarshal(second.Body.Bytes(), &secondResp)

	if firstResp.Key != secondResp.Key {
		t.Errorf("same intent produced different keys: %q vs %q", firstResp.Key, secondResp.Key)
	}
	if f.starter.CallCount() != 2 {
		t.Errorf("Starter called %d times, want 2 (attach semantics are the starter's job, not this handler's)", f.starter.CallCount())
	}
}

// Tenants are isolated: two organisations submitting the identical
// intent must get different keys, since each agent authenticates as
// its own organisation.
func TestSubmitIntent_SameIntentFromTwoOrganisationsGetsDifferentKeys(t *testing.T) {
	f := newAuthFixture(t, true)
	_, bearerA := f.newAgent(t)
	_, bearerB := f.newAgent(t)

	a := postIntentAs(t, f, bearerA, validIntentJSON)
	b := postIntentAs(t, f, bearerB, validIntentJSON)

	var respA, respB submitIntentResponse
	json.Unmarshal(a.Body.Bytes(), &respA)
	json.Unmarshal(b.Body.Bytes(), &respB)

	if respA.Key == respB.Key {
		t.Errorf("two organisations submitting the identical intent got the same key %q", respA.Key)
	}
}

// An agent retrying a payment rewords its reason. Once the first
// submission has history, the reworded one must resolve to the same key
// and start nothing.
func TestSubmitIntent_RewordedReasonResolvesToSameIntentAndStartsNothingNew(t *testing.T) {
	f := newAuthFixture(t, true)
	org, bearer := f.newAgent(t)

	first := postIntentAs(t, f, bearer, validIntentJSON)
	var firstResp submitIntentResponse
	if err := json.Unmarshal(first.Body.Bytes(), &firstResp); err != nil {
		t.Fatalf("decoding first response: %v", err)
	}
	// The worker has picked the first run up and recorded its start.
	f.events.Append(context.Background(), org, domain.IntentKey(firstResp.Key), string(domain.PROPOSED), "")

	reworded := `{"payee":"payee-1","amount":"10.50","currency":"USD","reference":"INV-1001","reason":"retrying payment for INV-1001 after a timeout"}`
	second := postIntentAs(t, f, bearer, reworded)
	if second.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", second.Code, http.StatusOK, second.Body.String())
	}
	var secondResp submitIntentResponse
	if err := json.Unmarshal(second.Body.Bytes(), &secondResp); err != nil {
		t.Fatalf("decoding second response: %v", err)
	}

	if secondResp.Key != firstResp.Key {
		t.Errorf("reworded reason produced a different key: %q vs %q", secondResp.Key, firstResp.Key)
	}
	if f.starter.CallCount() != 1 {
		t.Errorf("Starter called %d times, want 1: the reworded retry must not start a second payment", f.starter.CallCount())
	}
}

func TestSubmitIntent_RejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"missing payee":        `{"amount":"10.50","currency":"USD","reference":"INV-1","reason":"x"}`,
		"zero amount":          `{"payee":"p","amount":"0.00","currency":"USD","reference":"INV-1","reason":"x"}`,
		"unsupported currency": `{"payee":"p","amount":"10.50","currency":"JPY","reference":"INV-1","reason":"x"}`,
		"malformed JSON":       `{not json`,
		"blank reference":      `{"payee":"p","amount":"10.50","currency":"USD","reference":"  ","reason":"x"}`,
	}
	f := newAuthFixture(t, true)
	_, bearer := f.newAgent(t)
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rec := postIntentAs(t, f, bearer, body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

func getIntentAs(t *testing.T, f *authFixture, bearer, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/intents/"+key, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func TestGetIntent_ReturnsStateAndHistory(t *testing.T) {
	f := newAuthFixture(t, true)
	org, bearer := f.newAgent(t)

	intent, err := domain.NewIntent(org, "trs_test", "payee-1", 1050, "USD", "INV-2", "test")
	if err != nil {
		t.Fatalf("building intent: %v", err)
	}
	ctx := context.Background()
	f.events.Append(ctx, org, intent.Key, "PROPOSED", "")
	f.events.Append(ctx, org, intent.Key, "AUTHORIZED", "")

	rec := getIntentAs(t, f, bearer, string(intent.Key))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp getIntentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.State != string(domain.AUTHORIZED) {
		t.Errorf("state = %q, want %q", resp.State, domain.AUTHORIZED)
	}
	if len(resp.Events) != 2 {
		t.Errorf("len(events) = %d, want 2", len(resp.Events))
	}
	if resp.Events[0].Type != "PROPOSED" || resp.Events[1].Type != "AUTHORIZED" {
		t.Errorf("events out of order or wrong: %+v", resp.Events)
	}
}

func TestGetIntent_UnknownKeyReturnsNotFound(t *testing.T) {
	f := newAuthFixture(t, true)
	_, bearer := f.newAgent(t)

	rec := getIntentAs(t, f, bearer, "does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

// Tenants are isolated: another organisation's agent must not be able
// to read a key it never submitted, even by its exact value.
func TestGetIntent_KeyFromAnotherOrganisationReturnsNotFound(t *testing.T) {
	f := newAuthFixture(t, true)
	orgA, bearerA := f.newAgent(t)
	_, bearerB := f.newAgent(t)

	intent, err := domain.NewIntent(orgA, "trs_test", "payee-1", 1050, "USD", "INV-3", "test")
	if err != nil {
		t.Fatalf("building intent: %v", err)
	}
	f.events.Append(context.Background(), orgA, intent.Key, "PROPOSED", "")

	if rec := getIntentAs(t, f, bearerA, string(intent.Key)); rec.Code != http.StatusOK {
		t.Fatalf("owner: status = %d, want %d", rec.Code, http.StatusOK)
	}
	if rec := getIntentAs(t, f, bearerB, string(intent.Key)); rec.Code != http.StatusNotFound {
		t.Errorf("another organisation: status = %d, want %d (body: %s)", rec.Code, http.StatusNotFound, rec.Body.String())
	}
}

func TestGetIntent_WithoutAnAPIKeyIsUnauthenticated(t *testing.T) {
	f := newAuthFixture(t, true)
	rec := getIntentAs(t, f, "", "some-key")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHealthz_ReturnsOK(t *testing.T) {
	f := newAuthFixture(t, true)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}

// One organisation's agent cannot pay a payee another organisation
// registered: the shared rail account would pay it, so Enquoir refuses.
func TestSubmitIntent_AnotherOrganisationsPayeeIsRefused(t *testing.T) {
	registrar := &fakeRegistrar{}
	f, bearerA := newBeneficiaryFixture(t, registrar)
	_, bearerB := f.newAgent(t)

	registered := decodeRegister(t, postBeneficiaryAs(t, f, bearerA, validBeneficiaryBody))
	intent := fmt.Sprintf(`{"payee":%q,"amount":"10.50","currency":"USD","reference":"INV-X"}`, registered.ID)

	rec := postIntentAs(t, f, bearerB, intent)
	if p := decodeProblem(t, rec); rec.Code != http.StatusUnprocessableEntity || p.Code != "payee_unknown" {
		t.Errorf("org B paying org A's payee: %d %q, want 422 payee_unknown", rec.Code, p.Code)
	}
	if f.starter.CallCount() != 0 {
		t.Errorf("a workflow started for another organisation's payee")
	}

	if rec := postIntentAs(t, f, bearerA, intent); rec.Code != http.StatusOK {
		t.Errorf("org A paying its own payee: %d %s, want 200", rec.Code, rec.Body)
	}
}

func TestSubmitIntent_ReportsWhatTheSubmissionDidAndRecordsEveryOne(t *testing.T) {
	f := newAuthFixture(t, true)
	_, bearer := f.newAgent(t)

	var first, second submitIntentResponse
	json.Unmarshal(postIntentAs(t, f, bearer, validIntentJSON).Body.Bytes(), &first)
	f.events.Append(context.Background(), f.starter.Calls()[0].OrgID, domain.IntentKey(first.Key), "PROPOSED", "")
	json.Unmarshal(postIntentAs(t, f, bearer, validIntentJSON).Body.Bytes(), &second)
	if first.Outcome != "started" || second.Outcome != "deduplicated" {
		t.Errorf("outcomes = %q then %q, want started then deduplicated", first.Outcome, second.Outcome)
	}

	postIntentAs(t, f, bearer, `{"payee":"payee-1","amount":"1.005","currency":"USD","reference":"R"}`)
	all := f.attempts.All()
	if len(all) != 3 || all[2].Outcome != "invalid" {
		t.Errorf("attempts = %d (last %q), want 3, the last invalid", len(all), all[len(all)-1].Outcome)
	}
}
