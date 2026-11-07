package app_test

import (
	"context"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
)

func actions(log *apptest.AuditLog, org domain.OrgID) []string {
	var out []string
	for _, e := range log.All() {
		if e.OrgID == org {
			out = append(out, e.Action)
		}
	}
	return out
}

func TestAudit_RecordsAccountActionsWithTheActor(t *testing.T) {
	f := newMembersFixture(t)
	log := apptest.NewAuditLog()
	audit := &app.Audit{Log: log, IDs: &apptest.IDs{}, Clock: f.clock}
	f.invite.Audit, f.change.Audit, f.remove.Audit, f.accept.Audit = audit, audit, audit, audit
	ctx := app.WithActor(context.Background(), app.Actor{UserID: f.owner.User.ID, Email: f.owner.User.Email, IP: "203.0.113.5"})

	inv, _ := f.invite.Run(ctx, f.owner, "bob@example.com", domain.Viewer)
	acc, _ := f.accept.Run(context.Background(), f.mailer.TokenTo("bob@example.com"), "bobs long passphrase")
	f.change.Run(ctx, f.owner, acc.User.ID, domain.Operator)
	f.remove.Run(ctx, f.owner, acc.User.ID)

	got := actions(log, f.org)
	want := []string{"member.invited", "invitation.accepted", "member.role_changed", "member.removed"}
	if len(got) != len(want) {
		t.Fatalf("actions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("action %d = %s, want %s", i, got[i], want[i])
		}
	}
	all := log.All()
	if e := all[0]; e.ActorUserID != f.owner.User.ID || e.IP != "203.0.113.5" || e.TargetID != inv.ID || e.Details["role"] != "viewer" {
		t.Errorf("invite event = %+v", e)
	}
	if e := all[2]; e.Details["from"] != "viewer" || e.Details["to"] != "operator" {
		t.Errorf("role change event = %+v", e)
	}
	if e := all[1]; e.ActorUserID != acc.User.ID {
		t.Errorf("an acceptance is by the person accepting: %+v", e)
	}
}

func TestAudit_FailedAndRefusedChangesAreNotRecordedAsDone(t *testing.T) {
	f := newMembersFixture(t)
	log := apptest.NewAuditLog()
	f.change.Audit = &app.Audit{Log: log, IDs: &apptest.IDs{}, Clock: f.clock}
	f.change.Run(context.Background(), f.owner, f.owner.User.ID, domain.Viewer) // last owner: refused
	if n := len(log.All()); n != 0 {
		t.Errorf("%d events for a refused change", n)
	}
}

// Events about a person's own account - verifying, resetting the
// password, being locked out - have no organisation, so the
// organisation's log cannot show them. Each person can see their own.
func TestAudit_AccountEventsAreVisibleToTheirOwner(t *testing.T) {
	f := newMembersFixture(t)
	log := apptest.NewAuditLog()
	audit := &app.Audit{Log: log, IDs: &apptest.IDs{}, Clock: f.clock}
	ctx := context.Background()
	verify := &app.VerifyEmail{Tokens: f.tokens, Users: f.accounts, Clock: f.clock, Audit: audit}
	if err := verify.Run(ctx, f.mailer.TokenTo(f.owner.User.Email)); err != nil {
		t.Fatal(err)
	}
	other, _ := f.join(t, domain.Viewer)
	log.Record(ctx, app.AuditEvent{ID: "aud_x", ActorUserID: other.User.ID, Action: app.AuditPasswordReset, TargetType: "user", TargetID: string(other.User.ID)})

	page, err := (&app.ListMyActivity{Log: log}).Run(ctx, f.owner.User.ID, app.AuditFilter{})
	if err != nil || len(page.Events) != 1 || page.Events[0].Action != app.AuditEmailVerified {
		t.Errorf("own activity = %+v, %v; want only the owner's verification", page.Events, err)
	}
}
