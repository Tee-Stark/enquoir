package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type authorizationBody struct {
	ID          string  `json:"id"`
	Status      string  `json:"status"`
	Grant       *string `json:"grant"`
	ExpiresAt   string  `json:"expires_at"`
	ExecutionID *string `json:"execution_id"`
}

type grantCheckoutBody struct {
	checkoutBody
	Authorization *authorizationBody `json:"authorization"`
}

type executionBody struct {
	ID            string `json:"id"`
	CheckoutID    string `json:"checkout_id"`
	AmountMinor   int64  `json:"amount_minor"`
	Currency      string `json:"currency"`
	Status        string `json:"status"`
	FundingMethod struct {
		ID        string  `json:"id"`
		Reference *string `json:"merchant_funding_method_id"`
		Provider  string  `json:"provider"`
	} `json:"funding_method"`
}

func (f *authFixture) submitForGrant(t *testing.T, w mandateWorld, body string) grantCheckoutBody {
	t.Helper()
	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/checkouts", body, w.key)
	var c grantCheckoutBody
	json.Unmarshal(rec.Body.Bytes(), &c)
	return c
}

func redeemJSON(grant, ext, hash string) string {
	return fmt.Sprintf(`{"grant":%q,"external_checkout_id":%q,"checkout_hash":%q}`, grant, ext, hash)
}

func TestGrants_IssuedWithTheAuthorizationAndRedeemedOnce(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)
	m := f.approvedMandate(t, w, "m1", "autonomous")
	c := f.submitForGrant(t, w, w.checkoutJSON("checkout_1", m, "USD", 135000))
	a := c.Authorization
	if c.Status != "authorized" || a == nil || a.Grant == nil || !strings.HasPrefix(*a.Grant, "enq_gr_") || a.Status != "active" {
		t.Fatalf("checkout = %+v %+v", c, a)
	}
	if exp, _ := time.Parse(time.RFC3339, a.ExpiresAt); !exp.Equal(f.clock.Now().Add(15 * time.Minute)) {
		t.Errorf("expires_at = %s", a.ExpiresAt)
	}
	grant := *a.Grant
	var got grantCheckoutBody
	rec := f.doBearer(t, http.MethodGet, "/v1/commerce/checkouts/"+c.ID, "", w.key)
	json.Unmarshal(rec.Body.Bytes(), &got)
	if strings.Contains(rec.Body.String(), grant) || got.Authorization == nil || got.Authorization.Grant != nil {
		t.Fatalf("the grant is shown again: %s", rec.Body)
	}

	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/grants/redeem", redeemJSON(grant, "checkout_1", c.Hash), w.key)
	var e executionBody
	json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusCreated || e.AmountMinor != 135000 || e.Currency != "USD" || e.Status != "pending" ||
		e.FundingMethod.ID != w.card || e.FundingMethod.Reference == nil || *e.FundingMethod.Reference != "mfm_1" || e.CheckoutID != c.ID {
		t.Fatalf("redeem: %d %s", rec.Code, rec.Body)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/grants/redeem", redeemJSON(grant, "checkout_1", c.Hash), w.key)
	var again executionBody
	json.Unmarshal(rec.Body.Bytes(), &again)
	if rec.Code != http.StatusOK || again.ID != e.ID {
		t.Errorf("repeat: %d %s", rec.Code, rec.Body)
	}
	if rec := f.doBearer(t, http.MethodGet, "/v1/commerce/executions/"+e.ID, "", w.key); rec.Code != http.StatusOK {
		t.Errorf("get execution: %d", rec.Code)
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/checkouts/"+c.ID, "", w.key).Body.Bytes(), &got)
	if got.Status != "redeemed" || got.Authorization.Status != "redeemed" || got.Authorization.ExecutionID == nil || *got.Authorization.ExecutionID != e.ID {
		t.Errorf("checkout after redemption: %+v %+v", got.Status, got.Authorization)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/checkouts/"+c.ID+"/grant", "", w.key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "grant_redeemed" {
		t.Errorf("reissuing a redeemed grant: %d %q", rec.Code, p.Code)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/checkouts/"+c.ID+"/cancel", "", w.key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "checkout_not_cancelable" {
		t.Errorf("cancelling a redeemed checkout: %d %q", rec.Code, p.Code)
	}
}

func TestGrants_RedemptionErrors(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)
	other := f.newMandateWorld(t)
	m := f.approvedMandate(t, w, "m1", "autonomous")
	c := f.submitForGrant(t, w, w.checkoutJSON("checkout_1", m, "USD", 1000))
	grant := *c.Authorization.Grant

	for name, tc := range map[string]struct {
		key, body string
		status    int
		code      string
	}{
		"another checkout":       {w.key, redeemJSON(grant, "checkout_9", c.Hash), 409, "redemption_mismatch"},
		"another version":        {w.key, redeemJSON(grant, "checkout_1", "sha256:00"), 409, "redemption_mismatch"},
		"a made-up grant":        {w.key, redeemJSON("enq_gr_nope", "checkout_1", c.Hash), 422, "grant_invalid"},
		"another merchant's key": {other.key, redeemJSON(grant, "checkout_1", c.Hash), 422, "grant_invalid"},
		"card data alongside":    {w.key, strings.Replace(redeemJSON(grant, "checkout_1", c.Hash), `{`, `{"card_number":"4111111111111111",`, 1), 400, "validation_failed"},
	} {
		rec := f.doBearer(t, http.MethodPost, "/v1/commerce/grants/redeem", tc.body, tc.key)
		if p := decodeProblem(t, rec); rec.Code != tc.status || p.Code != tc.code {
			t.Errorf("%s: %d %q, want %d %s", name, rec.Code, p.Code, tc.status, tc.code)
		}
	}
	f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods/"+w.card+"/disable", "", w.key)
	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/grants/redeem", redeemJSON(grant, "checkout_1", c.Hash), w.key)
	if p := decodeProblem(t, rec); rec.Code != 422 || p.Code != "mandate_not_active" {
		t.Errorf("card disabled: %d %q", rec.Code, p.Code)
	}
	f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods/"+w.card+"/enable", "", w.key)
	f.clock.Advance(15 * time.Minute)
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/grants/redeem", redeemJSON(grant, "checkout_1", c.Hash), w.key)
	if p := decodeProblem(t, rec); rec.Code != 422 || p.Code != "grant_expired" {
		t.Errorf("expired: %d %q", rec.Code, p.Code)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/checkouts/"+c.ID+"/grant", "", w.key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "grant_not_issuable" {
		t.Errorf("reissuing an expired grant: %d %q", rec.Code, p.Code)
	}
}

