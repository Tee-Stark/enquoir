package app_test

import (
	"context"
	"errors"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
)

// withCache gives every use case in the fixture one shared cache.
func (f *membersFixture) withCache() *apptest.PrincipalStore {
	store := apptest.NewPrincipalStore()
	c := &app.Cache{Store: store}
	f.resolve.Cache, f.logOut.Cache, f.change.Cache, f.remove.Cache, f.accept.Cache = c, c, c, c, c
	return store
}

func TestCache_ServesARepeatedResolveWithoutThePasswordHash(t *testing.T) {
	f := newMembersFixture(t)
	store := f.withCache()
	ctx := context.Background()
	_, token := f.join(t, domain.Viewer)
	f.resolve.Run(ctx, token)
	before := store.Hits
	p, err := f.resolve.Run(ctx, token)
	if err != nil || store.Hits != before+1 || p.Role != domain.Viewer {
		t.Fatalf("second resolve: %+v, %v, hits %d; want a cache hit", p, err, store.Hits)
	}
	if store.Contains("hashed:") || p.User.PasswordHash != "" {
		t.Error("a password hash reached the cache or the principal")
	}
}

// Every change that affects access must end cached access at once,
// not after the cache's TTL.
func TestCache_EveryAccessChangeInvalidates(t *testing.T) {
	cases := map[string]func(f *membersFixture, target app.UserPrincipal, token string) error{
		"role change": func(f *membersFixture, target app.UserPrincipal, _ string) error {
			return f.change.Run(context.Background(), f.owner, target.User.ID, domain.Operator)
		},
		"removal": func(f *membersFixture, target app.UserPrincipal, _ string) error {
			return f.remove.Run(context.Background(), f.owner, target.User.ID)
		},
		"log-out": func(f *membersFixture, _ app.UserPrincipal, token string) error {
			return f.logOut.Run(context.Background(), token)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newMembersFixture(t)
			f.withCache()
			ctx := context.Background()
			target, token := f.join(t, domain.Viewer)
			f.resolve.Run(ctx, token) // now cached
			if err := change(f, target, token); err != nil {
				t.Fatal(err)
			}
			if _, err := f.resolve.Run(ctx, token); !errors.Is(err, app.ErrUnauthenticated) {
				t.Errorf("after the %s the cached session still resolves: %v", name, err)
			}
		})
	}
}

func TestCache_VerificationIsSeenAtOnce(t *testing.T) {
	f := newMembersFixture(t)
	store := f.withCache()
	ctx := context.Background()
	if f.owner.User.EmailVerified {
		t.Fatal("fixture owner should start unverified")
	}
	token := f.mailer.TokenTo(f.owner.User.Email)
	sessionToken := f.ownerToken
	f.resolve.Run(ctx, sessionToken)
	verify := &app.VerifyEmail{Tokens: f.tokens, Users: f.accounts, Clock: f.clock, Cache: &app.Cache{Store: store}}
	if err := verify.Run(ctx, token); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.resolve.Run(ctx, sessionToken); !p.User.EmailVerified {
		t.Error("the cached session still reads unverified")
	}
}

func TestCache_AgentKeysRotateAndRevokeAtOnce(t *testing.T) {
	store, clock := apptest.NewAgents(), apptest.NewClock()
	cache := &app.Cache{Store: apptest.NewPrincipalStore()}
	ids, secrets := &apptest.IDs{}, &apptest.Secrets{}
	create := &app.CreateAgent{Agents: store, IDs: ids, Secrets: secrets, Clock: clock}
	auth := &app.AuthenticateAgent{Agents: store, Clock: clock, Cache: cache}
	rotate := &app.RotateAgentKey{Agents: store, IDs: ids, Secrets: secrets, Clock: clock, Cache: cache}
	revoke := &app.RevokeAgent{Agents: store, Clock: clock, Cache: cache}
	ctx := context.Background()

	issued, _ := create.Run(ctx, "org_a", "bot")
	auth.Run(ctx, issued.Secret)
	auth.Run(ctx, issued.Secret)
	rotated, _ := rotate.Run(ctx, "org_a", issued.Agent.ID)
	if _, err := auth.Run(ctx, issued.Secret); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("a rotated-away key still works from the cache: %v", err)
	}
	auth.Run(ctx, rotated.Secret)
	revoke.Run(ctx, "org_a", issued.Agent.ID)
	if _, err := auth.Run(ctx, rotated.Secret); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("a revoked agent's key still works from the cache: %v", err)
	}
}
