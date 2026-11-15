package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// merchantInOrg registers a customer/agent/funding method against an
// existing merchant integration's key, returning a mandateWorld for it -
// for tests that need two merchant integrations of one organisation,
// where newMandateWorld (which always creates a fresh organisation)
// doesn't fit.
func (f *authFixture) merchantInOrg(t *testing.T, key string) mandateWorld {
	t.Helper()
	var c customerBody
	json.Unmarshal(f.doBearer(t, http.MethodPost, "/v1/commerce/customers", `{"external_id":"cust_42","email":"ada@example.com"}`, key).Body.Bytes(), &c)
	var a commerceAgentBody
	json.Unmarshal(f.doBearer(t, http.MethodPost, "/v1/commerce/agents", `{"external_id":"assistant","name":"Assistant"}`, key).Body.Bytes(), &a)
	var fm fundingBody
	json.Unmarshal(f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods", fundingJSON(c.ID, "mfm_1"), key).Body.Bytes(), &fm)
	return mandateWorld{key: key, customer: c.ID, agent: a.ID, card: fm.ID}
}

// The dashboard's global commerce activity: every checkout/execution
// across every one of an organisation's merchant integrations, never a
// sibling organisation's, never reachable with a merchant key.
func TestOrgActivity_ListsAcrossMerchantsWithinOneOrg(t *testing.T) {
	f := newAuthFixture(t, true)
	_, owner, issued1 := f.newMerchant(t)
	w1 := f.merchantInOrg(t, issued1.Key.Secret)
	m1 := f.approvedMandate(t, w1, "m1", "autonomous")
	e1 := f.redeemedExecution(t, w1, m1, "c1", 1000)

	issued2 := f.createIntegration(t, owner, "Shop 2")
	w2 := f.merchantInOrg(t, issued2.Key.Secret)
	m2 := f.approvedMandate(t, w2, "m2", "autonomous")
	e2 := f.redeemedExecution(t, w2, m2, "c2", 2000)

	var checkouts struct {
		Data []checkoutBody `json:"data"`
	}
	json.Unmarshal(f.do(t, http.MethodGet, "/v1/checkouts", "", owner).Body.Bytes(), &checkouts)
	seen := map[string]bool{}
	for _, c := range checkouts.Data {
		seen[c.ID] = true
	}
	if !seen[e1.CheckoutID] || !seen[e2.CheckoutID] {
		t.Fatalf("org-wide checkouts missing one merchant's: %+v", checkouts.Data)
	}

	var executions struct {
		Data []executionBody `json:"data"`
	}
	json.Unmarshal(f.do(t, http.MethodGet, "/v1/executions", "", owner).Body.Bytes(), &executions)
	if len(executions.Data) != 2 {
		t.Fatalf("org-wide executions = %+v", executions.Data)
	}

	byAgent := f.do(t, http.MethodGet, "/v1/executions?agent_id="+w1.agent, "", owner)
	json.Unmarshal(byAgent.Body.Bytes(), &executions)
	if len(executions.Data) != 1 || executions.Data[0].ID != e1.ID {
		t.Errorf("by agent = %+v", executions.Data)
	}

	_, stranger := f.newOrg(t)
	json.Unmarshal(f.do(t, http.MethodGet, "/v1/checkouts", "", stranger).Body.Bytes(), &checkouts)
	if len(checkouts.Data) != 0 {
		t.Errorf("a sibling organisation sees %d checkouts", len(checkouts.Data))
	}

	for _, path := range []string{"/v1/checkouts", "/v1/executions"} {
		if rec := f.doBearer(t, http.MethodGet, path, "", issued1.Key.Secret); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with a merchant key: %d, want 401 (dashboard reads are session-only)", path, rec.Code)
		}
		if rec := f.do(t, http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session: %d, want 401", path, rec.Code)
		}
	}
}

