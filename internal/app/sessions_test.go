package app_test

import (
	"context"
	"errors"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
)

func TestSessions_ListAndRevoke(t *testing.T) {
	f := newAccountsFixture()
	ctx := context.Background()
	cache := &app.Cache{Store: apptest.NewPrincipalStore()}
	f.resolve.Cache = cache
	first, _ := f.signUp.Run(ctx, validSignUp)
	second, _ := f.logIn.Run(ctx, "ada@example.com", validSignUp.Password)
	third, _ := f.logIn.Run(ctx, "ada@example.com", validSignUp.Password)
	eve, _ := f.signUp.Run(ctx, app.SignUpCommand{Email: "eve@example.com", Password: "correct horse battery", OrganisationName: "Eve"})
	p, _ := f.resolve.Run(ctx, first.Session.Token)
	f.resolve.Run(ctx, second.Session.Token) // cached

	list := &app.ListSessions{Sessions: f.sessions, Clock: f.clock}
	revoke := &app.RevokeSession{Sessions: f.sessions, Clock: f.clock, Cache: cache}
	others := &app.RevokeOtherSessions{Sessions: f.sessions, Clock: f.clock, Cache: cache}

	got, err := list.Run(ctx, p)
	if err != nil || len(got) != 3 {
		t.Fatalf("sessions = %d, %v; want ada's 3", len(got), err)
	}
	var secondID string
	current := 0
	for _, s := range got {
		if s.Current {
			current++
		}
		if s.ID != p.SessionID && secondID == "" {
			secondID = s.ID
		}
	}
	if current != 1 {
		t.Errorf("%d sessions marked current, want 1", current)
	}

	evePrincipal, _ := f.resolve.Run(ctx, eve.Session.Token)
	if err := revoke.Run(ctx, p, evePrincipal.SessionID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("revoking someone else's session: %v, want ErrNotFound", err)
	}
	if err := revoke.Run(ctx, p, secondID); err != nil {
		t.Fatal(err)
	}
	if got, _ := list.Run(ctx, p); len(got) != 2 {
		t.Errorf("after revoking one: %d", len(got))
	}

	if err := others.Run(ctx, p); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{second.Session.Token, third.Session.Token} {
		if _, err := f.resolve.Run(ctx, tok); !errors.Is(err, app.ErrUnauthenticated) {
			t.Errorf("another session (cached or not) survived: %v", err)
		}
	}
	if _, err := f.resolve.Run(ctx, first.Session.Token); err != nil {
		t.Errorf("the current session was revoked: %v", err)
	}
}
