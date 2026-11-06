package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"enquoir/internal/domain"
)

// memberAs signs up a fresh person, adds them to org with role, and
// returns a session bound to org.
func (f *authFixture) memberAs(t *testing.T, org domain.OrgID, role domain.Role) *http.Cookie {
	t.Helper()
	email := fmt.Sprintf("member-%d@example.com", testEmailCounter.Add(1))
	rec := f.do(t, http.MethodPost, "/v1/auth/signup", fmt.Sprintf(`{"email":%q,"password":"correct horse battery","organisation_name":"Own"}`, email))
	var body authBody
	json.Unmarshal(rec.Body.Bytes(), &body)
	f.clock.Advance(1)
	f.accounts.AddMember(org, domain.UserID(body.User.ID), role, f.clock.Now())
	sw := f.do(t, http.MethodPost, "/v1/auth/switch-organisation", fmt.Sprintf(`{"organisation_id":%q}`, org), sessionCookie(t, rec))
	if sw.Code != http.StatusOK {
		t.Fatalf("switching to %s: %d %s", org, sw.Code, sw.Body)
	}
	return sessionCookie(t, sw)
}

func TestRoles_AreEnforcedOnTheDashboard(t *testing.T) {
	f := newAuthFixture(t, true)
	org, owner := f.newOrg(t)
	agent := f.createAgent(t, owner, "bot")

	writes := []struct{ method, path, body string }{
		{http.MethodPost, "/v1/agents", `{"name":"x"}`},
		{http.MethodPost, "/v1/agents/" + agent.Agent.ID + "/rotate-key", ""},
		{http.MethodDelete, "/v1/agents/" + agent.Agent.ID, ""},
	}
	reads := []string{"/v1/agents", "/v1/payments", "/v1/attempts", "/v1/beneficiaries", "/v1/analytics/summary"}

	viewer := f.memberAs(t, org, domain.Viewer)
	for _, w := range writes {
		rec := f.do(t, w.method, w.path, w.body, viewer)
		if p := decodeProblem(t, rec); rec.Code != http.StatusForbidden || p.Code != "forbidden" {
			t.Errorf("viewer %s %s: %d %q, want 403 forbidden", w.method, w.path, rec.Code, p.Code)
		}
	}
	for _, path := range reads {
		if rec := f.do(t, http.MethodGet, path, "", viewer); rec.Code != http.StatusOK {
			t.Errorf("viewer GET %s: %d, want 200", path, rec.Code)
		}
	}

	operator := f.memberAs(t, org, domain.Operator)
	if rec := f.do(t, http.MethodPost, "/v1/agents", `{"name":"ops bot"}`, operator); rec.Code != http.StatusCreated {
		t.Errorf("operator creating an agent: %d %s", rec.Code, rec.Body)
	}
}

func TestSession_ShowsRoleAndEveryOrganisation(t *testing.T) {
	f := newAuthFixture(t, true)
	org, _ := f.newOrg(t)
	viewer := f.memberAs(t, org, domain.Viewer)

	rec := f.do(t, http.MethodGet, "/v1/auth/session", "", viewer)
	var s struct {
		Role          string
		Organisation  struct{ ID string }
		Organisations []struct{ ID, Name, Role string }
	}
	json.Unmarshal(rec.Body.Bytes(), &s)
	if s.Role != "viewer" || s.Organisation.ID != string(org) || len(s.Organisations) != 2 {
		t.Errorf("session = %s", rec.Body)
	}
}

func TestSwitchOrganisation_OnlyToOnesYouBelongTo(t *testing.T) {
	f := newAuthFixture(t, true)
	_, cookie := f.newOrg(t)
	stranger, _ := f.newOrg(t)
	rec := f.do(t, http.MethodPost, "/v1/auth/switch-organisation", fmt.Sprintf(`{"organisation_id":%q}`, stranger), cookie)
	if p := decodeProblem(t, rec); rec.Code != http.StatusNotFound || p.Code != "not_found" {
		t.Errorf("switching to a stranger's organisation: %d %q, want 404", rec.Code, p.Code)
	}
}