func TestOrgActivity_GetByID_ScopedToOrg(t *testing.T) {
	f := newAuthFixture(t, true)
	_, owner, issued := f.newMerchant(t)
	w := f.merchantInOrg(t, issued.Key.Secret)
	m := f.approvedMandate(t, w, "m1", "autonomous")
	e := f.redeemedExecution(t, w, m, "c1", 1000)

	var ck checkoutBody
	rec := f.do(t, http.MethodGet, "/v1/checkouts/"+e.CheckoutID, "", owner)
	json.Unmarshal(rec.Body.Bytes(), &ck)
	if rec.Code != http.StatusOK || ck.ID != e.CheckoutID {
		t.Fatalf("get checkout: %d %+v", rec.Code, ck)
	}

	var exec executionBody
	rec = f.do(t, http.MethodGet, "/v1/executions/"+e.ID, "", owner)
	json.Unmarshal(rec.Body.Bytes(), &exec)
	if rec.Code != http.StatusOK || exec.ID != e.ID {
		t.Fatalf("get execution: %d %+v", rec.Code, exec)
	}

	_, stranger := f.newOrg(t)
	if rec := f.do(t, http.MethodGet, "/v1/checkouts/"+e.CheckoutID, "", stranger); rec.Code != http.StatusNotFound {
		t.Errorf("another organisation's checkout: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodGet, "/v1/executions/"+e.ID, "", stranger); rec.Code != http.StatusNotFound {
		t.Errorf("another organisation's execution: %d", rec.Code)
	}
}

func TestCheckoutReplay_OrdersTheFullLifecycle(t *testing.T) {
	f := newAuthFixture(t, true)
	_, owner, issued := f.newMerchant(t)
	w := f.merchantInOrg(t, issued.Key.Secret)
	m := f.approvedMandate(t, w, "m1", "autonomous")
	e := f.redeemedExecution(t, w, m, "c1", 1000)
	f.report(t, w.key, "/v1/commerce/executions/"+e.ID+"/outcome", outcomeJSON("succeeded", "pi_1", 1000, ""))

	var replay struct {
		Checkout   checkoutBody   `json:"checkout"`
		Execution  *executionBody `json:"execution"`
		StartedAt  *string        `json:"started_at"`
		EndedAt    *string        `json:"ended_at"`
		DurationMS int64          `json:"duration_ms"`
		Steps      []struct {
			Type  string `json:"type"`
			Actor string `json:"actor"`
			Title string `json:"title"`
		} `json:"steps"`
	}
	rec := f.do(t, http.MethodGet, "/v1/checkouts/"+e.CheckoutID+"/replay", "", owner)
	json.Unmarshal(rec.Body.Bytes(), &replay)
	if rec.Code != http.StatusOK || replay.Checkout.ID != e.CheckoutID || replay.Execution == nil || replay.Execution.ID != e.ID {
		t.Fatalf("replay: %d %+v", rec.Code, replay)
	}
	if replay.StartedAt == nil || replay.EndedAt == nil {
		t.Errorf("replay timing: %+v", replay)
	}
	wantTypes := []string{"submission", "decision", "grant", "redemption", "execution_report"}
	if len(replay.Steps) != len(wantTypes) {
		t.Fatalf("steps = %+v", replay.Steps)
	}
	for i, want := range wantTypes {
		if replay.Steps[i].Type != want {
			t.Errorf("step %d = %s, want %s", i, replay.Steps[i].Type, want)
		}
	}

	_, stranger := f.newOrg(t)
	if rec := f.do(t, http.MethodGet, "/v1/checkouts/"+e.CheckoutID+"/replay", "", stranger); rec.Code != http.StatusNotFound {
		t.Errorf("another organisation: %d", rec.Code)
	}
}

func TestCheckoutReplay_DeniedCheckoutHasAShortReplay(t *testing.T) {
	f := newAuthFixture(t, true)
	_, owner, issued := f.newMerchant(t)
	w := f.merchantInOrg(t, issued.Key.Secret)
	m := f.approvedMandate(t, w, "m1", "autonomous")
	var denied checkoutBody
	json.Unmarshal(f.doBearer(t, http.MethodPost, "/v1/commerce/checkouts", w.checkoutJSON("over", m, "USD", 99999999), w.key).Body.Bytes(), &denied)
	if denied.Status != "denied" {
		t.Fatalf("expected a denial to set up this test: %+v", denied)
	}

	var replay struct {
		Execution *executionBody `json:"execution"`
		Steps     []struct {
			Type     string `json:"type"`
			Decision *struct {
				Status  string `json:"status"`
				Reasons []struct {
					Code string `json:"code"`
				} `json:"reasons"`
			} `json:"decision"`
		} `json:"steps"`
	}
	rec := f.do(t, http.MethodGet, "/v1/checkouts/"+denied.ID+"/replay", "", owner)
	json.Unmarshal(rec.Body.Bytes(), &replay)
	if rec.Code != http.StatusOK || replay.Execution != nil || len(replay.Steps) != 2 {
		t.Fatalf("replay: %d %+v", rec.Code, replay)
	}
	d := replay.Steps[1].Decision
	if d == nil || d.Status != "denied" || len(d.Reasons) == 0 {
		t.Errorf("decision step: %+v", d)
	}
}

func TestOrgActivity_QueryValidation(t *testing.T) {
	f := newAuthFixture(t, true)
	_, owner := f.newOrg(t)
	for _, path := range []string{
		"/v1/checkouts?from=yesterday",
		"/v1/checkouts?status=lost",
		"/v1/checkouts?currency=XYZ",
		"/v1/executions?status=webhook",
		"/v1/executions?currency=XYZ",
		"/v1/executions?limit=lots",
	} {
		rec := f.do(t, http.MethodGet, path, "", owner)
		if p := decodeProblem(t, rec); rec.Code != 400 || p.Code != "validation_failed" {
			t.Errorf("%s: %d %q, want 400 validation_failed", path, rec.Code, p.Code)
		}
	}
	if rec := f.do(t, http.MethodGet, "/v1/checkouts?status=authorized&currency=USD", "", owner); rec.Code != 200 {
		t.Errorf("a valid checkouts query: %d %s", rec.Code, rec.Body)
	}
}

// A lower-case currency is accepted, matching the treasury /v1/payments
// endpoint's own case-insensitive handling - a client that otherwise
// matches that convention shouldn't get a 400 here.
func TestOrgActivity_CurrencyFilterIsCaseInsensitive(t *testing.T) {
	f := newAuthFixture(t, true)
	_, owner := f.newOrg(t)
	for _, path := range []string{"/v1/checkouts?currency=usd", "/v1/executions?currency=usd"} {
		if rec := f.do(t, http.MethodGet, path, "", owner); rec.Code != 200 {
			t.Errorf("%s: %d %s, want 200", path, rec.Code, rec.Body)
		}
	}
}
