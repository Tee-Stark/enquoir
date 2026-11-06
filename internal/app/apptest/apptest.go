// Package apptest provides in-memory implementations of the app ports,
// for tests in any layer. They behave like the real stores in the ways
// tests depend on (uniqueness, not-found, revocation) and nothing more.
package apptest

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

// Hasher is a transparent, fast stand-in for the slow hasher. It
// records every Verify so tests can check timing-equalising calls.
type Hasher struct {
	mu          sync.Mutex
	VerifyCalls int
}

func (h *Hasher) Hash(pw string) (string, error) { return "hashed:" + pw, nil }

func (h *Hasher) Verify(pw, encoded string) (bool, error) {
	h.mu.Lock()
	h.VerifyCalls++
	h.mu.Unlock()
	return encoded == "hashed:"+pw, nil
}

type IDs struct {
	mu sync.Mutex
	n  int
}

func (g *IDs) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("%s_%04d", prefix, g.n)
}

type Secrets struct {
	mu sync.Mutex
	n  int
}

func (g *Secrets) NewSecret(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("%s_secret%04d", prefix, g.n)
}

type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func NewClock() *Clock {
	return &Clock{now: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)}
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type Accounts struct {
	mu    sync.Mutex
	orgs  map[domain.OrgID]app.Organisation
	users map[domain.UserID]app.User
}

func NewAccounts() *Accounts {
	return &Accounts{orgs: map[domain.OrgID]app.Organisation{}, users: map[domain.UserID]app.User{}}
}

func (a *Accounts) CreateOrganisationWithOwner(ctx context.Context, org app.Organisation, owner app.User) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, u := range a.users {
		if u.Email == owner.Email {
			return app.ErrEmailTaken
		}
	}
	a.orgs[org.ID] = org
	a.users[owner.ID] = owner
	return nil
}

func (a *Accounts) UserByEmail(ctx context.Context, email domain.Email) (app.User, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, u := range a.users {
		if u.Email == email {
			return u, nil
		}
	}
	return app.User{}, app.ErrNotFound
}

func (a *Accounts) UserWithOrganisation(ctx context.Context, id domain.UserID) (app.User, app.Organisation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[id]
	if !ok {
		return app.User{}, app.Organisation{}, app.ErrNotFound
	}
	return u, a.orgs[u.OrgID], nil
}

type Sessions struct {
	mu       sync.Mutex
	sessions map[string]app.Session
}

func NewSessions() *Sessions { return &Sessions{sessions: map[string]app.Session{}} }

func (s *Sessions) Create(ctx context.Context, sess app.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.TokenHash] = sess
	return nil
}

func (s *Sessions) Get(ctx context.Context, tokenHash string) (app.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[tokenHash]
	if !ok {
		return app.Session{}, app.ErrNotFound
	}
	return sess, nil
}

func (s *Sessions) Revoke(ctx context.Context, tokenHash string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[tokenHash]; ok {
		sess.RevokedAt = &at
		s.sessions[tokenHash] = sess
	}
	return nil
}

// StoresPlaintext reports whether any stored value contains secret.
func (s *Sessions) StoresPlaintext(secret string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.sessions {
		if strings.Contains(k, secret) {
			return true
		}
	}
	return false
}

type agentRecord struct {
	agent app.Agent
	keys  []app.APIKey
	// active is the index into keys of the active key, or -1.
	active int
}

// Agents is an in-memory app.AgentStore.
type Agents struct {
	mu     sync.Mutex
	agents map[domain.AgentID]*agentRecord
	order  []domain.AgentID
}

func NewAgents() *Agents { return &Agents{agents: map[domain.AgentID]*agentRecord{}} }

func (s *Agents) view(r *agentRecord) app.Agent {
	a := r.agent
	if r.active >= 0 {
		k := r.keys[r.active]
		a.Key = &k
	}
	return a
}

