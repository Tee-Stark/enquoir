//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

func addMember(t *testing.T, store *AccountStore, org domain.OrgID, role domain.Role) app.User {
	t.Helper()
	_, u := newTestOrg(t, store)
	tx, _ := store.pool.Begin(context.Background())
	if err := insertMembership(context.Background(), tx, org, u.ID, role, time.Now()); err != nil {
		t.Fatal(err)
	}
	tx.Commit(context.Background())
	return u
}

// Two owners each demoting the other at once: one wins, one is refused,
// and the organisation keeps an owner.
func TestMemberStore_ConcurrentDemotionsKeepAnOwner(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	store := NewAccountStore(pool)
	org, first := newTestOrg(t, store)
	second := addMember(t, store, org.ID, domain.Owner)

	check := func(from domain.Role, owners int) error {
		if from == domain.Owner && owners <= 1 {
			return app.ErrLastOwner
		}
		return nil
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, u := range []domain.UserID{first.ID, second.ID} {
		wg.Add(1)
		go func(i int, u domain.UserID) {
			defer wg.Done()
			errs[i] = store.ChangeRole(ctx, org.ID, u, domain.Admin, check)
		}(i, u)
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) {
		t.Errorf("errors = %v; want exactly one demotion to succeed", errs)
	}
	members, _ := store.ListMembers(ctx, org.ID)
	owners := 0
	for _, m := range members {
		if m.Role == domain.Owner {
			owners++
		}
		if m.User.PasswordHash != "" {
			t.Error("ListMembers exposed a password hash")
		}
	}
	if owners != 1 {
		t.Errorf("%d owners left, want 1", owners)
	}
}

func TestInvitationStore_AcceptCreatesTheAccountAndMembership(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	accounts, invites := NewAccountStore(pool), NewInvitationStore(pool)
	org, owner := newTestOrg(t, accounts)
	now := time.Now().UTC().Truncate(time.Microsecond)
	email := domain.Email(ids.NewID("bob") + "@example.com")

	first := app.Invitation{ID: ids.NewID("inv"), OrgID: org.ID, Email: email, Role: domain.Viewer, InvitedBy: owner.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	second := first
	second.ID, second.Role = ids.NewID("inv"), domain.Operator
	invites.CreateInvitation(ctx, first)
	invites.CreateInvitation(ctx, second)
	pending, _ := invites.ListPendingInvitations(ctx, org.ID, now)
	if len(pending) != 1 || pending[0].ID != second.ID || pending[0].OrgName != org.Name {
		t.Fatalf("pending = %+v; re-inviting must replace the earlier invitation", pending)
	}

	bob := app.User{ID: domain.UserID(ids.NewID("usr")), Email: email, PasswordHash: "h", CreatedAt: now}
	if err := invites.AcceptInvitation(ctx, second.ID, bob.ID, &bob, now); err != nil {
		t.Fatal(err)
	}
	u, _ := accounts.UserByID(ctx, bob.ID)
	m, err := accounts.Membership(ctx, org.ID, bob.ID)
	if !u.EmailVerified || err != nil || m.Role != domain.Operator {
		t.Errorf("user %+v, membership %+v, %v", u, m, err)
	}
	if err := invites.AcceptInvitation(ctx, second.ID, bob.ID, nil, now); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("accepting twice: %v", err)
	}
	if err := invites.RevokeInvitation(ctx, org.ID, first.ID, now); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("revoking a replaced invitation: %v", err)
	}
}
