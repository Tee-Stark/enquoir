package commercetest

import (
	"context"
	"sort"
	"sync"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

var _ commerceapp.MandateStore = (*Mandates)(nil)

// Mandates is an in-memory commerceapp.MandateStore. It reads the
// statuses of what a mandate names from the other fakes, and spends
// consent tokens in Tokens, as the Postgres store does in one
// transaction (here, under one lock).
type Mandates struct {
	mu        sync.Mutex
	requests  map[commerce.MandateRequestID]commerce.MandateRequest
	mandates  map[commerce.MandateID]commerce.Mandate
	consents  []commerce.ConsentRecord
	tokens    app.TokenStore
	customers *Customers
	agents    *Agents
	methods   *FundingMethods
}

func NewMandates(tokens app.TokenStore, customers *Customers, agents *Agents, methods *FundingMethods) *Mandates {
	return &Mandates{requests: map[commerce.MandateRequestID]commerce.MandateRequest{}, mandates: map[commerce.MandateID]commerce.Mandate{},
		tokens: tokens, customers: customers, agents: agents, methods: methods}
}

func scopeOfTerms(t commerce.MandateTerms) commerceapp.Scope {
	return commerceapp.Scope{OrgID: t.Parties.OrgID, MerchantID: t.Parties.MerchantID}
}

func (s *Mandates) CreateRequest(ctx context.Context, r commerce.MandateRequest) (commerce.MandateRequest, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, have := range s.requests {
		if have.Terms.Parties.MerchantID == r.Terms.Parties.MerchantID && have.ExternalID == r.ExternalID {
			return have, false, nil
		}
	}
	s.requests[r.ID] = r
	return r, true, nil
}

func (s *Mandates) GetRequest(ctx context.Context, sc commerceapp.Scope, id commerce.MandateRequestID) (commerce.MandateRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.requests[id]
	if !ok || scopeOfTerms(r.Terms) != sc {
		return commerce.MandateRequest{}, app.ErrNotFound
	}
	return r, nil
}

func (s *Mandates) RequestForConsent(ctx context.Context, id commerce.MandateRequestID) (commerce.MandateRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.requests[id]
	if !ok {
		return commerce.MandateRequest{}, app.ErrNotFound
	}
	return r, nil
}

func (s *Mandates) Decide(ctx context.Context, cmd commerceapp.ConsentDecisionCommand) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.requests[cmd.Record.RequestID]
	if !ok || r.StatusAt(cmd.At) != commerce.RequestPending {
		return app.ErrTokenInvalid
	}
	tok, err := s.tokens.Consume(ctx, cmd.TokenHash, app.PurposeMandateConsent, cmd.At)
	if err != nil || tok.Subject != string(r.ID) {
		return app.ErrTokenInvalid
	}
	at := cmd.At
	r.DecidedAt = &at
	r.Status = commerce.RequestRejected
	if cmd.Mandate != nil {
		r.Status, r.MandateID = commerce.RequestApproved, cmd.Mandate.ID
		s.mandates[cmd.Mandate.ID] = *cmd.Mandate
	}
	s.requests[r.ID] = r
	s.consents = append(s.consents, cmd.Record)
	return nil
}

func (s *Mandates) deps(m commerce.Mandate) commerce.Dependencies {
	sc := scopeOfTerms(m.Terms)
	p := m.Terms.Parties
	c, cerr := s.customers.GetCustomer(context.Background(), sc, p.CustomerID)
	a, aerr := s.agents.GetAgent(context.Background(), sc, p.AgentID)
	f, ferr := s.methods.GetFundingMethod(context.Background(), sc, p.FundingMethodID)
	return commerce.Dependencies{
		IntegrationActive:   true, // the fakes keep integrations elsewhere
		CustomerActive:      cerr == nil && c.Status.IsActive(),
		AgentActive:         aerr == nil && a.Status.IsActive(),
		FundingMethodActive: ferr == nil && f.Status.IsActive(),
	}
}

func (s *Mandates) GetMandate(ctx context.Context, sc commerceapp.Scope, id commerce.MandateID) (commerceapp.MandateRow, error) {
	s.mu.Lock()
	m, ok := s.mandates[id]
	s.mu.Unlock()
	if !ok || scopeOfTerms(m.Terms) != sc {
		return commerceapp.MandateRow{}, app.ErrNotFound
	}
	return commerceapp.MandateRow{Mandate: m, Deps: s.deps(m)}, nil
}

func (s *Mandates) ListMandates(ctx context.Context, sc commerceapp.Scope, f commerceapp.MandateFilter, now time.Time) ([]commerceapp.MandateRow, string, error) {
	s.mu.Lock()
	var all []commerce.Mandate
	for _, m := range s.mandates {
		if scopeOfTerms(m.Terms) == sc && (f.CustomerID == "" || m.Terms.Parties.CustomerID == f.CustomerID) {
			all = append(all, m)
		}
	}
	s.mu.Unlock()
	sort.Slice(all, func(i, j int) bool {
		if !all[i].ApprovedAt.Equal(all[j].ApprovedAt) {
			return all[i].ApprovedAt.After(all[j].ApprovedAt)
		}
		return all[i].ID > all[j].ID
	})
	var rows []commerceapp.MandateRow
	past := f.Cursor == ""
	for _, m := range all {
		if !past {
			past = string(m.ID) == f.Cursor
			continue
		}
		row := commerceapp.MandateRow{Mandate: m, Deps: s.deps(m)}
		if f.Status == "" || m.StatusAt(now, row.Deps) == f.Status {
			rows = append(rows, row)
		}
	}
	next := ""
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		next = string(rows[f.Limit-1].ID)
	}
	return rows, next, nil
}

func (s *Mandates) RequestRevocation(ctx context.Context, sc commerceapp.Scope, id commerce.MandateID, at, expires time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.mandates[id]
	if !ok || scopeOfTerms(m.Terms) != sc {
		return false, app.ErrNotFound
	}
	if m.Revoked || m.RevocationPendingAt(at) {
		return false, nil
	}
	m.RevocationRequestedAt, m.RevocationExpiresAt = &at, &expires
	s.mandates[id] = m
	return true, nil
}

func (s *Mandates) MandateForRevocation(ctx context.Context, id commerce.MandateID) (commerceapp.MandateRow, error) {
	s.mu.Lock()
	m, ok := s.mandates[id]
	s.mu.Unlock()
	if !ok {
		return commerceapp.MandateRow{}, app.ErrNotFound
	}
	return commerceapp.MandateRow{Mandate: m, Deps: s.deps(m)}, nil
}

func (s *Mandates) ConfirmRevocation(ctx context.Context, tokenHash string, id commerce.MandateID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.mandates[id]
	if !ok || !m.RevocationPendingAt(at) {
		return app.ErrTokenInvalid
	}
	tok, err := s.tokens.Consume(ctx, tokenHash, app.PurposeMandateRevocation, at)
	if err != nil || tok.Subject != string(id) {
		return app.ErrTokenInvalid
	}
	m.Revoked, m.RevokedAt = true, &at
	s.mandates[id] = m
	return nil
}

// Consent returns a stored consent record.
func (s *Mandates) Consent(id commerce.ConsentRecordID) (commerce.ConsentRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.consents {
		if c.ID == id {
			return c, true
		}
	}
	return commerce.ConsentRecord{}, false
}

// ConsentsFor returns a request's consent records.
func (s *Mandates) ConsentsFor(id commerce.MandateRequestID) []commerce.ConsentRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []commerce.ConsentRecord
	for _, c := range s.consents {
		if c.RequestID == id {
			out = append(out, c)
		}
	}
	return out
}