// Per transaction: the customer approves, never sees a grant; the
// merchant has it issued.
func TestGrants_IssuedToTheMerchantAfterTheCustomerApproves(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)
	m := f.approvedMandate(t, w, "m1", "per_transaction")
	c := f.submitForGrant(t, w, w.checkoutJSON("checkout_1", m, "USD", 1000))
	if c.Authorization != nil {
		t.Fatalf("an awaiting checkout has an authorization: %+v", c.Authorization)
	}
	rec := f.do(t, http.MethodPost, "/v1/consent/checkout/approve", fmt.Sprintf(`{"token":%q,"checkout_hash":%q}`, f.consentToken(t), c.Hash))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "enq_gr_") {
		t.Fatalf("the customer's approval: %d %s", rec.Code, rec.Body)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/checkouts/"+c.ID+"/grant", "", w.key)
	var issued grantCheckoutBody
	json.Unmarshal(rec.Body.Bytes(), &issued)
	if rec.Code != http.StatusOK || issued.Authorization == nil || issued.Authorization.Grant == nil {
		t.Fatalf("issue: %d %s", rec.Code, rec.Body)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/grants/redeem", redeemJSON(*issued.Authorization.Grant, "checkout_1", c.Hash), w.key)
	if rec.Code != http.StatusCreated {
		t.Errorf("redeem: %d %s", rec.Code, rec.Body)
	}
	if rec := f.doBearer(t, http.MethodPost, "/v1/commerce/checkouts/"+c.ID+"/grant", "", f.newMandateWorld(t).key); rec.Code != http.StatusNotFound {
		t.Errorf("another merchant issuing: %d", rec.Code)
	}
}
