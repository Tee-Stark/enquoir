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

// Beneficiaries is an in-memory app.BeneficiaryStore.
type Beneficiaries struct {
	mu    sync.Mutex
	saved map[domain.BeneficiaryKey]app.RegisteredBeneficiary
}

func NewBeneficiaries() *Beneficiaries {
	return &Beneficiaries{saved: map[domain.BeneficiaryKey]app.RegisteredBeneficiary{}}
}

// Own seeds that org owns railID, for tests that need a payee. Unlike
// Save it skips the one-owner check, so tests can give several
// organisations a payee with the same literal ID.
func (s *Beneficiaries) Own(org domain.OrgID, railID string) *Beneficiaries {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := domain.BeneficiaryKey(string(org) + "/" + railID)
	s.saved[key] = app.RegisteredBeneficiary{OrgID: org, Key: key, RailID: railID}
	return s
}

func (s *Beneficiaries) Save(ctx context.Context, b app.RegisteredBeneficiary) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.saved {
		if existing.RailID == b.RailID && existing.OrgID != b.OrgID {
			return fmt.Errorf("rail beneficiary %s is already owned by another organisation", b.RailID)
		}
	}
	if _, ok := s.saved[b.Key]; !ok {
		s.saved[b.Key] = b
	}
	return nil
}

func (s *Beneficiaries) Owns(ctx context.Context, org domain.OrgID, railID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.saved {
		if b.OrgID == org && b.RailID == railID {
			return true, nil
		}
	}
	return false, nil
}

// Get returns what was saved under key.
func (s *Beneficiaries) Get(key domain.BeneficiaryKey) (app.RegisteredBeneficiary, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.saved[key]
	return b, ok
}

// Intents is an in-memory app.IntentRepository.
type Intents struct {
	mu     sync.Mutex
	stored map[domain.IntentKey]domain.Intent
}

func NewIntents() *Intents { return &Intents{stored: map[domain.IntentKey]domain.Intent{}} }

func (r *Intents) Save(ctx context.Context, intent domain.Intent) (domain.Intent, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.stored[intent.Key]; ok {
		return existing, false, nil
	}
	r.stored[intent.Key] = intent
	return intent, true, nil
}

// Get returns the stored intent for key.
func (r *Intents) Get(key domain.IntentKey) (domain.Intent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i, ok := r.stored[key]
	return i, ok
}

// Attempts is an in-memory app.AttemptLog.
type Attempts struct {
	mu       sync.Mutex
	attempts []app.Attempt
	Clock    app.Clock // for interrupted; NewClock() if nil
}

func NewAttempts() *Attempts { return &Attempts{} }

func (l *Attempts) Record(ctx context.Context, a app.Attempt) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts = append(l.attempts, a)
	return nil
}

func (l *Attempts) Start(ctx context.Context, a app.Attempt) error {
	a.Outcome = app.OutcomeInProgress
	return l.Record(ctx, a)
}

func (l *Attempts) Finish(ctx context.Context, org domain.OrgID, id string, f app.AttemptFinish) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.attempts {
		a := &l.attempts[i]
		if a.ID == id && a.OrgID == org {
			a.Outcome, a.TransferID, a.TransferStatus = f.Outcome, f.TransferID, f.TransferStatus
			a.HTTPStatus, a.ErrorCode, a.ErrorMessage = f.HTTPStatus, f.ErrorCode, f.ErrorMessage
			at := f.FinishedAt
			a.FinishedAt = &at
			return nil
		}
	}
	return app.ErrNotFound
}

func (l *Attempts) List(ctx context.Context, org domain.OrgID, f app.AttemptFilter) (app.AttemptPage, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []app.Attempt
	for i := len(l.attempts) - 1; i >= 0; i-- {
		a := l.attempts[i]
		if a.OrgID != org || (f.Kind != "" && a.Kind != f.Kind) || (f.IntentKey != "" && a.IntentKey != f.IntentKey) ||
			(f.Operation != "" && a.Operation != f.Operation) || (f.AgentID != "" && a.AgentID != f.AgentID) {
			continue
		}
		out = append(out, a)
	}
	return app.AttemptPage{Attempts: out}, nil
}

// All returns every recorded attempt, oldest first.
func (l *Attempts) All() []app.Attempt {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]app.Attempt(nil), l.attempts...)
}
