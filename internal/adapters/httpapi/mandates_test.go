package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type mandateTermsBody struct {
	Merchant struct{ ID, Name string } `json:"merchant"`
	Agent    struct{ ID, Name string } `json:"agent"`
	Funding  struct {
		ID       string  `json:"id"`
		Brand    string  `json:"brand"`
		LastFour *string `json:"last_four"`
	} `json:"funding_method"`
	CustomerID   string `json:"customer_id"`
	ApprovalMode string `json:"approval_mode"`
	Scope        struct {
		Currency          string   `json:"currency"`
		MaxTransaction    int64    `json:"max_transaction_minor"`
		MaxCumulative     int64    `json:"max_cumulative_minor"`
		MaxTransactions   *int     `json:"max_transactions"`
		AllowedCategories []string `json:"allowed_categories"`
	} `json:"scope"`
	ExpiresAt string `json:"expires_at"`
	Hash      string `json:"terms_hash"`
}

type mandateRequestBody struct {
	ID         string           `json:"id"`
	ExternalID string           `json:"external_id"`
	Status     string           `json:"status"`
	Terms      mandateTermsBody `json:"terms"`
	ExpiresAt  string           `json:"expires_at"`
	MandateID  *string          `json:"mandate_id"`
}

type mandateBody struct {
	ID        string           `json:"id"`
	RequestID string           `json:"request_id"`
	Status    string           `json:"status"`
	Terms     mandateTermsBody `json:"terms"`
	RevokedAt *string          `json:"revoked_at"`
}

// mandateWorld is a merchant with a customer (ada@example.com), an agent
// and the customer's card.
type mandateWorld struct {
	key, customer, agent, card string
}

