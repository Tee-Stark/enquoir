package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/commerce/commerceapp/commercetest"
	"enquoir/internal/domain"
)

type authFixture struct {
	router   http.Handler
	handler  *Handler
	clock    *apptest.Clock
	agents   *apptest.Agents
	events   *apptest.EventLog
	starter  *apptest.PaymentStarter
	payees   *apptest.Beneficiaries
	attempts *apptest.Attempts
	payments *apptest.Payments
	accounts *apptest.Accounts
	mailer   *apptest.Mailer
}

func newAuthFixture(t *testing.T, secureCookies bool) *authFixture {
	t.Helper()
	clock := apptest.NewClock()
	accounts, sessions, hasher := apptest.NewAccounts(), apptest.NewSessions(), &apptest.Hasher{}
	agents, ids, secrets := apptest.NewAgents(), &apptest.IDs{}, &apptest.Secrets{}
	events, starter, payees := apptest.NewEventLog(), &apptest.PaymentStarter{}, apptest.NewBeneficiaries()
	attempts, payments := apptest.NewAttempts(), apptest.NewPayments()
	events.Clock = clock
	analytics := &fixtureAnalytics{}
	issuer := &app.SessionIssuer{Sessions: sessions, IDs: ids, Secrets: secrets, Clock: clock, TTL: 7 * 24 * time.Hour}
	mailer, outbox, tokens := &apptest.Mailer{}, apptest.NewOutbox(), apptest.NewTokens()
	deliver := &app.DeliverEmail{Outbox: outbox, Tokens: tokens, Renderer: apptest.Renderer{}, Mailer: mailer,
		Secrets: secrets, Clock: clock, AppBaseURL: "https://enquoir.etimi.me"}
	audit := &app.Audit{Log: apptest.NewAuditLog(), IDs: ids, Clock: clock}
	invitations := apptest.NewInvitations()
	invitations.Accounts = accounts
	invitations.Tokens = tokens
	emails := &app.Emails{Outbox: outbox, Dispatcher: &apptest.Dispatcher{Deliver: deliver.Run}, IDs: ids, Clock: clock}
	h := &Handler{
		SignUp:         &app.SignUp{Accounts: accounts, Hasher: hasher, IDs: ids, Clock: clock, Issuer: issuer, Emails: emails},
		VerifyEmail:    &app.VerifyEmail{Tokens: tokens, Users: accounts, Clock: clock, Audit: audit},
		ResendVerify:   &app.ResendVerification{Emails: emails},
		ForgotPassword: &app.ForgotPassword{Accounts: accounts, Emails: emails},
		ResetPassword:  &app.ResetPassword{Tokens: tokens, Users: accounts, Accounts: accounts, Sessions: sessions, Hasher: hasher, Emails: emails, Clock: clock},
		ChangePassword: &app.ChangePassword{Users: accounts, Accounts: accounts, Sessions: sessions, Hasher: hasher, Emails: emails, Clock: clock},

		ListSessions:        &app.ListSessions{Sessions: sessions, Clock: clock},
		RevokeSession:       &app.RevokeSession{Sessions: sessions, Clock: clock},
		RevokeOtherSessions: &app.RevokeOtherSessions{Sessions: sessions, Clock: clock},

		ListAuditLog:   &app.ListAuditLog{Log: audit.Log},
		ListMyActivity: &app.ListMyActivity{Log: audit.Log},

		ListMembers:      &app.ListMembers{Members: accounts},
		ChangeMemberRole: &app.ChangeMemberRole{Members: accounts, Sessions: sessions, Clock: clock},
		RemoveMember:     &app.RemoveMember{Members: accounts, Sessions: sessions, Clock: clock},
		InviteMember:     &app.InviteMember{Accounts: accounts, Members: accounts, Invitations: invitations, Emails: emails, IDs: ids, Clock: clock, Audit: audit},
		ListInvitations:  &app.ListInvitations{Invitations: invitations, Clock: clock},
		RevokeInvitation: &app.RevokeInvitation{Invitations: invitations, Clock: clock},
		AcceptInvitation: &app.AcceptInvitation{Tokens: tokens, Invitations: invitations, Accounts: accounts, Sessions: sessions, Hasher: hasher, IDs: ids, Issuer: issuer, Clock: clock},
		LogIn:            &app.LogIn{Accounts: accounts, Hasher: hasher, Issuer: issuer},
		LogOut:           &app.LogOut{Sessions: sessions, Clock: clock},
		ResolveSession:   &app.ResolveSession{Sessions: sessions, Accounts: accounts, Clock: clock},
		SwitchOrg:        &app.SwitchOrganisation{Accounts: accounts, Sessions: sessions, Issuer: issuer},
		MyOrgs:           &app.ListMyOrganisations{Accounts: accounts},
		Cookies:          CookieSettings{Secure: secureCookies},

		CreateAgent:       &app.CreateAgent{Agents: agents, IDs: ids, Secrets: secrets, Clock: clock, Audit: audit},
		ListAgents:        &app.ListAgents{Agents: agents},
		GetAgent:          &app.GetAgent{Agents: agents},
		RotateAgentKey:    &app.RotateAgentKey{Agents: agents, IDs: ids, Secrets: secrets, Clock: clock},
		RevokeAgent:       &app.RevokeAgent{Agents: agents, Clock: clock},
		AuthenticateAgent: &app.AuthenticateAgent{Agents: agents, Clock: clock},

		Submit: &app.SubmitIntent{Treasuries: accounts, Starter: starter, Events: events, Payees: payees, Intents: apptest.NewIntents(), Attempts: attempts, IDs: ids, Clock: clock},
		Get:    &app.GetIntent{Events: events},

		ListPayments:      &app.ListPayments{Payments: payments},
		GetPayment:        &app.GetPayment{Payments: payments, Events: events, Attempts: attempts},
		ListAttempts:      &app.ListAttempts{Attempts: attempts},
		ListBeneficiaries: &app.ListBeneficiaries{Beneficiaries: payees},
		Summary:           &app.GetSummary{Analytics: analytics, Clock: clock},
		TimeSeries:        &app.GetTimeSeries{Analytics: analytics, Clock: clock},
		ReplayPayment:     &app.ReplayPayment{Get: &app.GetPayment{Payments: payments, Events: events, Attempts: attempts}},
	}
	integrations, customers, commerceAgents := commercetest.NewIntegrations(), commercetest.NewCustomers(), commercetest.NewAgents()
	h.Commerce = CommerceUseCases{
		CreateIntegration:    &commerceapp.CreateIntegration{Integrations: integrations, IDs: ids, Secrets: secrets, Clock: clock, Audit: audit},
		ListIntegrations:     &commerceapp.ListIntegrations{Integrations: integrations},
		GetIntegration:       &commerceapp.GetIntegration{Integrations: integrations},
		SetIntegrationStatus: &commerceapp.SetIntegrationStatus{Integrations: integrations, Clock: clock, Audit: audit},
		RotateIntegrationKey: &commerceapp.RotateIntegrationKey{Integrations: integrations, IDs: ids, Secrets: secrets, Clock: clock, Audit: audit},
		RevokeIntegrationKey: &commerceapp.RevokeIntegrationKey{Integrations: integrations, Clock: clock, Audit: audit},
		AuthenticateMerchant: &commerceapp.AuthenticateMerchant{Integrations: integrations, Clock: clock},
		RegisterCustomer:     &commerceapp.RegisterCustomer{Customers: customers, IDs: ids, Clock: clock, Audit: audit},
		ListCustomers:        &commerceapp.ListCustomers{Customers: customers},
		GetCustomer:          &commerceapp.GetCustomer{Customers: customers},
		SetCustomerStatus:    &commerceapp.SetCustomerStatus{Customers: customers, Clock: clock, Audit: audit},
		RegisterAgent:        &commerceapp.RegisterAgent{Agents: commerceAgents, IDs: ids, Clock: clock, Audit: audit},
		ListAgents:           &commerceapp.ListAgents{Agents: commerceAgents},
		GetAgent:             &commerceapp.GetAgent{Agents: commerceAgents},
		SetAgentStatus:       &commerceapp.SetAgentStatus{Agents: commerceAgents, Clock: clock, Audit: audit},
	}
	methods := commercetest.NewFundingMethods()
	h.Funding = FundingUseCases{
		Register: &commerceapp.RegisterFundingMethod{Methods: methods, Customers: customers, IDs: ids, Clock: clock, Audit: audit},
		Get:      &commerceapp.GetFundingMethod{Methods: methods},
		List:     &commerceapp.ListFundingMethods{Methods: methods, Customers: customers},
		Status:   &commerceapp.SetFundingMethodStatus{Methods: methods, Clock: clock, Audit: audit},
	}
	mandates := commercetest.NewMandates(tokens, customers, commerceAgents, methods)
	h.Mandates = MandateUseCases{
		Request:    &commerceapp.RequestMandate{Mandates: mandates, Customers: customers, Agents: commerceAgents, Methods: methods, Emails: emails, IDs: ids, Clock: clock, Audit: audit},
		GetRequest: &commerceapp.GetMandateRequest{Mandates: mandates, Clock: clock},
		View:       &commerceapp.ViewConsent{Tokens: tokens, Mandates: mandates, Clock: clock},
		Decide:     &commerceapp.DecideConsent{Tokens: tokens, Mandates: mandates, IDs: ids, Clock: clock, Audit: audit},
		List:       &commerceapp.ListMandates{Mandates: mandates, Clock: clock},
		Get:        &commerceapp.GetMandate{Mandates: mandates, Clock: clock},
		Revoke:     &commerceapp.RequestMandateRevocation{Mandates: mandates, Customers: customers, Emails: emails, Clock: clock, Audit: audit},
		ViewRevoke: &commerceapp.ViewRevocation{Tokens: tokens, Mandates: mandates, Clock: clock},
		Confirm:    &commerceapp.ConfirmRevocation{Tokens: tokens, Mandates: mandates, Clock: clock, Audit: audit},
	}
	checkouts := commercetest.NewCheckouts(mandates, tokens)
	h.Checkouts = CheckoutUseCases{
		Submit: &commerceapp.SubmitCheckout{Checkouts: checkouts, Customers: customers, Emails: emails, IDs: ids, Secrets: secrets, Clock: clock, Audit: audit},
		Get:    &commerceapp.GetCheckout{Checkouts: checkouts, Clock: clock},
		Cancel: &commerceapp.CancelCheckout{Checkouts: checkouts, Clock: clock, Audit: audit},
		View:   &commerceapp.ViewCheckoutApproval{Tokens: tokens, Checkouts: checkouts, Clock: clock},
		Decide: &commerceapp.DecideCheckoutApproval{Tokens: tokens, Checkouts: checkouts, IDs: ids, Clock: clock, Audit: audit},
	}
	h.Mandates.Get.Usage, h.Mandates.List.Usage = checkouts, checkouts
	h.Executions = ExecutionUseCases{
		Report:        &commerceapp.ReportExecutionOutcome{Checkouts: checkouts, Clock: clock, Audit: audit},
		List:          &commerceapp.ListExecutions{Checkouts: checkouts},
		ListCheckouts: &commerceapp.ListCheckouts{Checkouts: checkouts, Clock: clock},
		Dashboard:     &commerceapp.MerchantForDashboard{Integrations: integrations},
	}
	h.Grants = GrantUseCases{
		Issue:  &commerceapp.IssueGrant{Checkouts: checkouts, Secrets: secrets, Clock: clock, Audit: audit},
		Redeem: &commerceapp.RedeemGrant{Checkouts: checkouts, IDs: ids, Clock: clock, Audit: audit},
		Get:    &commerceapp.GetExecution{Checkouts: checkouts},
	}
	return &authFixture{router: contract(t, NewRouter(h)), handler: h, clock: clock, agents: agents, events: events, starter: starter, payees: payees, attempts: attempts, payments: payments, accounts: accounts, mailer: mailer}
}

