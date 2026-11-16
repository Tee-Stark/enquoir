package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

// doBearer sends a request with an Authorization: Bearer credential,
// and any cookies.
func (f *authFixture) doBearer(t *testing.T, method, path, body, bearer string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

type integrationBody struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	DisabledAt *string `json:"disabled_at"`
	Key        *struct {
		ID         string  `json:"id"`
		Prefix     string  `json:"prefix"`
		LastUsedAt *string `json:"last_used_at"`
	} `json:"key"`
}

type issuedIntegrationBody struct {
	Integration integrationBody `json:"integration"`
	Key         struct {
		ID     string `json:"id"`
		Prefix string `json:"prefix"`
		Secret string `json:"secret"`
	} `json:"key"`
}

func (f *authFixture) createIntegration(t *testing.T, cookie *http.Cookie, name string) issuedIntegrationBody {
	t.Helper()
	rec := f.do(t, http.MethodPost, "/v1/merchant-integrations", fmt.Sprintf(`{"name":%q}`, name), cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/merchant-integrations: %d %s", rec.Code, rec.Body)
	}
	var b issuedIntegrationBody
	json.Unmarshal(rec.Body.Bytes(), &b)
	return b
}

// newMerchant signs up an organisation, connects a merchant and returns
// the organisation, the owner's session and the integration key.
func (f *authFixture) newMerchant(t *testing.T) (domain.OrgID, *http.Cookie, issuedIntegrationBody) {
	t.Helper()
	org, owner := f.newOrg(t)
	return org, owner, f.createIntegration(t, owner, "Acme Electronics")
}

type customerBody struct {
	ID         string  `json:"id"`
	ExternalID string  `json:"external_id"`
	Email      string  `json:"email"`
	Status     string  `json:"status"`
	DisabledAt *string `json:"disabled_at"`
}

type commerceAgentBody struct {
	ID         string `json:"id"`
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
}

func TestMerchantIntegrations_CreateShowsTheKeyOnceThenOnlyItsPrefix(t *testing.T) {
	f := newAuthFixture(t, true)
	_, owner, issued := f.newMerchant(t)
	if !strings.HasPrefix(issued.Key.Secret, "enq_mk_") || issued.Integration.Status != "active" || issued.Integration.Name != "Acme Electronics" {
		t.Fatalf("issued = %+v", issued)
	}
	rec := f.do(t, http.MethodGet, "/v1/merchant-integrations/"+issued.Integration.ID, "", owner)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), issued.Key.Secret) {
		t.Fatalf("GET integration: %d, and the secret must never be shown again: %s", rec.Code, rec.Body)
	}
	var got integrationBody
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Key == nil || got.Key.Prefix != issued.Key.Secret[:12] {
		t.Errorf("key = %+v, want the prefix %q", got.Key, issued.Key.Secret[:12])
	}
	var list struct {
		Data []integrationBody `json:"data"`
	}
	json.Unmarshal(f.do(t, http.MethodGet, "/v1/merchant-integrations", "", owner).Body.Bytes(), &list)
	if len(list.Data) != 1 || list.Data[0].ID != issued.Integration.ID {
		t.Errorf("list = %+v", list.Data)
	}
}

