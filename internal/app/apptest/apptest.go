// Package apptest provides in-memory implementations of the app ports,
// for tests in any layer. They behave like the real stores in the ways
// tests depend on (uniqueness, not-found, revocation) and nothing more.
package apptest

import (
	"context"
	"encoding/json"
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

// Accounts is an in-memory app.AccountStore with memberships.
type Accounts struct {
	mu          sync.Mutex
	orgs        map[domain.OrgID]app.Organisation
	users       map[domain.UserID]app.User
	memberships []app.Membership
}

func NewAccounts() *Accounts {
	return &Accounts{orgs: map[domain.OrgID]app.Organisation{}, users: map[domain.UserID]app.User{}}
}

func (a *Accounts) emailTaken(email domain.Email) bool {
	for _, u := range a.users {
		if u.Email == email {
			return true
		}
	}
	return false
}

func (a *Accounts) CreateOrganisationWithOwner(ctx context.Context, org app.Organisation, owner app.User) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.emailTaken(owner.Email) {
		return app.ErrEmailTaken
	}
	a.orgs[org.ID] = org
	a.users[owner.ID] = owner
	a.memberships = append(a.memberships, app.Membership{Organisation: org, UserID: owner.ID, Role: domain.Owner, CreatedAt: owner.CreatedAt})
	return nil
}

// AddMember adds user (who must exist) to org with role.
func (a *Accounts) AddMember(org domain.OrgID, user domain.UserID, role domain.Role, at time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.memberships = append(a.memberships, app.Membership{Organisation: a.orgs[org], UserID: user, Role: role, CreatedAt: at})
}

// RemoveMembershipForTest drops a membership directly.
func (a *Accounts) RemoveMembershipForTest(org domain.OrgID, user domain.UserID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, m := range a.memberships {
		if m.Organisation.ID == org && m.UserID == user {
			a.memberships = append(a.memberships[:i], a.memberships[i+1:]...)
			return
		}
	}
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

func (a *Accounts) UserByID(ctx context.Context, id domain.UserID) (app.User, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[id]
	if !ok {
		return app.User{}, app.ErrNotFound
	}
	return u, nil
}

func (a *Accounts) Membership(ctx context.Context, org domain.OrgID, user domain.UserID) (app.Membership, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, m := range a.memberships {
		if m.Organisation.ID == org && m.UserID == user {
			m.Organisation = a.orgs[org]
			return m, nil
		}
	}
	return app.Membership{}, app.ErrNotFound
}

func (a *Accounts) MembershipsOf(ctx context.Context, user domain.UserID) ([]app.Membership, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []app.Membership
	for _, m := range a.memberships {
		if m.UserID == user {
			m.Organisation = a.orgs[m.Organisation.ID]
			out = append(out, m)
		}
	}
	return out, nil
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
	// Clock stamps each event's RecordedAt, as the database does; the
	// wall clock if nil.
	Clock app.Clock
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
	if l.Clock != nil {
		chain[len(chain)-1].RecordedAt = l.Clock.Now()
	} else {
		chain[len(chain)-1].RecordedAt = time.Now()
	}
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

func (s *Beneficiaries) List(ctx context.Context, org domain.OrgID) ([]app.RegisteredBeneficiary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []app.RegisteredBeneficiary
	for _, b := range s.saved {
		if b.OrgID == org {
			out = append(out, b)
		}
	}
	return out, nil
}

// Payments is an in-memory app.PaymentReader over seeded payments.
type Payments struct {
	mu       sync.Mutex
	payments []app.Payment
	// LastFilter is the filter the last ListPayments call received.
	LastFilter app.PaymentFilter
}

func NewPayments(ps ...app.Payment) *Payments { return &Payments{payments: ps} }

func (r *Payments) Add(p app.Payment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payments = append(r.payments, p)
}

func (r *Payments) ListPayments(ctx context.Context, org domain.OrgID, f app.PaymentFilter) (app.PaymentPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.LastFilter = f
	var out []app.Payment
	for _, p := range r.payments {
		if p.OrgID == org {
			out = append(out, p)
		}
	}
	return app.PaymentPage{Payments: out}, nil
}

func (r *Payments) GetPayment(ctx context.Context, org domain.OrgID, key domain.IntentKey) (app.Payment, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.payments {
		if p.OrgID == org && p.Key == key {
			return p, nil
		}
	}
	return app.Payment{}, app.ErrNotFound
}

// Outbox is an in-memory app.EmailOutbox.
type Outbox struct {
	mu     sync.Mutex
	emails map[string]app.Email
	order  []string
}

func NewOutbox() *Outbox { return &Outbox{emails: map[string]app.Email{}} }

func (o *Outbox) Queue(ctx context.Context, e app.Email) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.emails[e.ID] = e
	o.order = append(o.order, e.ID)
	return nil
}

func (o *Outbox) Get(ctx context.Context, id string) (app.Email, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	e, ok := o.emails[id]
	if !ok {
		return app.Email{}, app.ErrNotFound
	}
	return e, nil
}

func (o *Outbox) MarkSent(ctx context.Context, id, providerID string, at time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	e := o.emails[id]
	e.Status, e.ProviderID, e.SentAt = app.EmailSent, providerID, &at
	o.emails[id] = e
	return nil
}

func (o *Outbox) MarkFailed(ctx context.Context, id, reason string, at time.Time) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	e := o.emails[id]
	e.Status, e.Error = app.EmailFailed, reason
	o.emails[id] = e
	return nil
}

