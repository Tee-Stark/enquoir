package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

type fundingBody struct {
	ID         string `json:"id"`
	CustomerID string `json:"customer_id"`
	Reference  string `json:"merchant_funding_method_id"`
	Provider   string `json:"provider"`
	Display    struct {
		Brand    string  `json:"brand"`
		LastFour *string `json:"last_four"`
	} `json:"display"`
	Status     string  `json:"status"`
	DisabledAt *string `json:"disabled_at"`
}

// merchantWithCustomer connects a merchant and registers one customer;
// it returns the merchant's key and the customer's ID.
func (f *authFixture) merchantWithCustomer(t *testing.T) (string, string) {
	t.Helper()
	_, _, issued := f.newMerchant(t)
	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/customers", `{"external_id":"cust_42","email":"ada@example.com"}`, issued.Key.Secret)
	var c customerBody
	json.Unmarshal(rec.Body.Bytes(), &c)
	return issued.Key.Secret, c.ID
}

func fundingJSON(customer, ref string) string {
	return fmt.Sprintf(`{"customer_id":%q,"provider":"stripe","merchant_funding_method_id":%q,"display":{"brand":"visa","last_four":"4242"}}`, customer, ref)
}

func TestFundingMethods_RegisterOrResolve(t *testing.T) {
	f := newAuthFixture(t, true)
	key, customer := f.merchantWithCustomer(t)

	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods", fundingJSON(customer, "mfm_123"), key)
	var first fundingBody
	json.Unmarshal(rec.Body.Bytes(), &first)
	if rec.Code != http.StatusCreated || !strings.HasPrefix(first.ID, "fmb_") || first.CustomerID != customer || first.Reference != "mfm_123" ||
		first.Provider != "stripe" || first.Display.Brand != "visa" || first.Display.LastFour == nil || *first.Display.LastFour != "4242" || first.Status != "active" {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods", fundingJSON(customer, "mfm_123"), key)
	var again fundingBody
	json.Unmarshal(rec.Body.Bytes(), &again)
	if rec.Code != http.StatusOK || again.ID != first.ID {
		t.Errorf("repeat: %d %s, want 200 and the same binding", rec.Code, rec.Body)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods",
		fmt.Sprintf(`{"customer_id":%q,"provider":"stripe","merchant_funding_method_id":"mfm_123","display":{"brand":"mastercard","last_four":"4242"}}`, customer), key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "registration_conflict" {
		t.Errorf("different display: %d %q, want 409", rec.Code, p.Code)
	}
	// A wallet has no last four.
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods",
		fmt.Sprintf(`{"customer_id":%q,"provider":"stripe","merchant_funding_method_id":"mfm_wallet","display":{"brand":"paypal"}}`, customer), key)
	var wallet fundingBody
	json.Unmarshal(rec.Body.Bytes(), &wallet)
	if rec.Code != http.StatusCreated || wallet.Display.LastFour != nil {
		t.Errorf("wallet: %d %s", rec.Code, rec.Body)
	}
}

// Card data is refused, not ignored - and never repeated back.
func TestFundingMethods_RefuseCardData(t *testing.T) {
	f := newAuthFixture(t, true)
	key, customer := f.merchantWithCustomer(t)
	const pan = "4111111111111111"
	cases := map[string]struct{ body, field string }{
		"a card number field":                  {fmt.Sprintf(`{"customer_id":%q,"provider":"stripe","merchant_funding_method_id":"mfm_1","display":{"brand":"visa","last_four":"1111"},"card_number":%q}`, customer, pan), "card_number"},
		"a CVV field":                          {fmt.Sprintf(`{"customer_id":%q,"provider":"stripe","merchant_funding_method_id":"mfm_1","display":{"brand":"visa","last_four":"1111"},"cvv":"123"}`, customer), "cvv"},
		"a PSP secret key field":               {fmt.Sprintf(`{"customer_id":%q,"provider":"stripe","merchant_funding_method_id":"mfm_1","psp_secret_key":"sk_test_123","display":{"brand":"visa"}}`, customer), "psp_secret_key"},
		"a card number as the PSP's reference": {fmt.Sprintf(`{"customer_id":%q,"provider":"stripe","provider_method_reference":"4111-1111-1111-1111","display":{"brand":"visa"}}`, customer), "provider_method_reference"},
		"an unknown display field":             {fmt.Sprintf(`{"customer_id":%q,"provider":"stripe","merchant_funding_method_id":"mfm_1","display":{"brand":"visa","number":%q}}`, customer, pan), "display.number"},
		"a card number as the reference":       {fmt.Sprintf(`{"customer_id":%q,"provider":"stripe","merchant_funding_method_id":"4111 1111 1111 1111","display":{"brand":"visa"}}`, customer), "merchant_funding_method_id"},
		"a card number as last four":           {fmt.Sprintf(`{"customer_id":%q,"provider":"stripe","merchant_funding_method_id":"mfm_1","display":{"brand":"visa","last_four":%q}}`, customer, pan), "display.last_four"},
	}
	for name, tc := range cases {
		rec := f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods", tc.body, key)
		p := decodeProblem(t, rec)
		if rec.Code != http.StatusBadRequest || p.Code != "validation_failed" || p.Field != tc.field {
			t.Errorf("%s: %d %+v, want 400 validation_failed on %s", name, rec.Code, p, tc.field)
		}
		if strings.Contains(rec.Body.String(), "4111") {
			t.Errorf("%s: the response repeats card digits: %s", name, rec.Body)
		}
	}
	var list struct {
		Data []fundingBody `json:"data"`
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/customers/"+customer+"/funding-methods", "", key).Body.Bytes(), &list)
	if len(list.Data) != 0 {
		t.Errorf("refused requests stored %d bindings", len(list.Data))
	}
}

func TestFundingMethods_NeedAnActiveCustomerOfThisMerchant(t *testing.T) {
	f := newAuthFixture(t, true)
	key, customer := f.merchantWithCustomer(t)
	otherKey, otherCustomer := f.merchantWithCustomer(t)
	_ = otherKey

	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods", fundingJSON(otherCustomer, "mfm_1"), key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusUnprocessableEntity || p.Code != "customer_unknown" {
		t.Errorf("another merchant's customer: %d %q, want 422 customer_unknown", rec.Code, p.Code)
	}
	f.doBearer(t, http.MethodPost, "/v1/commerce/customers/"+customer+"/disable", "", key)
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods", fundingJSON(customer, "mfm_1"), key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusUnprocessableEntity || p.Code != "customer_disabled" {
		t.Errorf("a disabled customer: %d %q, want 422 customer_disabled", rec.Code, p.Code)
	}
}