var testEmailCounter atomic.Int64

// newAgent signs up a fresh organisation, creates an agent for it, and
// returns the organisation's ID and the agent's bearer token - a
// realistic way to get something requireAgent will accept, since an
// agent always belongs to a real, signed-up organisation.
func (f *authFixture) newAgent(t *testing.T) (domain.OrgID, string) {
	t.Helper()
	email := fmt.Sprintf("agent-owner-%d@example.com", testEmailCounter.Add(1))
	body := fmt.Sprintf(`{"email":%q,"password":"correct horse battery","organisation_name":"Org of %s"}`, email, email)
	rec := f.do(t, http.MethodPost, "/v1/auth/signup", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("signing up: %d %s", rec.Code, rec.Body)
	}
	var signed authBody
	if err := json.Unmarshal(rec.Body.Bytes(), &signed); err != nil {
		t.Fatalf("decoding signup response: %v", err)
	}
	f.verify(t, email)
	issued := f.createAgent(t, sessionCookie(t, rec), "Test Agent")
	org := domain.OrgID(signed.Organisation.ID)
	// By convention every test organisation owns a payee "payee-1".
	f.payees.Own(org, "payee-1")
	return org, issued.APIKey.Secret
}

func (f *authFixture) do(t *testing.T, method, path, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie set; headers %v", SessionCookieName, rec.Header())
	return nil
}