// All returns every queued email, oldest first.
func (o *Outbox) All() []app.Email {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]app.Email, len(o.order))
	for i, id := range o.order {
		out[i] = o.emails[id]
	}
	return out
}

// Dispatcher records dispatched email IDs. Err, if set, is returned.
type Dispatcher struct {
	mu  sync.Mutex
	ids []string
	Err error
	// Deliver, if set, is run for each dispatch: a synchronous stand-in
	// for the durable send.
	Deliver func(ctx context.Context, id string) error
}

func (d *Dispatcher) Dispatch(ctx context.Context, id string) error {
	d.mu.Lock()
	d.ids = append(d.ids, id)
	d.mu.Unlock()
	if d.Err != nil {
		return d.Err
	}
	if d.Deliver != nil {
		return d.Deliver(ctx, id)
	}
	return nil
}

func (d *Dispatcher) Dispatched() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.ids...)
}

// Mailer records sent emails. Err, if set, is returned instead.
type Mailer struct {
	mu   sync.Mutex
	sent []app.OutgoingEmail
	n    int
	Err  error
}

func (m *Mailer) Send(ctx context.Context, e app.OutgoingEmail) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return "", m.Err
	}
	m.sent = append(m.sent, e)
	m.n++
	return fmt.Sprintf("msg_%d", m.n), nil
}

func (m *Mailer) Sent() []app.OutgoingEmail {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]app.OutgoingEmail(nil), m.sent...)
}

// LastTo returns the link in the last email sent to addr.
func (m *Mailer) LastLinkTo(addr domain.Email) string {
	sent := m.Sent()
	for i := len(sent) - 1; i >= 0; i-- {
		if sent[i].To == addr {
			return LinkIn(sent[i].Text)
		}
	}
	return ""
}

// TokenFrom is the token in the last link sent to addr.
func (m *Mailer) TokenTo(addr domain.Email) string {
	link := m.LastLinkTo(addr)
	if i := strings.Index(link, "token="); i >= 0 {
		return link[i+len("token="):]
	}
	return ""
}

// LinkIn returns the first URL with a token in text.
func LinkIn(text string) string {
	for _, f := range strings.Fields(text) {
		if strings.Contains(f, "token=") {
			return f
		}
	}
	return ""
}

// Renderer renders any template as its params, one per line, so tests
// can read them back.
type Renderer struct{}

func (Renderer) Render(id app.TemplateID, params map[string]string) (app.RenderedEmail, error) {
	var b strings.Builder
	for k, v := range params {
		fmt.Fprintf(&b, "%s: %s\n", k, v)
	}
	if params["link"] != "" {
		b.WriteString(params["link"] + "\n")
	}
	return app.RenderedEmail{Subject: string(id), HTML: "<p>" + string(id) + "</p>", Text: b.String()}, nil
}

// Tokens is an in-memory app.TokenStore.
type Tokens struct {
	mu     sync.Mutex
	tokens map[string]app.OneTimeToken
}

func NewTokens() *Tokens { return &Tokens{tokens: map[string]app.OneTimeToken{}} }

func (s *Tokens) Create(ctx context.Context, t app.OneTimeToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[t.Hash] = t
	return nil
}

func (s *Tokens) Consume(ctx context.Context, hash string, purpose app.TokenPurpose, now time.Time) (app.OneTimeToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[hash]
	if !ok || t.Purpose != purpose || t.UsedAt != nil || !now.Before(t.ExpiresAt) {
		return app.OneTimeToken{}, app.ErrTokenInvalid
	}
	for h, other := range s.tokens {
		if other.Purpose == t.Purpose && other.Subject == t.Subject && other.UsedAt == nil {
			other.UsedAt = &now
			s.tokens[h] = other
		}
	}
	t.UsedAt = &now
	return t, nil
}

func (s *Tokens) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens)
}

