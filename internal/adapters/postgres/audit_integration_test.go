//go:build integration

package postgres

import (
	"context"
	"testing"

	"enquoir/internal/app"
)

func TestAuditLog_RecordListScopedAndPaged(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	org, owner := newTestOrg(t, NewAccountStore(pool))
	other, _ := newTestOrg(t, NewAccountStore(pool))
	log := NewAuditLog(pool)
	for _, a := range []string{app.AuditMemberInvited, app.AuditRoleChanged, app.AuditAgentCreated} {
		if err := log.Record(ctx, app.AuditEvent{ID: ids.NewID("aud"), OrgID: org.ID, ActorUserID: owner.ID, ActorEmail: owner.Email,
			Action: a, TargetType: "user", TargetID: "x", Details: map[string]string{"role": "viewer"}, IP: "203.0.113.5"}); err != nil {
			t.Fatal(err)
		}
	}
	log.Record(ctx, app.AuditEvent{ID: ids.NewID("aud"), OrgID: other.ID, Action: app.AuditAgentCreated})

	page, err := log.List(ctx, org.ID, app.AuditFilter{Limit: 10})
	if err != nil || len(page.Events) != 3 || page.Events[0].Action != app.AuditAgentCreated || page.Events[2].Details["role"] != "viewer" || page.Events[0].IP != "203.0.113.5" {
		t.Fatalf("page = %+v, %v", page, err)
	}
	if p, _ := log.List(ctx, org.ID, app.AuditFilter{Limit: 10, Action: app.AuditRoleChanged}); len(p.Events) != 1 {
		t.Errorf("by action: %d", len(p.Events))
	}
	first, _ := log.List(ctx, org.ID, app.AuditFilter{Limit: 2})
	second, _ := log.List(ctx, org.ID, app.AuditFilter{Limit: 2, Cursor: first.NextCursor})
	if len(second.Events) != 1 || second.NextCursor != "" {
		t.Errorf("paging: %+v", second)
	}
}

func TestAuditLog_ListForUserIncludesAccountLevelEvents(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	org, me := newTestOrg(t, NewAccountStore(pool))
	_, other := newTestOrg(t, NewAccountStore(pool))
	log := NewAuditLog(pool)
	log.Record(ctx, app.AuditEvent{ID: ids.NewID("aud"), ActorUserID: me.ID, Action: app.AuditPasswordReset, TargetType: "user", TargetID: string(me.ID)})
	log.Record(ctx, app.AuditEvent{ID: ids.NewID("aud"), OrgID: org.ID, ActorUserID: other.ID, Action: app.AuditRoleChanged, TargetType: "user", TargetID: string(me.ID)})
	log.Record(ctx, app.AuditEvent{ID: ids.NewID("aud"), ActorUserID: other.ID, Action: app.AuditPasswordReset, TargetType: "user", TargetID: string(other.ID)})

	page, err := log.ListForUser(ctx, me.ID, app.AuditFilter{Limit: 10})
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("my events = %+v, %v; want my reset (no organisation) and the role change done to me", page.Events, err)
	}
}
