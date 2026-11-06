package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"enquoir/internal/domain"
)

func TestAuditLog_AdminsSeeTheirOrganisationsActions(t *testing.T) {
	f := newAuthFixture(t, true)
	org, owner := f.newOrg(t)
	f.createAgent(t, owner, "Invoice bot")
	f.do(t, http.MethodPost, "/v1/invitations", `{"email":"bob@example.com","role":"viewer"}`, owner)
	operator := f.memberAs(t, org, domain.Operator)
	_, otherOwner := f.newOrg(t)

	rec := f.do(t, http.MethodGet, "/v1/audit-log", "", owner)
	if rec.Code != 200 {
		t.Fatalf("audit log: %d %s", rec.Code, rec.Body)
	}
	var page struct {
		Data []struct {
			Action  string
			Actor   *struct{ ID, Email string }
			Target  *struct{ Type, ID string }
			Details map[string]string
		}
	}
	json.Unmarshal(rec.Body.Bytes(), &page)
	seen := map[string]bool{}
	for _, e := range page.Data {
		seen[e.Action] = true
		if e.Action == "agent.created" && (e.Actor == nil || e.Actor.Email == "" || e.Details["name"] != "Invoice bot") {
			t.Errorf("agent.created = %+v", e)
		}
	}
	for _, want := range []string{"agent.created", "member.invited"} {
		if !seen[want] {
			t.Errorf("no %s in %s", want, rec.Body)
		}
	}

	if rec := f.do(t, http.MethodGet, "/v1/audit-log", "", operator); rec.Code != http.StatusForbidden {
		t.Errorf("an operator: %d, want 403", rec.Code)
	}
	json.Unmarshal(f.do(t, http.MethodGet, "/v1/audit-log", "", otherOwner).Body.Bytes(), &page)
	for _, e := range page.Data {
		if e.Action == "agent.created" {
			t.Error("another organisation sees this one's events")
		}
	}
	if rec := f.do(t, http.MethodGet, "/v1/audit-log?action=delete_everything", "", owner); rec.Code != 400 {
		t.Errorf("an unknown action filter: %d", rec.Code)
	}
}
