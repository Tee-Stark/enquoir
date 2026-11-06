package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// signUpAs signs up a fresh organisation and returns its session cookie.
func (f *authFixture) signUpAs(t *testing.T, email string) *http.Cookie {
	t.Helper()
	body := fmt.Sprintf(`{"email":%q,"password":"correct horse battery","organisation_name":"Org of %s"}`, email, email)
	cookie := sessionCookie(t, f.do(t, http.MethodPost, "/v1/auth/signup", body))
	f.verify(t, email)
	return cookie
}

type agentBody struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Status    string  `json:"status"`
	RevokedAt *string `json:"revoked_at"`
	APIKey    *struct {
		ID         string  `json:"id"`
		Prefix     string  `json:"prefix"`
		LastUsedAt *string `json:"last_used_at"`
	} `json:"api_key"`
}

type issuedAgentBody struct {
	Agent  agentBody `json:"agent"`
	APIKey struct {
		ID     string `json:"id"`
		Prefix string `json:"prefix"`
		Secret string `json:"secret"`
	} `json:"api_key"`
}

func (f *authFixture) createAgent(t *testing.T, cookie *http.Cookie, name string) issuedAgentBody {
	t.Helper()
	rec := f.do(t, http.MethodPost, "/v1/agents", fmt.Sprintf(`{"name":%q}`, name), cookie)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/agents: %d %s", rec.Code, rec.Body)
	}
	var b issuedAgentBody
	json.Unmarshal(rec.Body.Bytes(), &b)
	return b
}

func TestAgents_CreateShowsTheKeyOnceThenOnlyItsPrefix(t *testing.T) {
	f := newAuthFixture(t, true)
	cookie := f.signUpAs(t, "ada@example.com")

	issued := f.createAgent(t, cookie, "Invoice bot")
	if !strings.HasPrefix(issued.APIKey.Secret, "enq_sk_") || issued.Agent.Name != "Invoice bot" || issued.Agent.Status != "active" {
		t.Fatalf("issued = %+v", issued)
	}

	rec := f.do(t, http.MethodGet, "/v1/agents/"+issued.Agent.ID, "", cookie)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), issued.APIKey.Secret) {
		t.Fatalf("GET agent: %d, and the secret must never be shown again: %s", rec.Code, rec.Body)
	}
	var got agentBody
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got.APIKey == nil || got.APIKey.Prefix != issued.APIKey.Secret[:12] {
		t.Errorf("api_key = %+v, want the prefix %q", got.APIKey, issued.APIKey.Secret[:12])
	}

	var list struct {
		Data []agentBody `json:"data"`
	}
	json.Unmarshal(f.do(t, http.MethodGet, "/v1/agents", "", cookie).Body.Bytes(), &list)
	if len(list.Data) != 1 || list.Data[0].ID != issued.Agent.ID {
		t.Errorf("list = %+v", list.Data)
	}
}

func TestAgents_RotateAndRevoke(t *testing.T) {
	f := newAuthFixture(t, true)
	cookie := f.signUpAs(t, "ada@example.com")
	issued := f.createAgent(t, cookie, "bot")

	rec := f.do(t, http.MethodPost, "/v1/agents/"+issued.Agent.ID+"/rotate-key", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rec.Code, rec.Body)
	}
	var rotated issuedAgentBody
	json.Unmarshal(rec.Body.Bytes(), &rotated)
	if rotated.APIKey.Secret == "" || rotated.APIKey.Secret == issued.APIKey.Secret {
		t.Errorf("rotate returned %q, want a new secret", rotated.APIKey.Secret)
	}

	if rec := f.do(t, http.MethodDelete, "/v1/agents/"+issued.Agent.ID, "", cookie); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", rec.Code, rec.Body)
	}
	var got agentBody
	json.Unmarshal(f.do(t, http.MethodGet, "/v1/agents/"+issued.Agent.ID, "", cookie).Body.Bytes(), &got)
	if got.Status != "revoked" || got.RevokedAt == nil || got.APIKey != nil {
		t.Errorf("revoked agent = %+v", got)
	}
}

func TestAgents_NeedASessionAndAreScopedToTheOrganisation(t *testing.T) {
	f := newAuthFixture(t, true)
	ada := f.signUpAs(t, "ada@example.com")
	bob := f.signUpAs(t, "bob@example.com")
	adas := f.createAgent(t, ada, "ada-bot")

	if rec := f.do(t, http.MethodGet, "/v1/agents", ""); rec.Code != http.StatusUnauthorized {
		t.Errorf("no session: %d, want 401", rec.Code)
	}
	// An agent's own key is not a dashboard session.
	req := f.do(t, http.MethodGet, "/v1/agents", "", &http.Cookie{Name: SessionCookieName, Value: adas.APIKey.Secret})
	if req.Code != http.StatusUnauthorized {
		t.Errorf("API key as a session: %d, want 401", req.Code)
	}
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/v1/agents/" + adas.Agent.ID},
		{http.MethodPost, "/v1/agents/" + adas.Agent.ID + "/rotate-key"},
		{http.MethodDelete, "/v1/agents/" + adas.Agent.ID},
	} {
		rec := f.do(t, r.method, r.path, "", bob)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s as another org: %d, want 404", r.method, r.path, rec.Code)
		}
	}
}

func TestAgents_CreateValidatesTheName(t *testing.T) {
	f := newAuthFixture(t, true)
	cookie := f.signUpAs(t, "ada@example.com")
	rec := f.do(t, http.MethodPost, "/v1/agents", `{"name":"  "}`, cookie)
	if p := decodeProblem(t, rec); rec.Code != 400 || p.Code != "validation_failed" {
		t.Errorf("blank name: %d %q", rec.Code, p.Code)
	}
}
