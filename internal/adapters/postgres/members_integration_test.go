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

	tokens := NewTokenStore(pool)
	link := app.HashSecret(ids.NewID("tok"))
	tokens.Create(ctx, app.OneTimeToken{Hash: link, Purpose: app.PurposeInvitation, Subject: second.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)})

	bob := app.User{ID: domain.UserID(ids.NewID("usr")), Email: email, PasswordHash: "h", CreatedAt: now}
	if err := invites.AcceptInvitation(ctx, app.AcceptInvitationCommand{InvitationID: second.ID, TokenHash: "not-the-link", UserID: bob.ID, NewUser: &bob, At: now}); !errors.Is(err, app.ErrTokenInvalid) {
		t.Fatalf("accepting with the wrong link: %v", err)
	}
	if _, err := accounts.UserByID(ctx, bob.ID); !errors.Is(err, app.ErrNotFound) {
		t.Fatal("a failed acceptance created the user anyway")
	}
	if err := invites.AcceptInvitation(ctx, app.AcceptInvitationCommand{InvitationID: second.ID, TokenHash: link, UserID: bob.ID, NewUser: &bob, At: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := tokens.Peek(ctx, link, app.PurposeInvitation, now); !errors.Is(err, app.ErrTokenInvalid) {
		t.Error("the link still works after acceptance")
	}
	u, _ := accounts.UserByID(ctx, bob.ID)
	m, err := accounts.Membership(ctx, org.ID, bob.ID)
	if !u.EmailVerified || err != nil || m.Role != domain.Operator {
		t.Errorf("user %+v, membership %+v, %v", u, m, err)
	}
	if err := invites.AcceptInvitation(ctx, app.AcceptInvitationCommand{InvitationID: second.ID, TokenHash: link, UserID: bob.ID, At: now}); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("accepting twice: %v", err)
	}
	if err := invites.RevokeInvitation(ctx, org.ID, first.ID, now); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("revoking a replaced invitation: %v", err)
	}
}

// Claiming an unverified account replaces its password and verifies it
// in the same transaction as the membership - and refuses an account
// that became verified meanwhile, changing nothing.
func TestInvitationStore_ClaimOnlyAnUnverifiedAccount(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	accounts, invites, tokens := NewAccountStore(pool), NewInvitationStore(pool), NewTokenStore(pool)
	org, owner := newTestOrg(t, accounts)
	_, squatter := newTestOrg(t, accounts) // unverified
	now := time.Now().UTC().Truncate(time.Microsecond)

	invite := func() (string, string) {
		inv := app.Invitation{ID: ids.NewID("inv"), OrgID: org.ID, Email: squatter.Email, Role: domain.Operator, InvitedBy: owner.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		invites.CreateInvitation(ctx, inv)
		link := app.HashSecret(ids.NewID("tok"))
		tokens.Create(ctx, app.OneTimeToken{Hash: link, Purpose: app.PurposeInvitation, Subject: inv.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
		return inv.ID, link
	}

	id, link := invite()
	if err := invites.AcceptInvitation(ctx, app.AcceptInvitationCommand{InvitationID: id, TokenHash: link, UserID: squatter.ID, ClaimPasswordHash: "claimed-hash", At: now}); err != nil {
		t.Fatal(err)
	}
	u, _ := accounts.UserByID(ctx, squatter.ID)
	if u.PasswordHash != "claimed-hash" || !u.EmailVerified {
		t.Errorf("claimed user = %+v", u)
	}

	// Now verified: a second claim must fail and change nothing.
	accounts.RemoveMember(ctx, org.ID, squatter.ID, func(domain.Role, int) error { return nil })
	id, link = invite()
	if err := invites.AcceptInvitation(ctx, app.AcceptInvitationCommand{InvitationID: id, TokenHash: link, UserID: squatter.ID, ClaimPasswordHash: "second-hash", At: now}); err == nil {
		t.Fatal("claimed an account that is already verified")
	}
	if u, _ := accounts.UserByID(ctx, squatter.ID); u.PasswordHash != "claimed-hash" {
		t.Error("a refused claim changed the password")
	}
	if _, err := tokens.Peek(ctx, link, app.PurposeInvitation, now); err != nil {
		t.Error("a refused claim spent the link")
	}
}
