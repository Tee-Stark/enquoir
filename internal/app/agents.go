package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"enquoir/internal/domain"
)

const (
	apiKeyPrefix        = "enq_sk"
	apiKeyDisplayLength = 12
)

// APIKey is an agent's credential. Only its hash is stored; Prefix is
// the first characters of the secret, for people to recognise it by.
type APIKey struct {
	ID         string
	AgentID    domain.AgentID
	OrgID      domain.OrgID
	Prefix     string
	SecretHash string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

// Agent is an AI agent that submits intents, with its active key (nil
// once revoked).
type Agent struct {
	ID        domain.AgentID
	OrgID     domain.OrgID
	Name      string
	CreatedAt time.Time
	// CreatedBy is empty for agents created before this field existed.
	CreatedBy domain.UserID
	RevokedAt *time.Time
	Key       *APIKey
}

func (a Agent) Revoked() bool { return a.RevokedAt != nil }

// AgentPrincipal is an authenticated agent.
type AgentPrincipal struct {
	AgentID   domain.AgentID
	AgentName string
	OrgID     domain.OrgID
	KeyID     string
}

// AgentStore persists agents and keys. Every method but
// ActiveKeyByHash is scoped by organisation: an agent of another
// organisation is ErrNotFound.
type AgentStore interface {
	// CreateAgent stores an agent and its first key together.
	CreateAgent(ctx context.Context, a Agent, key APIKey) error
	ListAgents(ctx context.Context, org domain.OrgID) ([]Agent, error)
	GetAgent(ctx context.Context, org domain.OrgID, id domain.AgentID) (Agent, error)
	// ReplaceKey revokes the agent's active key and stores key in its
	// place. ErrNotFound if the agent is unknown or revoked.
	ReplaceKey(ctx context.Context, org domain.OrgID, id domain.AgentID, key APIKey, at time.Time) error
	// RevokeAgent revokes an agent and its key. ErrNotFound if unknown;
	// revoking an already revoked agent is not an error.
	RevokeAgent(ctx context.Context, org domain.OrgID, id domain.AgentID, at time.Time) error
	// ActiveKeyByHash finds an unrevoked key of an unrevoked agent.
	ActiveKeyByHash(ctx context.Context, hash string) (APIKey, Agent, error)
	// TouchKey records a key's use.
	TouchKey(ctx context.Context, keyID string, at time.Time) error
}

// IssuedAgent is an agent with its newly issued key. Secret is shown
// to the caller once and never stored.
type IssuedAgent struct {
	Agent  Agent
	Secret string
}

func newAPIKey(ids IDGenerator, secrets SecretGenerator, org domain.OrgID, agent domain.AgentID, now time.Time) (APIKey, string) {
	secret := secrets.NewSecret(apiKeyPrefix)
	return APIKey{
		ID:         ids.NewID("key"),
		AgentID:    agent,
		OrgID:      org,
		Prefix:     secret[:apiKeyDisplayLength],
		SecretHash: HashSecret(secret),
		CreatedAt:  now,
	}, secret
}

// CreateAgent registers an agent and issues its API key.
type CreateAgent struct {
	Agents  AgentStore
	IDs     IDGenerator
	Secrets SecretGenerator
	Clock   Clock
	Audit   *Audit
}

func (c *CreateAgent) Run(ctx context.Context, org domain.OrgID, name string) (IssuedAgent, error) {
	name, err := domain.NewName(name)
	if err != nil {
		return IssuedAgent{}, invalid("name", err)
	}
	now := c.Clock.Now()
	agent := Agent{
		ID:        domain.AgentID(c.IDs.NewID("agt")),
		OrgID:     org,
		Name:      name,
		CreatedAt: now,
		CreatedBy: ActorFrom(ctx).UserID,
	}
	key, secret := newAPIKey(c.IDs, c.Secrets, org, agent.ID, now)
	if err := c.Agents.CreateAgent(ctx, agent, key); err != nil {
		return IssuedAgent{}, fmt.Errorf("creating agent: %w", err)
	}
	agent.Key = &key
	c.Audit.record(ctx, org, AuditAgentCreated, "agent", string(agent.ID), map[string]string{"name": name, "key_prefix": key.Prefix})
	return IssuedAgent{Agent: agent, Secret: secret}, nil
}

// ListAgents lists an organisation's agents, revoked ones included.
type ListAgents struct{ Agents AgentStore }

func (l *ListAgents) Run(ctx context.Context, org domain.OrgID) ([]Agent, error) {
	return l.Agents.ListAgents(ctx, org)
}

// GetAgent fetches one of an organisation's agents.
type GetAgent struct{ Agents AgentStore }

func (g *GetAgent) Run(ctx context.Context, org domain.OrgID, id domain.AgentID) (Agent, error) {
	return g.Agents.GetAgent(ctx, org, id)
}

// RotateAgentKey replaces an agent's key; the old key stops working
// at once.
type RotateAgentKey struct {
	Agents  AgentStore
	IDs     IDGenerator
	Secrets SecretGenerator
	Clock   Clock
	Cache   *Cache
	Audit   *Audit
}

func (r *RotateAgentKey) Run(ctx context.Context, org domain.OrgID, id domain.AgentID) (IssuedAgent, error) {
	now := r.Clock.Now()
	key, secret := newAPIKey(r.IDs, r.Secrets, org, id, now)
	if err := r.Agents.ReplaceKey(ctx, org, id, key, now); err != nil {
		return IssuedAgent{}, err
	}
	r.Cache.InvalidateAgent(ctx, id)
	r.Audit.record(ctx, org, AuditAgentKeyRotated, "agent", string(id), map[string]string{"key_prefix": key.Prefix})
	agent, err := r.Agents.GetAgent(ctx, org, id)
	if err != nil {
		return IssuedAgent{}, err
	}
	return IssuedAgent{Agent: agent, Secret: secret}, nil
}

// RevokeAgent revokes an agent. Its record and history stay.
type RevokeAgent struct {
	Agents AgentStore
	Clock  Clock
	Cache  *Cache
	Audit  *Audit
}

func (r *RevokeAgent) Run(ctx context.Context, org domain.OrgID, id domain.AgentID) error {
	if err := r.Agents.RevokeAgent(ctx, org, id, r.Clock.Now()); err != nil {
		return err
	}
	r.Cache.InvalidateAgent(ctx, id)
	r.Audit.record(ctx, org, AuditAgentRevoked, "agent", string(id), nil)
	return nil
}

// AuthenticateAgent turns an API key into the agent it belongs to.
type AuthenticateAgent struct {
	Agents AgentStore
	Clock  Clock
	Cache  *Cache
}

// Run is on every agent request. A cached key skips the database and
// the last-used write; last_used_at is recorded at least once per
// cache period.
func (a *AuthenticateAgent) Run(ctx context.Context, secret string) (AgentPrincipal, error) {
	if secret == "" {
		return AgentPrincipal{}, ErrUnauthenticated
	}
	hash := HashSecret(secret)
	if p, ok := a.Cache.agent(ctx, hash); ok {
		return p, nil
	}
	p, err := a.authenticate(ctx, hash)
	if err != nil {
		return AgentPrincipal{}, err
	}
	a.Cache.putAgent(ctx, hash, p)
	return p, nil
}

func (a *AuthenticateAgent) authenticate(ctx context.Context, hash string) (AgentPrincipal, error) {
	key, agent, err := a.Agents.ActiveKeyByHash(ctx, hash)
	if errors.Is(err, ErrNotFound) {
		return AgentPrincipal{}, ErrUnauthenticated
	}
	if err != nil {
		return AgentPrincipal{}, err
	}
	if err := a.Agents.TouchKey(ctx, key.ID, a.Clock.Now()); err != nil {
		return AgentPrincipal{}, err
	}
	return AgentPrincipal{AgentID: agent.ID, AgentName: agent.Name, OrgID: agent.OrgID, KeyID: key.ID}, nil
}
