package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"enquoir/internal/domain"
)

func TestMembers_ListChangeRemove(t *testing.T) {
	f := newAuthFixture(t, true)
	org, owner := f.newOrg(t)
	viewer := f.memberAs(t, org, domain.Viewer)

	var list struct {
		Data []struct {
			User struct{ ID, Email string }
			Role string
		}
	}
	rec := f.do(t, http.MethodGet, "/v1/members", "", viewer)
	json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != 200 || len(list.Data) != 2 {
		t.Fatalf("members: %d %s", rec.Code, rec.Body)
	}
	var viewerID string
	for _, m := range list.Data {
		if m.Role == "viewer" {
			viewerID = m.User.ID
		}
	}

	if rec := f.do(t, http.MethodPatch, "/v1/members/"+viewerID, `{"role":"admin"}`, viewer); rec.Code != http.StatusForbidden {
		t.Errorf("a viewer changing roles: %d, want 403", rec.Code)
	}
	if rec := f.do(t, http.MethodPatch, "/v1/members/"+viewerID, `{"role":"operator"}`, owner); rec.Code != http.StatusNoContent {
		t.Fatalf("owner changing a role: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodGet, "/v1/members", "", viewer); rec.Code != http.StatusUnauthorized {
		t.Errorf("after a role change the member must sign in again: %d", rec.Code)
	}

	var me struct{ User struct{ ID string } }
	json.Unmarshal(f.do(t, http.MethodGet, "/v1/auth/session", "", owner).Body.Bytes(), &me)
	rec = f.do(t, http.MethodDelete, "/v1/members/"+me.User.ID, "", owner)
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "last_owner" {
		t.Errorf("the only owner leaving: %d %q, want 409 last_owner", rec.Code, p.Code)
	}
	if rec := f.do(t, http.MethodDelete, "/v1/members/"+viewerID, "", owner); rec.Code != http.StatusNoContent {
		t.Errorf("removing a member: %d %s", rec.Code, rec.Body)
	}
	if rec := f.do(t, http.MethodDelete, "/v1/members/"+viewerID, "", owner); rec.Code != http.StatusNotFound {
		t.Errorf("removing a non-member: %d", rec.Code)
	}
}

func TestInvitations_InviteAcceptAsANewAccount(t *testing.T) {
	f := newAuthFixture(t, true)
	org, owner := f.newOrg(t)

	rec := f.do(t, http.MethodPost, "/v1/invitations", `{"email":"bob@example.com","role":"operator"}`, owner)
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite: %d %s", rec.Code, rec.Body)
	}
	var inv struct{ ID, Email, Role string }
	json.Unmarshal(rec.Body.Bytes(), &inv)
	if inv.Email != "bob@example.com" || inv.Role != "operator" {
		t.Errorf("invitation = %s", rec.Body)
	}
	var pending struct{ Data []struct{ ID string } }
	json.Unmarshal(f.do(t, http.MethodGet, "/v1/invitations", "", owner).Body.Bytes(), &pending)
	if len(pending.Data) != 1 {
		t.Errorf("pending = %+v", pending)
	}

	token := f.mailer.TokenTo("bob@example.com")
	acc := f.do(t, http.MethodPost, "/v1/invitations/accept", fmt.Sprintf(`{"token":%q,"password":"bobs long passphrase"}`, token))
	if acc.Code != http.StatusOK {
		t.Fatalf("accept: %d %s", acc.Code, acc.Body)
	}
	var out struct {
		AccountCreated bool `json:"account_created"`
		Role           string
		Organisation   struct{ ID string }
	}
	json.Unmarshal(acc.Body.Bytes(), &out)
	if !out.AccountCreated || out.Role != "operator" || out.Organisation.ID != string(org) {
		t.Errorf("accepted = %s", acc.Body)
	}
	bob := sessionCookie(t, acc)
	if rec := f.do(t, http.MethodPost, "/v1/agents", `{"name":"bobs bot"}`, bob); rec.Code != http.StatusCreated {
		t.Errorf("an invited operator (verified by the link) creating an agent: %d %s", rec.Code, rec.Body)
	}
}

func TestInvitations_Rules(t *testing.T) {
	f := newAuthFixture(t, true)
	org, owner := f.newOrg(t)
	operator := f.memberAs(t, org, domain.Operator)

	if rec := f.do(t, http.MethodPost, "/v1/invitations", `{"email":"x@example.com","role":"viewer"}`, operator); rec.Code != http.StatusForbidden {
		t.Errorf("an operator inviting: %d, want 403", rec.Code)
	}
	rec := f.do(t, http.MethodPost, "/v1/invitations", `{"email":"x@example.com","role":"superuser"}`, owner)
	if p := decodeProblem(t, rec); rec.Code != 400 || p.Code != "validation_failed" {
		t.Errorf("an unknown role: %d %q", rec.Code, p.Code)
	}
	var inv struct{ ID string }
	json.Unmarshal(f.do(t, http.MethodPost, "/v1/invitations", `{"email":"dan@example.com","role":"viewer"}`, owner).Body.Bytes(), &inv)
	if rec := f.do(t, http.MethodDelete, "/v1/invitations/"+inv.ID, "", owner); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", rec.Code)
	}
	rec = f.do(t, http.MethodPost, "/v1/invitations/accept", fmt.Sprintf(`{"token":%q,"password":"dans long passphrase"}`, f.mailer.TokenTo("dan@example.com")))
	if p := decodeProblem(t, rec); rec.Code != 400 || p.Code != "token_invalid" {
		t.Errorf("a revoked invitation: %d %q", rec.Code, p.Code)
	}

	unverified := sessionCookie(t, f.do(t, http.MethodPost, "/v1/auth/signup", signUpBody))
	if rec := f.do(t, http.MethodPost, "/v1/invitations", `{"email":"y@example.com","role":"viewer"}`, unverified); rec.Code != http.StatusForbidden {
		t.Errorf("an unverified owner inviting: %d, want 403", rec.Code)
	}
}
