package app_test

import (
	"context"
	"errors"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

// A request that resolves a session while it is being revoked must not
// leave it cached as valid: the revoke is committed first, then the
// cache is cleared - never the other way round. The store's
// BeforeRevoke hook is that racing request, at the worst moment.
func TestCache_ARevokeRacingAResolveNeverLeavesTheSessionCached(t *testing.T) {
	cases := map[string]func(f *membersFixture, victim app.UserPrincipal, token string) error{
		"log-out": func(f *membersFixture, _ app.UserPrincipal, token string) error {
			return f.logOut.Run(context.Background(), token)
		},
		"switching organisation": func(f *membersFixture, victim app.UserPrincipal, token string) error {
			sw := &app.SwitchOrganisation{Accounts: f.accounts, Sessions: f.sessions, Issuer: f.issuer, Cache: f.resolve.Cache}
			own, _ := f.accounts.MembershipsOf(context.Background(), victim.User.ID)
			_, err := sw.Run(context.Background(), victim, token, own[0].Organisation.ID)
			return err
		},
		"role change": func(f *membersFixture, victim app.UserPrincipal, _ string) error {
			return f.change.Run(context.Background(), f.owner, victim.User.ID, domain.Operator)
		},
		"removal": func(f *membersFixture, victim app.UserPrincipal, _ string) error {
			return f.remove.Run(context.Background(), f.owner, victim.User.ID)
		},
		"password change": func(f *membersFixture, victim app.UserPrincipal, _ string) error {
			// Changed from another session: the victim's must end.
			other, _ := f.logIn.Run(context.Background(), string(victim.User.Email), "correct horse battery")
			p, _ := f.resolve.Run(context.Background(), other.Session.Token)
			ch := &app.ChangePassword{Users: f.accounts, Accounts: f.accounts, Sessions: f.sessions, Hasher: f.hasher, Emails: f.emails, Clock: f.clock, Cache: f.resolve.Cache}
			f.sessions.BeforeRevoke = f.raceHook
			return ch.Run(context.Background(), p, "correct horse battery", "a brand new passphrase")
		},
		"password reset": func(f *membersFixture, victim app.UserPrincipal, _ string) error {
			f.sessions.BeforeRevoke = nil
			(&app.ForgotPassword{Accounts: f.accounts, Emails: f.emails}).Run(context.Background(), string(victim.User.Email))
			f.sessions.BeforeRevoke = f.raceHook
			rs := &app.ResetPassword{Tokens: f.tokens, Users: f.accounts, Accounts: f.accounts, Sessions: f.sessions, Hasher: f.hasher, Emails: f.emails, Clock: f.clock, Cache: f.resolve.Cache}
			return rs.Run(context.Background(), f.mailer.TokenTo(victim.User.Email), "a brand new passphrase")
		},
	}
	for name, revoke := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMembersFixture(t)
			f.withCache()
			ctx := context.Background()
			victim, token := f.join(t, domain.Viewer)
			f.raceHook = func() { f.resolve.Run(ctx, token) }
			f.sessions.BeforeRevoke = f.raceHook
			if err := revoke(f, victim, token); err != nil {
				t.Fatal(err)
			}
			f.sessions.BeforeRevoke = nil
			if _, err := f.resolve.Run(ctx, token); !errors.Is(err, app.ErrUnauthenticated) {
				t.Errorf("after the %s the session still resolves from the cache: %v", name, err)
			}
		})
	}
}
