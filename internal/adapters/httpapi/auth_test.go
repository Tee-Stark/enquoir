package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
)

type authFixture struct {
	router http.Handler
	clock  *apptest.Clock
	agents *apptest.Agents
}

func newAuthFixture(t *testing.T, secureCookies bool) *authFixture {
	t.Helper()
	clock := apptest.NewClock()
	accounts, sessions, hasher := apptest.NewAccounts(), apptest.NewSessions(), &apptest.Hasher{}
	agents, ids, secrets := apptest.NewAgents(), &apptest.IDs{}, &apptest.Secrets{}
	issuer := &app.SessionIssuer{Sessions: sessions, Secrets: secrets, Clock: clock, TTL: 7 * 24 * time.Hour}
	h := &Handler{
		SignUp:         &app.SignUp{Accounts: accounts, Hasher: hasher, IDs: ids, Clock: clock, Issuer: issuer},
		LogIn:          &app.LogIn{Accounts: accounts, Hasher: hasher, Issuer: issuer},
		LogOut:         &app.LogOut{Sessions: sessions, Clock: clock},
		ResolveSession: &app.ResolveSession{Sessions: sessions, Accounts: accounts, Clock: clock},
		Cookies:        CookieSettings{Secure: secureCookies},

		CreateAgent:       &app.CreateAgent{Agents: agents, IDs: ids, Secrets: secrets, Clock: clock},
		ListAgents:        &app.ListAgents{Agents: agents},
		GetAgent:          &app.GetAgent{Agents: agents},
		RotateAgentKey:    &app.RotateAgentKey{Agents: agents, IDs: ids, Secrets: secrets, Clock: clock},
		RevokeAgent:       &app.RevokeAgent{Agents: agents, Clock: clock},
		AuthenticateAgent: &app.AuthenticateAgent{Agents: agents, Clock: clock},
	}
	return &authFixture{router: contract(t, NewRouter(h)), clock: clock, agents: agents}
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
