package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestPassword_ForgotAndReset(t *testing.T) {
	f := newAuthFixture(t, true)
	old := sessionCookie(t, f.do(t, http.MethodPost, "/v1/auth/signup", signUpBody))

	for _, email := range []string{"ada@example.com", "nobody@example.com"} {
		if rec := f.do(t, http.MethodPost, "/v1/auth/password/forgot", fmt.Sprintf(`{"email":%q}`, email)); rec.Code != http.StatusAccepted {
			t.Errorf("forgot %s: %d, want 202 whether or not the account exists", email, rec.Code)
		}
	}
	token := f.mailer.TokenTo("ada@example.com")

	weak := f.do(t, http.MethodPost, "/v1/auth/password/reset", fmt.Sprintf(`{"token":%q,"password":"short"}`, token))
	if p := decodeProblem(t, weak); weak.Code != 400 || p.Code != "validation_failed" {
		t.Errorf("weak password: %d %q", weak.Code, p.Code)
	}
	if rec := f.do(t, http.MethodPost, "/v1/auth/password/reset", fmt.Sprintf(`{"token":%q,"password":"a brand new passphrase"}`, token)); rec.Code != http.StatusNoContent {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodGet, "/v1/auth/session", "", old); rec.Code != http.StatusUnauthorized {
		t.Errorf("a session from before the reset: %d, want 401", rec.Code)
	}
	if rec := f.do(t, http.MethodPost, "/v1/auth/login", `{"email":"ada@example.com","password":"a brand new passphrase"}`); rec.Code != http.StatusOK {
		t.Errorf("log in with the new password: %d", rec.Code)
	}
}

func TestPassword_Change(t *testing.T) {
	f := newAuthFixture(t, true)
	cookie := sessionCookie(t, f.do(t, http.MethodPost, "/v1/auth/signup", signUpBody))

	wrong := f.do(t, http.MethodPost, "/v1/auth/password/change", `{"current_password":"wrong wrong wrong","new_password":"a brand new passphrase"}`, cookie)
	if p := decodeProblem(t, wrong); wrong.Code != 401 || p.Code != "invalid_credentials" {
		t.Errorf("wrong current password: %d %q", wrong.Code, p.Code)
	}
	if rec := f.do(t, http.MethodPost, "/v1/auth/password/change", `{"current_password":"correct horse battery","new_password":"a brand new passphrase"}`, cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("change: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodGet, "/v1/auth/session", "", cookie); rec.Code != http.StatusOK {
		t.Errorf("the session that changed the password: %d, want it kept", rec.Code)
	}
}

func TestSessions_ListAndRevoke(t *testing.T) {
	f := newAuthFixture(t, true)
	first := sessionCookie(t, f.do(t, http.MethodPost, "/v1/auth/signup", signUpBody))
	second := sessionCookie(t, f.do(t, http.MethodPost, "/v1/auth/login", `{"email":"ada@example.com","password":"correct horse battery"}`))
	third := sessionCookie(t, f.do(t, http.MethodPost, "/v1/auth/login", `{"email":"ada@example.com","password":"correct horse battery"}`))

	var list struct {
		Data []struct {
			ID      string
			Current bool
		}
	}
	rec := f.do(t, http.MethodGet, "/v1/auth/sessions", "", first)
	json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != 200 || len(list.Data) != 3 || strings.Contains(rec.Body.String(), first.Value) {
		t.Fatalf("sessions: %d %s", rec.Code, rec.Body)
	}
	var other string
	for _, s := range list.Data {
		if !s.Current {
			other = s.ID
		}
	}
	if rec := f.do(t, http.MethodDelete, "/v1/auth/sessions/"+other, "", first); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke one: %d", rec.Code)
	}
	if rec := f.do(t, http.MethodDelete, "/v1/auth/sessions/"+other, "", first); rec.Code != http.StatusNotFound {
		t.Errorf("revoke it again: %d, want 404", rec.Code)
	}
	if rec := f.do(t, http.MethodDelete, "/v1/auth/sessions", "", first); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke others: %d", rec.Code)
	}
	for _, c := range []*http.Cookie{second, third} {
		if rec := f.do(t, http.MethodGet, "/v1/auth/session", "", c); rec.Code != http.StatusUnauthorized {
			t.Errorf("another session survived: %d", rec.Code)
		}
	}
	if rec := f.do(t, http.MethodGet, "/v1/auth/session", "", first); rec.Code != http.StatusOK {
		t.Errorf("the current session: %d", rec.Code)
	}
}