func (s *Agents) find(org domain.OrgID, id domain.AgentID) (*agentRecord, error) {
	r, ok := s.agents[id]
	if !ok || r.agent.OrgID != org {
		return nil, app.ErrNotFound
	}
	return r, nil
}

func (s *Agents) CreateAgent(ctx context.Context, a app.Agent, key app.APIKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agents[a.ID] = &agentRecord{agent: a, keys: []app.APIKey{key}, active: 0}
	s.order = append(s.order, a.ID)
	return nil
}

func (s *Agents) ListAgents(ctx context.Context, org domain.OrgID) ([]app.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []app.Agent
	for _, id := range s.order {
		if r := s.agents[id]; r.agent.OrgID == org {
			out = append(out, s.view(r))
		}
	}
	return out, nil
}

func (s *Agents) GetAgent(ctx context.Context, org domain.OrgID, id domain.AgentID) (app.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.find(org, id)
	if err != nil {
		return app.Agent{}, err
	}
	return s.view(r), nil
}

func (s *Agents) ReplaceKey(ctx context.Context, org domain.OrgID, id domain.AgentID, key app.APIKey, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.find(org, id)
	if err != nil || r.agent.Revoked() {
		return app.ErrNotFound
	}
	r.keys = append(r.keys, key)
	r.active = len(r.keys) - 1
	return nil
}

func (s *Agents) RevokeAgent(ctx context.Context, org domain.OrgID, id domain.AgentID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.find(org, id)
	if err != nil {
		return err
	}
	if !r.agent.Revoked() {
		r.agent.RevokedAt = &at
		r.active = -1
	}
	return nil
}

func (s *Agents) ActiveKeyByHash(ctx context.Context, hash string) (app.APIKey, app.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.agents {
		if r.active >= 0 && r.keys[r.active].SecretHash == hash {
			return r.keys[r.active], s.view(r), nil
		}
	}
	return app.APIKey{}, app.Agent{}, app.ErrNotFound
}

func (s *Agents) TouchKey(ctx context.Context, keyID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.agents {
		for i := range r.keys {
			if r.keys[i].ID == keyID {
				r.keys[i].LastUsedAt = &at
			}
		}
	}
	return nil
}

// StoresPlaintext reports whether any stored key holds secret.
func (s *Agents) StoresPlaintext(secret string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.agents {
		for _, k := range r.keys {
			if k.SecretHash == secret || k.Prefix == secret || k.ID == secret {
				return true
			}
		}
	}
	return false
}

// EventLog is an in-memory app.EventLog, scoped by organisation: two
// organisations never share a chain even if they somehow used the
// same key.
type EventLog struct {
	mu     sync.Mutex
	chains map[domain.OrgID]map[domain.IntentKey]domain.Chain
}

func NewEventLog() *EventLog {
	return &EventLog{chains: map[domain.OrgID]map[domain.IntentKey]domain.Chain{}}
}

func (l *EventLog) Append(ctx context.Context, org domain.OrgID, key domain.IntentKey, eventType, payload string) (domain.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.chains[org] == nil {
		l.chains[org] = map[domain.IntentKey]domain.Chain{}
	}
	chain := l.chains[org][key].Append(key, eventType, payload)
	l.chains[org][key] = chain
	return chain[len(chain)-1], nil
}

func (l *EventLog) Load(ctx context.Context, org domain.OrgID, key domain.IntentKey) (domain.Chain, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.chains[org][key], nil
}

// PaymentStarter is an in-memory app.PaymentStarter. Err, if set, is
// returned from every call.
type PaymentStarter struct {
	mu    sync.Mutex
	calls []domain.Intent
	Err   error
}

func (s *PaymentStarter) Start(ctx context.Context, intent domain.Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, intent)
	return s.Err
}

// Calls returns every intent Start was called with, in order.
func (s *PaymentStarter) Calls() []domain.Intent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.Intent(nil), s.calls...)
}

// CallCount returns how many times Start was called.
func (s *PaymentStarter) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}
