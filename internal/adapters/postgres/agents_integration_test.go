//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"enquoir/internal/adapters/credentials"
	"enquoir/internal/app"
)

func newAgentUseCases(store *AgentStore) (*app.CreateAgent, *app.RotateAgentKey, *app.RevokeAgent, *app.AuthenticateAgent) {
	clock := realClock{}
	ids, secrets := credentials.RandomIDs{}, credentials.RandomSecrets{}
	return &app.CreateAgent{Agents: store, IDs: ids, Secrets: secrets, Clock: clock},
		&app.RotateAgentKey{Agents: store, IDs: ids, Secrets: secrets, Clock: clock},
		&app.RevokeAgent{Agents: store, Clock: clock},
		&app.AuthenticateAgent{Agents: store, Clock: clock}
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// The app's agent rules, run against the real store.
func TestAgentStore_LifecycleAndScoping(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	accounts, store := NewAccountStore(pool), NewAgentStore(pool)
	orgA, _ := newTestOrg(t, accounts)
	orgB, _ := newTestOrg(t, accounts)
	create, rotate, revoke, auth := newAgentUseCases(store)

	issued, err := create.Run(ctx, orgA.ID, "bot")
	if err != nil {
		t.Fatal(err)
	}
	p, err := auth.Run(ctx, issued.Secret)
	if err != nil || p.AgentID != issued.Agent.ID || p.OrgID != orgA.ID {
		t.Fatalf("auth = %+v, %v", p, err)
	}
	got, _ := store.GetAgent(ctx, orgA.ID, issued.Agent.ID)
	if got.Key == nil || got.Key.LastUsedAt == nil || got.Key.Prefix != issued.Secret[:12] {
		t.Errorf("agent after auth = %+v, want its key with last use recorded", got.Key)
	}

	if _, err := store.GetAgent(ctx, orgB.ID, issued.Agent.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("get across orgs: %v, want ErrNotFound", err)
	}
	if err := revoke.Run(ctx, orgB.ID, issued.Agent.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("revoke across orgs: %v, want ErrNotFound", err)
	}
	if list, _ := store.ListAgents(ctx, orgB.ID); len(list) != 0 {
		t.Errorf("org B lists %d agents, want 0", len(list))
	}

	rotated, err := rotate.Run(ctx, orgA.ID, issued.Agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Run(ctx, issued.Secret); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("old key after rotation: %v, want ErrUnauthenticated", err)
	}
	if _, err := auth.Run(ctx, rotated.Secret); err != nil {
		t.Errorf("new key: %v", err)
	}

	if err := revoke.Run(ctx, orgA.ID, issued.Agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Run(ctx, rotated.Secret); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("key of revoked agent: %v, want ErrUnauthenticated", err)
	}
	got, err = store.GetAgent(ctx, orgA.ID, issued.Agent.ID)
	if err != nil || !got.Revoked() || got.Key != nil {
		t.Errorf("revoked agent = %+v, %v; want kept, revoked, no active key", got, err)
	}
	if _, err := rotate.Run(ctx, orgA.ID, issued.Agent.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("rotating revoked agent: %v, want ErrNotFound", err)
	}
}