// StoresPlaintext reports whether any stored token hash is token.
func (s *Tokens) StoresPlaintext(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.tokens[token]
	return ok
}

// MarkEmailVerified and SetPassword make Accounts an app.UserStore.
func (a *Accounts) MarkEmailVerified(ctx context.Context, user domain.UserID, at time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[user]
	if !ok {
		return app.ErrNotFound
	}
	u.EmailVerified = true
	a.users[user] = u
	return nil
}

func (a *Accounts) SetPassword(ctx context.Context, user domain.UserID, hash string, at time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[user]
	if !ok {
		return app.ErrNotFound
	}
	u.PasswordHash = hash
	a.users[user] = u
	return nil
}

func (s *Sessions) RevokeAllForUser(ctx context.Context, user domain.UserID, except string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, sess := range s.sessions {
		if sess.UserID == user && sess.ID != except && sess.RevokedAt == nil {
			sess.RevokedAt = &at
			s.sessions[h] = sess
		}
	}
	return nil
}

func (s *Tokens) Peek(ctx context.Context, hash string, purpose app.TokenPurpose, now time.Time) (app.OneTimeToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tokens[hash]
	if !ok || t.Purpose != purpose || t.UsedAt != nil || !now.Before(t.ExpiresAt) {
		return app.OneTimeToken{}, app.ErrTokenInvalid
	}
	return t, nil
}

func (s *Sessions) RevokeForMember(ctx context.Context, org domain.OrgID, user domain.UserID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, sess := range s.sessions {
		if sess.UserID == user && sess.OrgID == org && sess.RevokedAt == nil {
			sess.RevokedAt = &at
			s.sessions[h] = sess
		}
	}
	return nil
}

func (a *Accounts) owners(org domain.OrgID) int {
	n := 0
	for _, m := range a.memberships {
		if m.Organisation.ID == org && m.Role == domain.Owner {
			n++
		}
	}
	return n
}

func (a *Accounts) ListMembers(ctx context.Context, org domain.OrgID) ([]app.Member, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []app.Member
	for _, m := range a.memberships {
		if m.Organisation.ID == org {
			out = append(out, app.Member{User: a.users[m.UserID], Role: m.Role, JoinedAt: m.CreatedAt})
		}
	}
	return out, nil
}

func (a *Accounts) ChangeRole(ctx context.Context, org domain.OrgID, user domain.UserID, to domain.Role, check app.MembershipCheck) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, m := range a.memberships {
		if m.Organisation.ID == org && m.UserID == user {
			if err := check(m.Role, a.owners(org)); err != nil {
				return err
			}
			a.memberships[i].Role = to
			return nil
		}
	}
	return app.ErrNotFound
}

func (a *Accounts) RemoveMember(ctx context.Context, org domain.OrgID, user domain.UserID, check app.MembershipCheck) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i, m := range a.memberships {
		if m.Organisation.ID == org && m.UserID == user {
			if err := check(m.Role, a.owners(org)); err != nil {
				return err
			}
			a.memberships = append(a.memberships[:i], a.memberships[i+1:]...)
			return nil
		}
	}
	return app.ErrNotFound
}

// Invitations is an in-memory app.InvitationStore, joined to an
// Accounts for acceptance.
type Invitations struct {
	mu       sync.Mutex
	invs     map[string]app.Invitation
	Accounts *Accounts
}

func NewInvitations() *Invitations { return &Invitations{invs: map[string]app.Invitation{}} }

func (s *Invitations) CreateInvitation(ctx context.Context, inv app.Invitation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, other := range s.invs {
		if other.OrgID == inv.OrgID && other.Email == inv.Email && other.Pending(inv.CreatedAt) {
			at := inv.CreatedAt
			other.RevokedAt = &at
			s.invs[id] = other
		}
	}
	s.invs[inv.ID] = inv
	return nil
}

func (s *Invitations) ListPendingInvitations(ctx context.Context, org domain.OrgID, now time.Time) ([]app.Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []app.Invitation
	for _, inv := range s.invs {
		if inv.OrgID == org && inv.Pending(now) {
			out = append(out, inv)
		}
	}
	return out, nil
}

func (s *Invitations) GetInvitation(ctx context.Context, id string) (app.Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.invs[id]
	if !ok {
		return app.Invitation{}, app.ErrNotFound
	}
	return inv, nil
}

func (s *Invitations) RevokeInvitation(ctx context.Context, org domain.OrgID, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.invs[id]
	if !ok || inv.OrgID != org || !inv.Pending(at) {
		return app.ErrNotFound
	}
	inv.RevokedAt = &at
	s.invs[id] = inv
	return nil
}

