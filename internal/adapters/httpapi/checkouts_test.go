package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type checkoutBody struct {
	ID         string `json:"id"`
	ExternalID string `json:"external_id"`
	MandateID  string `json:"mandate_id"`
	Status     string `json:"status"`
	Hash       string `json:"checkout_hash"`
	Decision   struct {
		Allowed bool `json:"allowed"`
		Reasons []struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"reasons"`
	} `json:"decision"`
	Checkout struct {
		Currency   string `json:"currency"`
		TotalMinor int64  `json:"total_minor"`
		Items      []struct {
			ProductID string `json:"product_id"`
		} `json:"items"`
	} `json:"checkout"`
	ApprovalExpiresAt *string `json:"approval_expires_at"`
	CanceledAt        *string `json:"canceled_at"`
}

// approvedMandate requests a mandate in mode and approves it as the
// customer, returning its ID.
func (f *authFixture) approvedMandate(t *testing.T, w mandateWorld, ext, mode string) string {
	t.Helper()
	body := strings.Replace(w.requestJSON(ext, f.clock.Now().Add(30*24*time.Hour)), `"autonomous"`, fmt.Sprintf("%q", mode), 1)
	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/mandate-requests", body, w.key)
	var r mandateRequestBody
	json.Unmarshal(rec.Body.Bytes(), &r)
	rec = f.do(t, http.MethodPost, "/v1/consent/approve", fmt.Sprintf(`{"token":%q,"terms_hash":%q}`, f.consentToken(t), r.Terms.Hash))
	var out struct {
		Mandate mandateBody `json:"mandate"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out.Mandate.ID == "" {
		t.Fatalf("approving the mandate: %d %s", rec.Code, rec.Body)
	}
	return out.Mandate.ID
}

func (w mandateWorld) checkoutJSON(ext, mandate, currency string, total int64) string {
	return fmt.Sprintf(`{"external_id":%q,"mandate_id":%q,"customer_id":%q,"agent_id":%q,"funding_method_id":%q,
		"checkout":{"currency":%q,"items":[{"product_id":"laptop_001","name":"Lenovo ThinkPad","category":"electronics","quantity":1,"unit_price_minor":%d}],
		"shipping_minor":500,"tax_minor":0,"fees_minor":0,"total_minor":%d}}`, ext, mandate, w.customer, w.agent, w.card, currency, total-500, total)
}

func (f *authFixture) submitCheckout(t *testing.T, w mandateWorld, body string) (int, checkoutBody) {
	t.Helper()
	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/checkouts", body, w.key)
	var c checkoutBody
	json.Unmarshal(rec.Body.Bytes(), &c)
	return rec.Code, c
}

func TestCheckouts_AuthorizedDeniedAndCancelled(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)
	m := f.approvedMandate(t, w, "m1", "autonomous")

	code, c := f.submitCheckout(t, w, w.checkoutJSON("checkout_1", m, "USD", 135000))
	if code != http.StatusCreated || c.Status != "authorized" || !c.Decision.Allowed || len(c.Decision.Reasons) != 0 ||
		c.Checkout.TotalMinor != 135000 || !strings.HasPrefix(c.Hash, "sha256:") || c.MandateID != m {
		t.Fatalf("authorized: %d %+v", code, c)
	}
	if code, again := f.submitCheckout(t, w, w.checkoutJSON("checkout_1", m, "USD", 135000)); code != http.StatusOK || again.ID != c.ID {
		t.Errorf("repeat: %d", code)
	}
	if code, _ := f.submitCheckout(t, w, w.checkoutJSON("checkout_1", m, "USD", 135001)); code != http.StatusConflict {
		t.Errorf("changed under the same ID: %d", code)
	}

	code, d := f.submitCheckout(t, w, w.checkoutJSON("checkout_2", m, "EUR", 160000))
	if code != http.StatusCreated || d.Status != "denied" || d.Decision.Allowed {
		t.Fatalf("denied: %d %+v", code, d)
	}
	var reasons []string
	for _, r := range d.Decision.Reasons {
		reasons = append(reasons, r.Code)
	}
	if strings.Join(reasons, ",") != "currency_mismatch,over_transaction_limit" {
		t.Errorf("reasons = %v", reasons)
	}

	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/checkouts/"+c.ID+"/cancel", "", w.key)
	var cancelled checkoutBody
	json.Unmarshal(rec.Body.Bytes(), &cancelled)
	if rec.Code != http.StatusOK || cancelled.Status != "canceled" || cancelled.CanceledAt == nil {
		t.Errorf("cancel: %d %s", rec.Code, rec.Body)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/checkouts/"+d.ID+"/cancel", "", w.key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "checkout_not_cancelable" {
		t.Errorf("cancel a denied one: %d %q", rec.Code, p.Code)
	}
	if rec := f.doBearer(t, http.MethodGet, "/v1/commerce/checkouts/"+d.ID, "", w.key); rec.Code != http.StatusOK {
		t.Errorf("get: %d", rec.Code)
	}
}

