package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
)

type agentsFixture struct {
	store  *apptest.Agents
	clock  *apptest.Clock
	create *app.CreateAgent
	list   *app.ListAgents
	get    *app.GetAgent
	rotate *app.RotateAgentKey
	revoke *app.RevokeAgent
	auth   *app.AuthenticateAgent
}

func newAgentsFixture() *agentsFixture {
	store, clock := apptest.NewAgents(), apptest.NewClock()
	ids, secrets := &apptest.IDs{}, &apptest.Secrets{}
	return &agentsFixture{
		store: store, clock: clock,
		create: &app.CreateAgent{Agents: store, IDs: ids, Secrets: secrets, Clock: clock},
		list:   &app.ListAgents{Agents: store},
		get:    &app.GetAgent{Agents: store},
		rotate: &app.RotateAgentKey{Agents: store, IDs: ids, Secrets: secrets, Clock: clock},
		revoke: &app.RevokeAgent{Agents: store, Clock: clock},
		auth:   &app.AuthenticateAgent{Agents: store, Clock: clock},
	}
}

const orgA, orgB = domain.OrgID("org_a"), domain.OrgID("org_b")

func TestCreateAgent_IssuesAKeyShownOnce(t *testing.T) {
	f := newAgentsFixture()
	got, err := f.create.Run(context.Background(), orgA, "  Invoice bot  ")
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent.Name != "Invoice bot" || got.Agent.OrgID != orgA || got.Agent.Revoked() {
		t.Errorf("agent = %+v", got.Agent)
	}
	if !strings.HasPrefix(got.Secret, "enq_sk_") {
		t.Errorf("secret = %q, want an enq_sk_ key", got.Secret)
	}
	if got.Agent.Key == nil || !strings.HasPrefix(got.Secret, got.Agent.Key.Prefix) || len(got.Agent.Key.Prefix) != 12 {
		t.Errorf("key = %+v, want a 12-character display prefix of the secret", got.Agent.Key)
	}
	if f.store.StoresPlaintext(got.Secret) {
		t.Error("the API key was stored in plaintext; only its hash may be")
	}
}

func TestCreateAgent_RequiresAName(t *testing.T) {
	_, err := newAgentsFixture().create.Run(context.Background(), orgA, " ")
	var verr *app.ValidationError
	if !errors.As(err, &verr) || verr.Field != "name" {
		t.Errorf("err = %v, want a ValidationError on name", err)
	}
}

func TestAuthenticateAgent(t *testing.T) {
	f := newAgentsFixture()
	ctx := context.Background()
	created, _ := f.create.Run(ctx, orgA, "bot")

	p, err := f.auth.Run(ctx, created.Secret)
	if err != nil || p.AgentID != created.Agent.ID || p.OrgID != orgA || p.AgentName != "bot" {
		t.Fatalf("Authenticate = %+v, %v", p, err)
	}
	if got, _ := f.get.Run(ctx, orgA, created.Agent.ID); got.Key.LastUsedAt == nil {
		t.Error("authenticating did not record the key's last use")
	}
	for _, bad := range []string{"", "enq_sk_made_up", created.Secret + "x"} {
		if _, err := f.auth.Run(ctx, bad); !errors.Is(err, app.ErrUnauthenticated) {
			t.Errorf("Authenticate(%q) err = %v, want ErrUnauthenticated", bad, err)
		}
	}
}

func TestRotateAgentKey_ReplacesTheKey(t *testing.T) {
	f := newAgentsFixture()
	ctx := context.Background()
	created, _ := f.create.Run(ctx, orgA, "bot")
	f.clock.Advance(time.Hour)

	rotated, err := f.rotate.Run(ctx, orgA, created.Agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Secret == created.Secret || rotated.Agent.Key.ID == created.Agent.Key.ID {
		t.Error("rotation returned the old key")
	}
	if _, err := f.auth.Run(ctx, created.Secret); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("old key after rotation: err = %v, want ErrUnauthenticated", err)
	}
	if _, err := f.auth.Run(ctx, rotated.Secret); err != nil {
		t.Errorf("new key: %v", err)
	}
}

func TestRevokeAgent_StopsItsKeyAndKeepsTheRecord(t *testing.T) {
	f := newAgentsFixture()
	ctx := context.Background()
	created, _ := f.create.Run(ctx, orgA, "bot")

	if err := f.revoke.Run(ctx, orgA, created.Agent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.auth.Run(ctx, created.Secret); !errors.Is(err, app.ErrUnauthenticated) {
		t.Errorf("revoked agent's key: err = %v, want ErrUnauthenticated", err)
	}
	got, err := f.get.Run(ctx, orgA, created.Agent.ID)
	if err != nil || !got.Revoked() {
		t.Errorf("revoked agent = %+v, %v; want it kept, marked revoked", got, err)
	}
	if _, err := f.rotate.Run(ctx, orgA, created.Agent.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("rotating a revoked agent: err = %v, want ErrNotFound", err)
	}
}

// Another organisation's agent is not found, never forbidden: its
// existence is not revealed.
func TestAgents_AreScopedToTheirOrganisation(t *testing.T) {
	f := newAgentsFixture()
	ctx := context.Background()
	a, _ := f.create.Run(ctx, orgA, "a-bot")
	f.create.Run(ctx, orgB, "b-bot")

	if _, err := f.get.Run(ctx, orgB, a.Agent.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("get across orgs: err = %v, want ErrNotFound", err)
	}
	if _, err := f.rotate.Run(ctx, orgB, a.Agent.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("rotate across orgs: err = %v, want ErrNotFound", err)
	}
	if err := f.revoke.Run(ctx, orgB, a.Agent.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("revoke across orgs: err = %v, want ErrNotFound", err)
	}
	list, _ := f.list.Run(ctx, orgB)
	if len(list) != 1 || list[0].Name != "b-bot" {
		t.Errorf("org B's list = %+v, want only its own agent", list)
	}
}