func (s *Invitations) AcceptInvitation(ctx context.Context, id string, user domain.UserID, newUser *app.User, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.invs[id]
	if !ok || !inv.Pending(at) {
		return app.ErrTokenInvalid
	}
	if _, err := s.Accounts.Membership(ctx, inv.OrgID, user); err == nil {
		return app.ErrAlreadyMember
	}
	if newUser != nil {
		s.Accounts.mu.Lock()
		if s.Accounts.emailTaken(newUser.Email) {
			s.Accounts.mu.Unlock()
			return app.ErrEmailTaken
		}
		s.Accounts.users[newUser.ID] = *newUser
		s.Accounts.mu.Unlock()
	}
	s.Accounts.AddMember(inv.OrgID, user, inv.Role, at)
	inv.AcceptedAt = &at
	s.invs[id] = inv
	return nil
}

// Counter is an in-memory app.Counter driven by a Clock.
type Counter struct {
	mu    sync.Mutex
	Clock *Clock
	hits  map[string]counterEntry
	locks map[string]time.Time
	// Err, if set, fails every call: a counter that is down.
	Err error
}

type counterEntry struct {
	n     int64
	until time.Time
}

func NewCounter(c *Clock) *Counter {
	return &Counter{Clock: c, hits: map[string]counterEntry{}, locks: map[string]time.Time{}}
}

func (c *Counter) Hit(ctx context.Context, key string, window time.Duration) (int64, time.Duration, error) {
	if c.Err != nil {
		return 0, 0, c.Err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.Clock.Now()
	e := c.hits[key]
	if !now.Before(e.until) {
		e = counterEntry{until: now.Add(window)}
	}
	e.n++
	c.hits[key] = e
	return e.n, e.until.Sub(now), nil
}

func (c *Counter) Lock(ctx context.Context, key string, ttl time.Duration) error {
	if c.Err != nil {
		return c.Err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.locks[key] = c.Clock.Now().Add(ttl)
	return nil
}

func (c *Counter) Locked(ctx context.Context, key string) (time.Duration, error) {
	if c.Err != nil {
		return 0, c.Err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if left := c.locks[key].Sub(c.Clock.Now()); left > 0 {
		return left, nil
	}
	return 0, nil
}

func (c *Counter) Reset(ctx context.Context, key string) error {
	if c.Err != nil {
		return c.Err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.hits, key)
	delete(c.locks, key)
	return nil
}

// PrincipalStore is an in-memory app.PrincipalStore with owner
// versions, like the Redis one. Gets counts cache reads that hit.
type PrincipalStore struct {
	mu       sync.Mutex
	entries  map[string]principalEntry
	versions map[string]int
	Hits     int
}

type principalEntry struct {
	owner   string
	version int
	value   []byte
}

func NewPrincipalStore() *PrincipalStore {
	return &PrincipalStore{entries: map[string]principalEntry{}, versions: map[string]int{}}
}

func (s *PrincipalStore) Get(ctx context.Context, key string, into any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	if !ok || e.version != s.versions[e.owner] {
		return false
	}
	if json.Unmarshal(e.value, into) != nil {
		return false
	}
	s.Hits++
	return true
}

func (s *PrincipalStore) Put(ctx context.Context, key, owner string, value any, ttl time.Duration) {
	b, err := json.Marshal(value)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = principalEntry{owner: owner, version: s.versions[owner], value: b}
}

func (s *PrincipalStore) Forget(ctx context.Context, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
}

func (s *PrincipalStore) InvalidateOwner(ctx context.Context, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.versions[owner]++
}

// Contains reports whether any cached value contains needle.
func (s *PrincipalStore) Contains(needle string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if strings.Contains(string(e.value), needle) {
			return true
		}
	}
	return false
}

// Orgs lets Sessions name organisations when listing.
func (s *Sessions) ActiveForUser(ctx context.Context, user domain.UserID, now time.Time) ([]app.SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []app.SessionInfo
	for _, sess := range s.sessions {
		if sess.UserID == user && sess.RevokedAt == nil && now.Before(sess.ExpiresAt) {
			out = append(out, app.SessionInfo{ID: sess.ID, Organisation: app.Organisation{ID: sess.OrgID}, CreatedAt: sess.CreatedAt, ExpiresAt: sess.ExpiresAt})
		}
	}
	return out, nil
}

func (s *Sessions) RevokeByID(ctx context.Context, user domain.UserID, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, sess := range s.sessions {
		if sess.ID == id && sess.UserID == user && sess.RevokedAt == nil {
			sess.RevokedAt = &at
			s.sessions[h] = sess
			return nil
		}
	}
	return app.ErrNotFound
}