func TestCheckouts_PerTransactionApprovalFromTheEmailedLink(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)
	m := f.approvedMandate(t, w, "m1", "per_transaction")

	code, c := f.submitCheckout(t, w, w.checkoutJSON("checkout_1", m, "USD", 135000))
	if code != http.StatusCreated || c.Status != "awaiting_customer_approval" || c.Decision.Allowed || c.ApprovalExpiresAt == nil {
		t.Fatalf("submit: %d %+v", code, c)
	}
	if !strings.Contains(f.mailer.LastLinkTo("ada@example.com"), "/consent/checkout?token=") {
		t.Fatalf("no approval link was emailed")
	}
	token := f.consentToken(t)
	rec := f.do(t, http.MethodPost, "/v1/consent/checkout/view", fmt.Sprintf(`{"token":%q}`, token))
	var view checkoutBody
	json.Unmarshal(rec.Body.Bytes(), &view)
	if rec.Code != http.StatusOK || view.ID != c.ID || view.Hash != c.Hash {
		t.Fatalf("view: %d %s", rec.Code, rec.Body)
	}
	rec = f.do(t, http.MethodPost, "/v1/consent/checkout/approve", fmt.Sprintf(`{"token":%q,"checkout_hash":"sha256:other"}`, token))
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "checkout_mismatch" {
		t.Errorf("approving another checkout: %d %q", rec.Code, p.Code)
	}
	rec = f.do(t, http.MethodPost, "/v1/consent/checkout/approve", fmt.Sprintf(`{"token":%q,"checkout_hash":%q}`, token, c.Hash))
	var approved checkoutBody
	json.Unmarshal(rec.Body.Bytes(), &approved)
	if rec.Code != http.StatusOK || approved.Status != "authorized" || !approved.Decision.Allowed {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body)
	}
	var got checkoutBody
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/checkouts/"+c.ID, "", w.key).Body.Bytes(), &got)
	if got.Status != "authorized" {
		t.Errorf("as the merchant sees it: %s", got.Status)
	}

	f.submitCheckout(t, w, w.checkoutJSON("checkout_2", m, "USD", 1000))
	rec = f.do(t, http.MethodPost, "/v1/consent/checkout/reject", fmt.Sprintf(`{"token":%q}`, f.consentToken(t)))
	var rejected checkoutBody
	json.Unmarshal(rec.Body.Bytes(), &rejected)
	if rec.Code != http.StatusOK || rejected.Status != "rejected" {
		t.Errorf("reject: %d %s", rec.Code, rec.Body)
	}
}

func TestCheckouts_ValidationScopeAndStrictBody(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)
	m := f.approvedMandate(t, w, "m1", "autonomous")
	other := f.newMandateWorld(t)

	cases := map[string]struct {
		body   string
		status int
		code   string
		field  string
	}{
		"another merchant's mandate":  {other.checkoutJSON("a", m, "USD", 1000), 422, "mandate_unknown", ""},
		"a total that doesn't add up": {strings.Replace(w.checkoutJSON("b", m, "USD", 1000), `"total_minor":1000`, `"total_minor":999`, 1), 400, "validation_failed", "total_minor"},
		"a card number field":         {strings.Replace(w.checkoutJSON("c", m, "USD", 1000), `"external_id"`, `"card_number":"4111111111111111","external_id"`, 1), 400, "validation_failed", "card_number"},
		"an agent-asserted price":     {strings.Replace(w.checkoutJSON("d", m, "USD", 1000), `"quantity":1`, `"quantity":1,"agent_price":1`, 1), 400, "validation_failed", "agent_price"},
		"no items":                    {strings.Replace(w.checkoutJSON("e", m, "USD", 1000), `"items":[{"product_id":"laptop_001","name":"Lenovo ThinkPad","category":"electronics","quantity":1,"unit_price_minor":500}]`, `"items":[]`, 1), 400, "validation_failed", "items"},
	}
	for name, tc := range cases {
		key := w.key
		if tc.code == "mandate_unknown" {
			key = other.key
		}
		rec := f.doBearer(t, http.MethodPost, "/v1/commerce/checkouts", tc.body, key)
		p := decodeProblem(t, rec)
		if rec.Code != tc.status || p.Code != tc.code || (tc.field != "" && p.Field != tc.field) {
			t.Errorf("%s: %d %+v, want %d %s %s", name, rec.Code, p, tc.status, tc.code, tc.field)
		}
		if strings.Contains(rec.Body.String(), "4111") {
			t.Errorf("%s: the response repeats card digits", name)
		}
	}
	_, c := f.submitCheckout(t, w, w.checkoutJSON("mine", m, "USD", 1000))
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/v1/commerce/checkouts/" + c.ID},
		{http.MethodPost, "/v1/commerce/checkouts/" + c.ID + "/cancel"},
	} {
		if rec := f.doBearer(t, r.method, r.path, "", other.key); rec.Code != http.StatusNotFound {
			t.Errorf("another merchant %s %s: %d", r.method, r.path, rec.Code)
		}
	}
	if rec := f.doBearer(t, http.MethodPost, "/v1/commerce/checkouts", w.checkoutJSON("x", m, "USD", 1000), ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: %d", rec.Code)
	}
}