const signUpBody = `{"email":"Ada@Example.com","password":"correct horse battery","organisation_name":"Acme Treasury"}`

type authBody struct {
	User struct {
		ID            string `json:"id"`
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
	} `json:"user"`
	Organisation struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"organisation"`
}

func TestSignUp_CreatesAccountAndSetsASessionCookie(t *testing.T) {
	f := newAuthFixture(t, true)
	rec := f.do(t, http.MethodPost, "/v1/auth/signup", signUpBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body)
	}
	var body authBody
	json.Unmarshal(rec.Body.Bytes(), &body)
	if body.User.Email != "ada@example.com" || body.User.EmailVerified || body.Organisation.Name != "Acme Treasury" {
		t.Errorf("body = %+v", body)
	}

	c := sessionCookie(t, rec)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" {
		t.Errorf("cookie = %+v, want HttpOnly, Secure, SameSite=Lax, Path=/", c)
	}
	if c.MaxAge != int((7 * 24 * time.Hour).Seconds()) {
		t.Errorf("cookie Max-Age = %d, want the session TTL", c.MaxAge)
	}
}

// Plain-HTTP local development needs a non-Secure cookie, or browsers
// and curl drop it.
func TestSignUp_CookieSecureFlagFollowsSettings(t *testing.T) {
	rec := newAuthFixture(t, false).do(t, http.MethodPost, "/v1/auth/signup", signUpBody)
	if sessionCookie(t, rec).Secure {
		t.Error("cookie is Secure although settings say otherwise")
	}
}

func TestSignUp_Errors(t *testing.T) {
	f := newAuthFixture(t, true)
	f.do(t, http.MethodPost, "/v1/auth/signup", signUpBody)

	cases := []struct {
		name, body string
		status     int
		code       string
	}{
		{"email taken", signUpBody, 409, "email_taken"},
		{"short password", `{"email":"b@example.com","password":"short","organisation_name":"B"}`, 400, "validation_failed"},
		{"malformed", `{`, 400, "invalid_json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(t, http.MethodPost, "/v1/auth/signup", tc.body)
			if p := decodeProblem(t, rec); rec.Code != tc.status || p.Code != tc.code {
				t.Errorf("got %d %q, want %d %q", rec.Code, p.Code, tc.status, tc.code)
			}
		})
	}
}

func TestSignUp_ValidationNamesTheField(t *testing.T) {
	rec := newAuthFixture(t, true).do(t, http.MethodPost, "/v1/auth/signup",
		`{"email":"b@example.com","password":"short","organisation_name":"B"}`)
	var p struct {
		Field string `json:"field"`
	}
	json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Field != "password" {
		t.Errorf("field = %q, want password", p.Field)
	}
}

func TestLogIn_SessionAndLogOut(t *testing.T) {
	f := newAuthFixture(t, true)
	f.do(t, http.MethodPost, "/v1/auth/signup", signUpBody)

	bad := f.do(t, http.MethodPost, "/v1/auth/login", `{"email":"ada@example.com","password":"wrong wrong wrong"}`)
	if p := decodeProblem(t, bad); bad.Code != 401 || p.Code != "invalid_credentials" {
		t.Errorf("wrong password: %d %q, want 401 invalid_credentials", bad.Code, p.Code)
	}

	login := f.do(t, http.MethodPost, "/v1/auth/login", `{"email":"ada@example.com","password":"correct horse battery"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login status = %d; body %s", login.Code, login.Body)
	}
	cookie := sessionCookie(t, login)

	if rec := f.do(t, http.MethodGet, "/v1/auth/session", "", cookie); rec.Code != http.StatusOK {
		t.Errorf("GET /v1/auth/session with cookie: %d, want 200", rec.Code)
	}
	if rec := f.do(t, http.MethodGet, "/v1/auth/session", ""); rec.Code != http.StatusUnauthorized || decodeProblem(t, rec).Code != "unauthenticated" {
		t.Errorf("GET /v1/auth/session without cookie: %d, want 401 unauthenticated", rec.Code)
	}

	out := f.do(t, http.MethodPost, "/v1/auth/logout", "", cookie)
	if out.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", out.Code)
	}
	if cleared := sessionCookie(t, out); cleared.MaxAge >= 0 || cleared.Value != "" {
		t.Errorf("logout cookie = %+v, want it cleared", cleared)
	}
	if rec := f.do(t, http.MethodGet, "/v1/auth/session", "", cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("session after logout: %d, want 401", rec.Code)
	}
}

func TestSession_ExpiresAfterItsTTL(t *testing.T) {
	f := newAuthFixture(t, true)
	cookie := sessionCookie(t, f.do(t, http.MethodPost, "/v1/auth/signup", signUpBody))
	f.clock.Advance(7*24*time.Hour + time.Second)
	if rec := f.do(t, http.MethodGet, "/v1/auth/session", "", cookie); rec.Code != http.StatusUnauthorized {
		t.Errorf("expired session: %d, want 401", rec.Code)
	}
}

// verify follows the verification link last emailed to email.
func (f *authFixture) verify(t *testing.T, email string) {
	t.Helper()
	token := f.mailer.TokenTo(domain.Email(email))
	if rec := f.do(t, http.MethodPost, "/v1/auth/verify-email", fmt.Sprintf(`{"token":%q}`, token)); rec.Code != http.StatusNoContent {
		t.Fatalf("verifying %s: %d %s", email, rec.Code, rec.Body)
	}
}