// Connecting merchants is for owners and admins with a verified email;
// any member may look.
func TestMerchantIntegrations_RolesAndVerification(t *testing.T) {
	f := newAuthFixture(t, true)
	org, owner, issued := f.newMerchant(t)
	id := issued.Integration.ID
	writes := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/merchant-integrations", `{"name":"x"}`},
		{http.MethodPost, "/v1/merchant-integrations/" + id + "/disable", ""},
		{http.MethodPost, "/v1/merchant-integrations/" + id + "/enable", ""},
		{http.MethodPost, "/v1/merchant-integrations/" + id + "/rotate-key", ""},
		{http.MethodDelete, "/v1/merchant-integrations/" + id + "/key", ""},
	}
	for _, role := range []domain.Role{domain.Viewer, domain.Operator} {
		member := f.memberAs(t, org, role)
		for _, w := range writes {
			rec := f.do(t, w.method, w.path, w.body, member)
			if p := decodeProblem(t, rec); rec.Code != http.StatusForbidden || p.Code != "forbidden" {
				t.Errorf("%s %s %s: %d %q, want 403 forbidden", role, w.method, w.path, rec.Code, p.Code)
			}
		}
		for _, path := range []string{"/v1/merchant-integrations", "/v1/merchant-integrations/" + id} {
			if rec := f.do(t, http.MethodGet, path, "", member); rec.Code != http.StatusOK {
				t.Errorf("%s GET %s: %d, want 200", role, path, rec.Code)
			}
		}
	}
	admin := f.memberAs(t, org, domain.Admin)
	if rec := f.do(t, http.MethodPost, "/v1/merchant-integrations", `{"name":"Second shop"}`, admin); rec.Code != http.StatusCreated {
		t.Errorf("admin creating an integration: %d %s", rec.Code, rec.Body)
	}

	// An unverified owner may not issue or re-enable credentials.
	unverified := sessionCookie(t, f.do(t, http.MethodPost, "/v1/auth/signup",
		`{"email":"fresh@example.com","password":"correct horse battery","organisation_name":"Fresh"}`))
	rec := f.do(t, http.MethodPost, "/v1/merchant-integrations", `{"name":"x"}`, unverified)
	if p := decodeProblem(t, rec); rec.Code != http.StatusForbidden || p.Code != "email_unverified" {
		t.Errorf("unverified owner creating an integration: %d %q", rec.Code, p.Code)
	}
	_ = owner
}

func TestMerchantIntegrations_AreScopedToTheOrganisation(t *testing.T) {
	f := newAuthFixture(t, true)
	_, _, issued := f.newMerchant(t)
	_, other := f.newOrg(t)
	id := issued.Integration.ID
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/v1/merchant-integrations/" + id},
		{http.MethodPost, "/v1/merchant-integrations/" + id + "/disable"},
		{http.MethodPost, "/v1/merchant-integrations/" + id + "/rotate-key"},
		{http.MethodDelete, "/v1/merchant-integrations/" + id + "/key"},
	} {
		if rec := f.do(t, r.method, r.path, "", other); rec.Code != http.StatusNotFound {
			t.Errorf("another org %s %s: %d, want 404", r.method, r.path, rec.Code)
		}
	}
	if rec := f.doBearer(t, http.MethodGet, "/v1/commerce/customers", "", issued.Key.Secret); rec.Code != http.StatusOK {
		t.Errorf("the key after another org's attempts: %d, want it still working", rec.Code)
	}
}

