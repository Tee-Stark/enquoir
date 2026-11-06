//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"enquoir/internal/adapters/credentials"
	"enquoir/internal/app"
	"enquoir/internal/domain"
)

var ids = credentials.RandomIDs{}

// newTestOrg stores a fresh organisation with an owner and returns both.
func newTestOrg(t *testing.T, store *AccountStore) (app.Organisation, app.User) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	org := app.Organisation{ID: domain.OrgID(ids.NewID("org")), Name: "Test " + t.Name(), CreatedAt: now}
	user := app.User{
		ID:           domain.UserID(ids.NewID("usr")),
		Email:        domain.Email(fmt.Sprintf("%s@example.com", ids.NewID("u"))),
		PasswordHash: "hashed", CreatedAt: now,
	}
	if err := store.CreateOrganisationWithOwner(context.Background(), org, user); err != nil {
		t.Fatalf("creating test organisation: %v", err)
	}
	return org, user
}

func TestAccountStore_CreateAndFind(t *testing.T) {
	ctx := context.Background()
	store := NewAccountStore(testPool(t))
	org, user := newTestOrg(t, store)

	got, err := store.UserByEmail(ctx, user.Email)
	if err != nil || got.ID != user.ID || got.PasswordHash != "hashed" || got.EmailVerified {
		t.Errorf("UserByEmail = %+v, %v", got, err)
	}
	if u, err := store.UserByID(ctx, user.ID); err != nil || u.Email != user.Email {
		t.Errorf("UserByID = %+v, %v", u, err)
	}
	m, err := store.Membership(ctx, org.ID, user.ID)
	if err != nil || m.Role != domain.Owner || m.Organisation.Name != org.Name || !m.Organisation.CreatedAt.Equal(org.CreatedAt) {
		t.Errorf("Membership = %+v, %v", m, err)
	}
	other, _ := newTestOrg(t, store)
	if _, err := store.Membership(ctx, other.ID, user.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("membership in an organisation the user is not in: %v", err)
	}
	if ms, err := store.MembershipsOf(ctx, user.ID); err != nil || len(ms) != 1 || ms[0].Organisation.ID != org.ID {
		t.Errorf("MembershipsOf = %+v, %v", ms, err)
	}
	if _, err := store.UserByEmail(ctx, "nobody-"+user.Email); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("UserByEmail(unknown) err = %v, want ErrNotFound", err)
	}
}

// A taken email fails the whole sign-up: no orphan organisation.
func TestAccountStore_TakenEmailStoresNothing(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	store := NewAccountStore(pool)
	_, user := newTestOrg(t, store)

	org2 := app.Organisation{ID: domain.OrgID(ids.NewID("org")), Name: "Second", CreatedAt: time.Now()}
	dup := app.User{ID: domain.UserID(ids.NewID("usr")), Email: user.Email, PasswordHash: "x", CreatedAt: time.Now()}
	if err := store.CreateOrganisationWithOwner(ctx, org2, dup); !errors.Is(err, app.ErrEmailTaken) {
		t.Fatalf("err = %v, want ErrEmailTaken", err)
	}
	var n int
	pool.QueryRow(ctx, `SELECT count(*) FROM organisations WHERE id = $1`, string(org2.ID)).Scan(&n)
	if n != 0 {
		t.Error("the organisation was stored although its owner was not")
	}
}

func TestSessionStore_CreateGetRevoke(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	org, user := newTestOrg(t, NewAccountStore(pool))
	sessions := NewSessionStore(pool)

	now := time.Now().UTC().Truncate(time.Microsecond)
	s := app.Session{ID: ids.NewID("ses"), TokenHash: app.HashSecret(ids.NewID("ses")), UserID: user.ID, OrgID: org.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := sessions.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := sessions.Get(ctx, s.TokenHash)
	if err != nil || got.UserID != user.ID || got.ID != s.ID || got.OrgID != org.ID || !got.ExpiresAt.Equal(s.ExpiresAt) || got.RevokedAt != nil {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if err := sessions.Revoke(ctx, s.TokenHash, now); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Revoke(ctx, s.TokenHash, now.Add(time.Minute)); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	got, _ = sessions.Get(ctx, s.TokenHash)
	if got.RevokedAt == nil || !got.RevokedAt.Equal(now) {
		t.Errorf("RevokedAt = %v, want the first revocation time %v", got.RevokedAt, now)
	}
	if _, err := sessions.Get(ctx, "unknown"); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("Get(unknown) err = %v, want ErrNotFound", err)
	}
}

func TestAccountStore_VerifyAndSetPassword(t *testing.T) {
	ctx := context.Background()
	store := NewAccountStore(testPool(t))
	_, user := newTestOrg(t, store)
	now := time.Now()
	if err := store.MarkEmailVerified(ctx, user.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPassword(ctx, user.ID, "new-hash", now); err != nil {
		t.Fatal(err)
	}
	u, _ := store.UserByID(ctx, user.ID)
	if !u.EmailVerified || u.PasswordHash != "new-hash" {
		t.Errorf("user = %+v", u)
	}
	if err := store.SetPassword(ctx, "usr_nobody", "x", now); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("unknown user: %v", err)
	}
}