func TestFundingMethods_ListGetDisableEnableAndScope(t *testing.T) {
	f := newAuthFixture(t, true)
	key, customer := f.merchantWithCustomer(t)
	otherKey, _ := f.merchantWithCustomer(t)
	var a, b fundingBody
	json.Unmarshal(f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods", fundingJSON(customer, "mfm_a"), key).Body.Bytes(), &a)
	f.clock.Advance(1)
	json.Unmarshal(f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods", fundingJSON(customer, "mfm_b"), key).Body.Bytes(), &b)

	var page struct {
		Data       []fundingBody `json:"data"`
		NextCursor *string       `json:"next_cursor"`
	}
	rec := f.doBearer(t, http.MethodGet, "/v1/commerce/customers/"+customer+"/funding-methods?limit=1", "", key)
	json.Unmarshal(rec.Body.Bytes(), &page)
	if rec.Code != http.StatusOK || len(page.Data) != 1 || page.Data[0].ID != b.ID || page.NextCursor == nil {
		t.Fatalf("first page: %d %s", rec.Code, rec.Body)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods/"+a.ID+"/disable", "", key)
	var got fundingBody
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Status != "disabled" || got.DisabledAt == nil {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/customers/"+customer+"/funding-methods?status=disabled", "", key).Body.Bytes(), &page)
	if len(page.Data) != 1 || page.Data[0].ID != a.ID {
		t.Errorf("disabled: %+v", page.Data)
	}
	if rec := f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods/"+a.ID+"/enable", "", key); rec.Code != http.StatusOK {
		t.Errorf("enable: %d", rec.Code)
	}
	if rec := f.doBearer(t, http.MethodGet, "/v1/commerce/funding-methods/"+a.ID, "", key); rec.Code != http.StatusOK {
		t.Errorf("get: %d", rec.Code)
	}
	if rec := f.doBearer(t, http.MethodGet, "/v1/commerce/customers/"+customer+"/funding-methods?status=gone", "", key); rec.Code != http.StatusBadRequest {
		t.Errorf("bad status: %d, want 400", rec.Code)
	}

	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/v1/commerce/funding-methods/" + a.ID},
		{http.MethodPost, "/v1/commerce/funding-methods/" + a.ID + "/disable"},
		{http.MethodGet, "/v1/commerce/customers/" + customer + "/funding-methods"},
	} {
		if rec := f.doBearer(t, r.method, r.path, "", otherKey); rec.Code != http.StatusNotFound {
			t.Errorf("another merchant %s %s: %d, want 404", r.method, r.path, rec.Code)
		}
	}
	if rec := f.doBearer(t, http.MethodGet, "/v1/commerce/funding-methods/"+a.ID, "", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no key: %d, want 401", rec.Code)
	}
}

func TestFundingMethods_ByThePSPsReference(t *testing.T) {
	f := newAuthFixture(t, true)
	key, customer := f.merchantWithCustomer(t)
	body := fmt.Sprintf(`{"customer_id":%q,"provider":"stripe","provider_method_reference":"pm_1Nq2fPLk","display":{"brand":"visa","last_four":"4242"}}`, customer)
	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods", body, key)
	var got struct {
		ID        string  `json:"id"`
		Reference *string `json:"merchant_funding_method_id"`
		PSPRef    *string `json:"provider_method_reference"`
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusCreated || got.Reference != nil || got.PSPRef == nil || *got.PSPRef != "pm_1Nq2fPLk" {
		t.Fatalf("register by the PSP's reference: %d %s", rec.Code, rec.Body)
	}
	if rec := f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods", body, key); rec.Code != http.StatusOK {
		t.Errorf("repeat: %d", rec.Code)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods", fmt.Sprintf(`{"customer_id":%q,"provider":"stripe","display":{"brand":"visa"}}`, customer), key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusBadRequest || p.Field != "merchant_funding_method_id" {
		t.Errorf("no reference at all: %d %+v", rec.Code, p)
	}
}