// Disabling, rotating and revoking each end the key's access at once.
func TestMerchantIntegrations_DisableRotateAndRevokeTheKey(t *testing.T) {
	f := newAuthFixture(t, true)
	_, owner, issued := f.newMerchant(t)
	base := "/v1/merchant-integrations/" + issued.Integration.ID
	key := issued.Key.Secret
	works := func(secret string) int {
		return f.doBearer(t, http.MethodGet, "/v1/commerce/customers", "", secret).Code
	}

	rec := f.do(t, http.MethodPost, base+"/disable", "", owner)
	var got integrationBody
	json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Status != "disabled" || got.DisabledAt == nil {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if code := works(key); code != http.StatusUnauthorized {
		t.Errorf("disabled integration's key: %d, want 401", code)
	}
	if rec := f.do(t, http.MethodPost, base+"/enable", "", owner); rec.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
	if code := works(key); code != http.StatusOK {
		t.Errorf("re-enabled integration's key: %d, want 200", code)
	}

	rec = f.do(t, http.MethodPost, base+"/rotate-key", "", owner)
	var rotated issuedIntegrationBody
	json.Unmarshal(rec.Body.Bytes(), &rotated)
	if rec.Code != http.StatusOK || rotated.Key.Secret == "" || rotated.Key.Secret == key {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	if works(key) != http.StatusUnauthorized || works(rotated.Key.Secret) != http.StatusOK {
		t.Error("after rotation the old key must fail and the new one work")
	}

	if rec := f.do(t, http.MethodDelete, base+"/key", "", owner); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke key: %d %s", rec.Code, rec.Body)
	}
	if code := works(rotated.Key.Secret); code != http.StatusUnauthorized {
		t.Errorf("revoked key: %d, want 401", code)
	}
	json.Unmarshal(f.do(t, http.MethodGet, base, "", owner).Body.Bytes(), &got)
	if got.Key != nil || got.Status != "active" {
		t.Errorf("after revoking: %+v, want an active integration with no key", got)
	}
}

// Treasury and commerce credentials are never interchangeable, and a
// dashboard session is not a merchant credential.
func TestCommerce_OnlyAMerchantKeyIsAccepted(t *testing.T) {
	f := newAuthFixture(t, true)
	_, owner, issued := f.newMerchant(t)
	_, agentKey := f.newAgent(t)

	for name, try := range map[string]func() *httptest.ResponseRecorder{
		"no credential": func() *httptest.ResponseRecorder {
			return f.doBearer(t, http.MethodGet, "/v1/commerce/customers", "", "")
		},
		"a treasury key": func() *httptest.ResponseRecorder {
			return f.doBearer(t, http.MethodGet, "/v1/commerce/customers", "", agentKey)
		},
		"a dashboard session": func() *httptest.ResponseRecorder {
			return f.doBearer(t, http.MethodGet, "/v1/commerce/customers", "", "", owner)
		},
		"a made-up key": func() *httptest.ResponseRecorder {
			return f.doBearer(t, http.MethodGet, "/v1/commerce/agents", "", "enq_mk_made_up")
		},
	} {
		if rec := try(); rec.Code != http.StatusUnauthorized {
			t.Errorf("commerce with %s: %d, want 401", name, rec.Code)
		}
	}
	// And the merchant key opens no treasury route.
	if rec := postIntentAs(t, f, issued.Key.Secret, validIntentJSON); rec.Code != http.StatusUnauthorized {
		t.Errorf("a merchant key submitting a treasury intent: %d, want 401", rec.Code)
	}
	if rec := f.doBearer(t, http.MethodPost, "/v1/beneficiaries", `{}`, issued.Key.Secret); rec.Code != http.StatusUnauthorized {
		t.Errorf("a merchant key registering a payee: %d, want 401", rec.Code)
	}
	if rec := f.doBearer(t, http.MethodGet, "/v1/merchant-integrations", "", issued.Key.Secret); rec.Code != http.StatusUnauthorized {
		t.Errorf("a merchant key on the dashboard: %d, want 401", rec.Code)
	}
}

func TestCommerceCustomers_RegisterOrResolve(t *testing.T) {
	f := newAuthFixture(t, true)
	_, _, issued := f.newMerchant(t)
	key := issued.Key.Secret

	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/customers", `{"external_id":"cust_42","email":"Ada@Example.com"}`, key)
	var first customerBody
	json.Unmarshal(rec.Body.Bytes(), &first)
	if rec.Code != http.StatusCreated || first.ExternalID != "cust_42" || first.Email != "ada@example.com" || first.Status != "active" {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/customers", `{"external_id":"cust_42","email":"ada@example.com"}`, key)
	var again customerBody
	json.Unmarshal(rec.Body.Bytes(), &again)
	if rec.Code != http.StatusOK || again.ID != first.ID {
		t.Errorf("repeat: %d %s, want 200 and the same customer", rec.Code, rec.Body)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/customers", `{"external_id":"cust_42","email":"eve@example.com"}`, key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "registration_conflict" {
		t.Errorf("different email: %d %q, want 409 registration_conflict", rec.Code, p.Code)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/customers", `{"external_id":"cust_43","email":"nope"}`, key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusBadRequest || p.Field != "email" {
		t.Errorf("bad email: %d %+v", rec.Code, p)
	}
	// The merchant comes from the key, never from the body.
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/customers", `{"external_id":"cust_44","email":"x@example.com","merchant_id":"mi_someone_else"}`, key)
	if rec.Code != http.StatusCreated {
		t.Errorf("a body naming a merchant: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Data       []customerBody `json:"data"`
		NextCursor *string        `json:"next_cursor"`
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/customers?external_id=cust_44", "", key).Body.Bytes(), &list)
	if len(list.Data) != 1 {
		t.Errorf("cust_44 under this merchant: %+v", list.Data)
	}
}

func TestCommerceCustomers_ListGetDisableAndEnable(t *testing.T) {
	f := newAuthFixture(t, true)
	_, _, issued := f.newMerchant(t)
	key := issued.Key.Secret
	var ids []string
	for i := range 3 {
		rec := f.doBearer(t, http.MethodPost, "/v1/commerce/customers", fmt.Sprintf(`{"external_id":"c%d","email":"c%d@example.com"}`, i, i), key)
		var c customerBody
		json.Unmarshal(rec.Body.Bytes(), &c)
		ids = append(ids, c.ID)
		f.clock.Advance(1)
	}
	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/customers/"+ids[1]+"/disable", "", key)
	var c customerBody
	json.Unmarshal(rec.Body.Bytes(), &c)
	if rec.Code != http.StatusOK || c.Status != "disabled" || c.DisabledAt == nil {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}

	var page struct {
		Data       []customerBody `json:"data"`
		NextCursor *string        `json:"next_cursor"`
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/customers?limit=2", "", key).Body.Bytes(), &page)
	if len(page.Data) != 2 || page.Data[0].ID != ids[2] || page.NextCursor == nil {
		t.Fatalf("first page: %+v", page)
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/customers?limit=2&cursor="+*page.NextCursor, "", key).Body.Bytes(), &page)
	if len(page.Data) != 1 || page.Data[0].ID != ids[0] || page.NextCursor != nil {
		t.Errorf("second page: %+v", page)
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/customers?status=disabled", "", key).Body.Bytes(), &page)
	if len(page.Data) != 1 || page.Data[0].ID != ids[1] {
		t.Errorf("disabled: %+v", page.Data)
	}
	for _, q := range []string{"limit=abc", "limit=500", "status=gone"} {
		if rec := f.doBearer(t, http.MethodGet, "/v1/commerce/customers?"+q, "", key); rec.Code != http.StatusBadRequest {
			t.Errorf("?%s: %d, want 400", q, rec.Code)
		}
	}

	if rec := f.doBearer(t, http.MethodPost, "/v1/commerce/customers/"+ids[1]+"/enable", "", key); rec.Code != http.StatusOK {
		t.Errorf("enable: %d %s", rec.Code, rec.Body)
	}
	rec = f.doBearer(t, http.MethodGet, "/v1/commerce/customers/"+ids[1], "", key)
	json.Unmarshal(rec.Body.Bytes(), &c)
	if rec.Code != http.StatusOK || c.Status != "active" {
		t.Errorf("get: %d %s", rec.Code, rec.Body)
	}
}

// Another merchant - another organisation's, or a sibling storefront
// of the same organisation - gets 404 for this merchant's records.
func TestCommerce_RecordsAreScopedToTheMerchant(t *testing.T) {
	f := newAuthFixture(t, true)
	_, owner, mine := f.newMerchant(t)
	sibling := f.createIntegration(t, owner, "Second storefront")
	_, _, stranger := f.newMerchant(t)

	var c customerBody
	json.Unmarshal(f.doBearer(t, http.MethodPost, "/v1/commerce/customers", `{"external_id":"cust_42","email":"ada@example.com"}`, mine.Key.Secret).Body.Bytes(), &c)
	var a commerceAgentBody
	json.Unmarshal(f.doBearer(t, http.MethodPost, "/v1/commerce/agents", `{"external_id":"assistant","name":"Assistant"}`, mine.Key.Secret).Body.Bytes(), &a)

	for _, other := range []string{sibling.Key.Secret, stranger.Key.Secret} {
		for _, r := range []struct{ method, path string }{
			{http.MethodGet, "/v1/commerce/customers/" + c.ID},
			{http.MethodPost, "/v1/commerce/customers/" + c.ID + "/disable"},
			{http.MethodGet, "/v1/commerce/agents/" + a.ID},
			{http.MethodPost, "/v1/commerce/agents/" + a.ID + "/disable"},
		} {
			if rec := f.doBearer(t, r.method, r.path, "", other); rec.Code != http.StatusNotFound {
				t.Errorf("another merchant %s %s: %d, want 404", r.method, r.path, rec.Code)
			}
		}
		// The same external ID is a separate record for them.
		rec := f.doBearer(t, http.MethodPost, "/v1/commerce/customers", `{"external_id":"cust_42","email":"ada@example.com"}`, other)
		var theirs customerBody
		json.Unmarshal(rec.Body.Bytes(), &theirs)
		if rec.Code != http.StatusCreated || theirs.ID == c.ID {
			t.Errorf("cust_42 under another merchant: %d %+v, want a new record", rec.Code, theirs)
		}
	}
}

func TestCommerceAgents_RegisterOrResolveAndDisable(t *testing.T) {
	f := newAuthFixture(t, true)
	_, _, issued := f.newMerchant(t)
	key := issued.Key.Secret

	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/agents", `{"external_id":"assistant","name":"Shopping assistant"}`, key)
	var a commerceAgentBody
	json.Unmarshal(rec.Body.Bytes(), &a)
	if rec.Code != http.StatusCreated || a.Name != "Shopping assistant" || a.Status != "active" {
		t.Fatalf("register: %d %s", rec.Code, rec.Body)
	}
	if rec := f.doBearer(t, http.MethodPost, "/v1/commerce/agents", `{"external_id":"assistant","name":"Shopping assistant"}`, key); rec.Code != http.StatusOK {
		t.Errorf("repeat: %d", rec.Code)
	}
	if rec := f.doBearer(t, http.MethodPost, "/v1/commerce/agents", `{"external_id":"assistant","name":"Renamed"}`, key); rec.Code != http.StatusConflict {
		t.Errorf("rename by re-registering: %d, want 409", rec.Code)
	}
	if rec := f.doBearer(t, http.MethodPost, "/v1/commerce/agents", `{"external_id":"has space","name":"x"}`, key); rec.Code != http.StatusBadRequest {
		t.Errorf("bad external ID: %d, want 400", rec.Code)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/agents/"+a.ID+"/disable", "", key)
	json.Unmarshal(rec.Body.Bytes(), &a)
	if rec.Code != http.StatusOK || a.Status != "disabled" {
		t.Errorf("disable: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Data []commerceAgentBody `json:"data"`
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/agents", "", key).Body.Bytes(), &list)
	if len(list.Data) != 1 || list.Data[0].Status != "disabled" {
		t.Errorf("list: %+v", list.Data)
	}
	if rec := f.doBearer(t, http.MethodPost, "/v1/commerce/agents/"+a.ID+"/enable", "", key); rec.Code != http.StatusOK {
		t.Errorf("enable: %d", rec.Code)
	}
}

// The audit log names the merchant integration that acted with its key.
func TestCommerce_AuditLogNamesTheMerchant(t *testing.T) {
	f := newAuthFixture(t, true)
	_, owner, issued := f.newMerchant(t)
	f.doBearer(t, http.MethodPost, "/v1/commerce/customers", `{"external_id":"cust_42","email":"ada@example.com"}`, issued.Key.Secret)

	rec := f.do(t, http.MethodGet, "/v1/audit-log?action="+app.AuditCommerceCustomerRegistered, "", owner)
	var body struct {
		Data []struct {
			Actor *struct {
				Type, ID string
				Name     *string
			} `json:"actor"`
			Target *struct{ Type, ID string } `json:"target"`
		} `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || len(body.Data) != 1 {
		t.Fatalf("audit log: %d %s", rec.Code, rec.Body)
	}
	e := body.Data[0]
	if e.Actor == nil || e.Actor.Type != "merchant_integration" || e.Actor.ID != issued.Integration.ID ||
		e.Actor.Name == nil || *e.Actor.Name != "Acme Electronics" || e.Target == nil || e.Target.Type != "commerce_customer" {
		t.Errorf("event = %+v actor %+v target %+v", e, e.Actor, e.Target)
	}
	if strings.Contains(rec.Body.String(), "ada@example.com") {
		t.Error("the audit log carries the customer's email")
	}
	rec = f.do(t, http.MethodGet, "/v1/audit-log?action="+app.AuditIntegrationCreated, "", owner)
	if !strings.Contains(rec.Body.String(), `"type":"merchant_integration"`) {
		t.Errorf("integration created: %s", rec.Body)
	}
}