func (f *authFixture) newMandateWorld(t *testing.T) mandateWorld {
	t.Helper()
	key, customer := f.merchantWithCustomer(t)
	var a commerceAgentBody
	json.Unmarshal(f.doBearer(t, http.MethodPost, "/v1/commerce/agents", `{"external_id":"assistant","name":"Acme Shopping Assistant"}`, key).Body.Bytes(), &a)
	var c fundingBody
	json.Unmarshal(f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods", fundingJSON(customer, "mfm_1"), key).Body.Bytes(), &c)
	return mandateWorld{key: key, customer: customer, agent: a.ID, card: c.ID}
}

func (w mandateWorld) requestJSON(ext string, expires time.Time) string {
	return fmt.Sprintf(`{"external_id":%q,"customer_id":%q,"agent_id":%q,"funding_method_id":%q,"approval_mode":"autonomous",
		"scope":{"currency":"USD","max_transaction_minor":150000,"max_cumulative_minor":300000,"max_transactions":3,"allowed_categories":["electronics"]},
		"expires_at":%q}`, ext, w.customer, w.agent, w.card, expires.UTC().Format(time.RFC3339))
}

func (f *authFixture) requestMandate(t *testing.T, w mandateWorld, ext string) mandateRequestBody {
	t.Helper()
	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/mandate-requests", w.requestJSON(ext, f.clock.Now().Add(30*24*time.Hour)), w.key)
	if rec.Code != http.StatusCreated {
		t.Fatalf("requesting a mandate: %d %s", rec.Code, rec.Body)
	}
	var r mandateRequestBody
	json.Unmarshal(rec.Body.Bytes(), &r)
	return r
}

func (f *authFixture) consentToken(t *testing.T) string {
	t.Helper()
	tok := f.mailer.TokenTo("ada@example.com")
	if tok == "" {
		t.Fatal("no consent link was emailed to the customer")
	}
	return tok
}

func TestMandates_TheWholeConsentFlow(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)

	r := f.requestMandate(t, w, "mreq_1")
	if r.Status != "pending" || r.MandateID != nil || r.Terms.Merchant.Name != "Acme Electronics" || r.Terms.Agent.Name != "Acme Shopping Assistant" ||
		r.Terms.Funding.LastFour == nil || *r.Terms.Funding.LastFour != "4242" || r.Terms.Scope.MaxCumulative != 300000 ||
		r.Terms.Scope.MaxTransactions == nil || *r.Terms.Scope.MaxTransactions != 3 || !strings.HasPrefix(r.Terms.Hash, "sha256:") {
		t.Fatalf("request = %+v", r)
	}
	token := f.consentToken(t)
	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/mandate-requests", w.requestJSON("mreq_1", f.clock.Now().Add(30*24*time.Hour)), w.key)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), token) {
		t.Fatalf("repeat: %d %s - want 200, and never the token", rec.Code, rec.Body)
	}

	// The consent page: no credential but the link.
	rec = f.do(t, http.MethodPost, "/v1/consent/view", fmt.Sprintf(`{"token":%q}`, token))
	var view struct {
		RequestID string           `json:"request_id"`
		Terms     mandateTermsBody `json:"terms"`
	}
	json.Unmarshal(rec.Body.Bytes(), &view)
	if rec.Code != http.StatusOK || view.RequestID != r.ID || view.Terms.Hash != r.Terms.Hash {
		t.Fatalf("view: %d %s", rec.Code, rec.Body)
	}
	rec = f.do(t, http.MethodPost, "/v1/consent/approve", fmt.Sprintf(`{"token":%q,"terms_hash":"sha256:other"}`, token))
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "terms_mismatch" {
		t.Errorf("approving other terms: %d %q", rec.Code, p.Code)
	}
	rec = f.do(t, http.MethodPost, "/v1/consent/approve", fmt.Sprintf(`{"token":%q,"terms_hash":%q}`, token, r.Terms.Hash))
	var approved struct {
		Request mandateRequestBody `json:"request"`
		Mandate *mandateBody       `json:"mandate"`
	}
	json.Unmarshal(rec.Body.Bytes(), &approved)
	if rec.Code != http.StatusOK || approved.Mandate == nil || approved.Mandate.Status != "active" || approved.Request.Status != "approved" {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodPost, "/v1/consent/approve", fmt.Sprintf(`{"token":%q,"terms_hash":%q}`, token, r.Terms.Hash)); rec.Code != http.StatusBadRequest {
		t.Errorf("the link a second time: %d, want 400", rec.Code)
	}

	// The merchant sees the outcome.
	rec = f.doBearer(t, http.MethodGet, "/v1/commerce/mandate-requests/"+r.ID, "", w.key)
	var got mandateRequestBody
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Status != "approved" || got.MandateID == nil || *got.MandateID != approved.Mandate.ID {
		t.Errorf("request as the merchant sees it: %s", rec.Body)
	}
	var list struct {
		Data []mandateBody `json:"data"`
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/mandates?customer_id="+w.customer+"&status=active", "", w.key).Body.Bytes(), &list)
	if len(list.Data) != 1 || list.Data[0].ID != approved.Mandate.ID {
		t.Errorf("active mandates: %+v", list.Data)
	}

	// The merchant asks to revoke, for the customer; the mandate pauses
	// at once, and only the customer's confirmation makes it final.
	before := len(f.mailer.Sent())
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/mandates/"+approved.Mandate.ID+"/revoke", "", w.key)
	var pending struct {
		mandateBody
		RevocationExpiresAt *string `json:"revocation_expires_at"`
	}
	json.Unmarshal(rec.Body.Bytes(), &pending)
	if rec.Code != http.StatusAccepted || pending.Status != "revocation_pending" || pending.RevokedAt != nil || pending.RevocationExpiresAt == nil {
		t.Fatalf("asking to revoke: %d %s", rec.Code, rec.Body)
	}
	sent := f.mailer.Sent()
	link := f.mailer.LastLinkTo("ada@example.com")
	if len(sent) != before+1 || !strings.Contains(link, "/consent/revoke?token=") {
		t.Fatalf("confirmation email: %d emails, link %q", len(sent)-before, link)
	}
	revokeToken := f.consentToken(t)
	if rec := f.do(t, http.MethodPost, "/v1/consent/revocation/view", fmt.Sprintf(`{"token":%q}`, revokeToken)); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"revocation_pending"`) {
		t.Errorf("confirmation page: %d %s", rec.Code, rec.Body)
	}
	rec = f.do(t, http.MethodPost, "/v1/consent/revocation/confirm", fmt.Sprintf(`{"token":%q}`, revokeToken))
	var revoked mandateBody
	json.Unmarshal(rec.Body.Bytes(), &revoked)
	if rec.Code != http.StatusOK || revoked.Status != "revoked" || revoked.RevokedAt == nil {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodPost, "/v1/consent/revocation/confirm", fmt.Sprintf(`{"token":%q}`, token)); rec.Code != http.StatusBadRequest {
		t.Errorf("confirming with the consent link: %d, want 400", rec.Code)
	}
}

func TestMandates_TheCustomerCanReject(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)
	r := f.requestMandate(t, w, "mreq_1")
	rec := f.do(t, http.MethodPost, "/v1/consent/reject", fmt.Sprintf(`{"token":%q}`, f.consentToken(t)))
	var out struct {
		Request mandateRequestBody `json:"request"`
		Mandate *mandateBody       `json:"mandate"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.Request.Status != "rejected" || out.Mandate != nil || out.Request.ID != r.ID {
		t.Fatalf("reject: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Data []mandateBody `json:"data"`
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/mandates", "", w.key).Body.Bytes(), &list)
	if len(list.Data) != 0 {
		t.Errorf("a rejection made %d mandates", len(list.Data))
	}
}

// Nothing a merchant can send approves: it has no route to, and its key
// is no consent token.
func TestMandates_AMerchantCannotApprove(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)
	r := f.requestMandate(t, w, "mreq_1")
	// Asked of the bare router: these routes are deliberately not in the
	// contract, which the contract-checking wrapper would report.
	bare := NewRouter(f.handler)
	for _, path := range []string{"/v1/commerce/mandate-requests/" + r.ID + "/approve", "/v1/commerce/mandates"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"approved":true}`))
		req.Header.Set("Authorization", "Bearer "+w.key)
		rec := httptest.NewRecorder()
		bare.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s with a merchant key: %d, want no such route", path, rec.Code)
		}
	}
	for _, body := range []string{`{"token":""}`, fmt.Sprintf(`{"token":%q,"terms_hash":%q}`, w.key, r.Terms.Hash), fmt.Sprintf(`{"token":%q,"terms_hash":%q}`, r.ID, r.Terms.Hash)} {
		rec := f.doBearer(t, http.MethodPost, "/v1/consent/approve", body, w.key)
		if p := decodeProblem(t, rec); rec.Code != http.StatusBadRequest || p.Code != "token_invalid" {
			t.Errorf("approving with %s: %d %q, want 400 token_invalid", body, rec.Code, p.Code)
		}
	}
}

func TestMandateRequests_Validation(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)
	_, otherCustomer := f.merchantWithCustomer(t)
	exp := f.clock.Now().Add(24 * time.Hour)

	codes := map[string]struct {
		body   string
		status int
		code   string
		field  string
	}{
		"another merchant's customer": {strings.Replace(w.requestJSON("a", exp), w.customer, otherCustomer, 1), 422, "customer_unknown", ""},
		"made-up agent":               {strings.Replace(w.requestJSON("b", exp), w.agent, "cagt_nope", 1), 422, "agent_unknown", ""},
		"made-up card":                {strings.Replace(w.requestJSON("c", exp), w.card, "fmb_nope", 1), 422, "funding_method_unknown", ""},
		"NGN":                         {strings.Replace(w.requestJSON("d", exp), `"USD"`, `"NGN"`, 1), 400, "validation_failed", "scope.currency"},
		"over 90 days":                {w.requestJSON("e", f.clock.Now().Add(91*24*time.Hour)), 400, "validation_failed", "expires_at"},
		"not a time":                  {strings.Replace(w.requestJSON("f", exp), exp.UTC().Format(time.RFC3339), "tomorrow", 1), 400, "validation_failed", "expires_at"},
		"bad mode":                    {strings.Replace(w.requestJSON("g", exp), "autonomous", "always", 1), 400, "validation_failed", "approval_mode"},
		"no scope":                    {fmt.Sprintf(`{"external_id":"h","customer_id":%q,"agent_id":%q,"funding_method_id":%q,"approval_mode":"autonomous","expires_at":%q}`, w.customer, w.agent, w.card, exp.Format(time.RFC3339)), 400, "validation_failed", "scope.currency"},
	}
	for name, tc := range codes {
		rec := f.doBearer(t, http.MethodPost, "/v1/commerce/mandate-requests", tc.body, w.key)
		p := decodeProblem(t, rec)
		if rec.Code != tc.status || p.Code != tc.code || (tc.field != "" && p.Field != tc.field) {
			t.Errorf("%s: %d %+v, want %d %s %s", name, rec.Code, p, tc.status, tc.code, tc.field)
		}
	}
	f.doBearer(t, http.MethodPost, "/v1/commerce/agents/"+w.agent+"/disable", "", w.key)
	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/mandate-requests", w.requestJSON("i", exp), w.key)
	if p := decodeProblem(t, rec); rec.Code != 422 || p.Code != "agent_disabled" {
		t.Errorf("disabled agent: %d %q", rec.Code, p.Code)
	}
}

func TestMandates_AreScopedToTheMerchant(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)
	r := f.requestMandate(t, w, "mreq_1")
	var approved struct {
		Mandate mandateBody `json:"mandate"`
	}
	json.Unmarshal(f.do(t, http.MethodPost, "/v1/consent/approve", fmt.Sprintf(`{"token":%q,"terms_hash":%q}`, f.consentToken(t), r.Terms.Hash)).Body.Bytes(), &approved)
	otherKey, _ := f.merchantWithCustomer(t)
	for _, req := range []struct{ method, path string }{
		{http.MethodGet, "/v1/commerce/mandate-requests/" + r.ID},
		{http.MethodGet, "/v1/commerce/mandates/" + approved.Mandate.ID},
		{http.MethodPost, "/v1/commerce/mandates/" + approved.Mandate.ID + "/revoke"},
	} {
		if rec := f.doBearer(t, req.method, req.path, "", otherKey); rec.Code != http.StatusNotFound {
			t.Errorf("another merchant %s %s: %d, want 404", req.method, req.path, rec.Code)
		}
	}
	var list struct {
		Data []mandateBody `json:"data"`
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/mandates", "", otherKey).Body.Bytes(), &list)
	if len(list.Data) != 0 {
		t.Errorf("another merchant lists %d mandates", len(list.Data))
	}
	if rec := f.doBearer(t, http.MethodGet, "/v1/commerce/mandates?status=pending", "", w.key); rec.Code != http.StatusBadRequest {
		t.Errorf("bad status filter: %d", rec.Code)
	}
}
