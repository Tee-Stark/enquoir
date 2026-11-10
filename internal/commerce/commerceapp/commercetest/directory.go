package commercetest

import (
	"context"
	"sort"
	"sync"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

// record is what customers and agents share: an ID, an owner, an
// external ID unique per merchant, a status and a creation time.
type record[T any] struct {
	id       string
	scope    commerceapp.Scope
	external commerce.ExternalID
	created  time.Time
	value    T
	status   func(*T) *commerce.Status
	disabled func(*T) **time.Time
}

// directory is an in-memory register-or-resolve store, ordered newest
// first, with the last ID seen as the cursor.
type directory[T any] struct {
	mu      sync.Mutex
	records []*record[T]
}

func (d *directory[T]) register(r *record[T]) (T, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, have := range d.records {
		if have.scope.MerchantID == r.scope.MerchantID && have.external == r.external {
			return have.value, false
		}
	}
	d.records = append(d.records, r)
	return r.value, true
}

func (d *directory[T]) find(s commerceapp.Scope, id string) (*record[T], error) {
	for _, r := range d.records {
		if r.id == id && r.scope == s {
			return r, nil
		}
	}
	return nil, app.ErrNotFound
}

func (d *directory[T]) get(s commerceapp.Scope, id string) (T, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.find(s, id)
	if err != nil {
		var zero T
		return zero, err
	}
	return r.value, nil
}

func (d *directory[T]) setStatus(s commerceapp.Scope, id string, status commerce.Status, at time.Time) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r, err := d.find(s, id)
	if err != nil {
		return false, err
	}
	if *r.status(&r.value) == status {
		return false, nil
	}
	*r.status(&r.value) = status
	*r.disabled(&r.value) = nil
	if status == commerce.Disabled {
		*r.disabled(&r.value) = &at
	}
	return true, nil
}

func (d *directory[T]) list(s commerceapp.Scope, f commerceapp.ListFilter) ([]T, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var match []*record[T]
	for _, r := range d.records {
		if r.scope == s && (f.ExternalID == "" || r.external == f.ExternalID) &&
			(f.Status == "" || *r.status(&r.value) == f.Status) {
			match = append(match, r)
		}
	}
	sort.Slice(match, func(i, j int) bool {
		if !match[i].created.Equal(match[j].created) {
			return match[i].created.After(match[j].created)
		}
		return match[i].id > match[j].id
	})
	if f.Cursor != "" {
		for i, r := range match {
			if r.id == f.Cursor {
				match = match[i+1:]
				break
			}
		}
	}
	next := ""
	if len(match) > f.Limit {
		match = match[:f.Limit]
		next = match[f.Limit-1].id
	}
	out := make([]T, len(match))
	for i, r := range match {
		out[i] = r.value
	}
	return out, next
}

var (
	_ commerceapp.CustomerStore = (*Customers)(nil)
	_ commerceapp.AgentStore    = (*Agents)(nil)
)

// Customers is an in-memory commerceapp.CustomerStore.
type Customers struct{ d directory[commerce.Customer] }

func NewCustomers() *Customers { return &Customers{} }

func scopeOf(org, merchant string) commerceapp.Scope {
	return commerceapp.Scope{OrgID: domain.OrgID(org), MerchantID: commerce.MerchantID(merchant)}
}

func (s *Customers) RegisterCustomer(ctx context.Context, c commerce.Customer) (commerce.Customer, bool, error) {
	got, created := s.d.register(&record[commerce.Customer]{
		id: string(c.ID), scope: scopeOf(string(c.OrgID), string(c.MerchantID)), external: c.ExternalID, created: c.CreatedAt, value: c,
		status:   func(c *commerce.Customer) *commerce.Status { return &c.Status },
		disabled: func(c *commerce.Customer) **time.Time { return &c.DisabledAt },
	})
	return got, created, nil
}

func (s *Customers) GetCustomer(ctx context.Context, sc commerceapp.Scope, id commerce.CustomerID) (commerce.Customer, error) {
	return s.d.get(sc, string(id))
}

func (s *Customers) SetCustomerStatus(ctx context.Context, sc commerceapp.Scope, id commerce.CustomerID, status commerce.Status, at time.Time) (bool, error) {
	return s.d.setStatus(sc, string(id), status, at)
}

func (s *Customers) ListCustomers(ctx context.Context, sc commerceapp.Scope, f commerceapp.ListFilter) (commerceapp.CustomerPage, error) {
	out, next := s.d.list(sc, f)
	return commerceapp.CustomerPage{Customers: out, NextCursor: next}, nil
}

// Agents is an in-memory commerceapp.AgentStore.
type Agents struct{ d directory[commerce.Agent] }

func NewAgents() *Agents { return &Agents{} }

func (s *Agents) RegisterAgent(ctx context.Context, a commerce.Agent) (commerce.Agent, bool, error) {
	got, created := s.d.register(&record[commerce.Agent]{
		id: string(a.ID), scope: scopeOf(string(a.OrgID), string(a.MerchantID)), external: a.ExternalID, created: a.CreatedAt, value: a,
		status:   func(a *commerce.Agent) *commerce.Status { return &a.Status },
		disabled: func(a *commerce.Agent) **time.Time { return &a.DisabledAt },
	})
	return got, created, nil
}

func (s *Agents) GetAgent(ctx context.Context, sc commerceapp.Scope, id commerce.AgentID) (commerce.Agent, error) {
	return s.d.get(sc, string(id))
}

func (s *Agents) SetAgentStatus(ctx context.Context, sc commerceapp.Scope, id commerce.AgentID, status commerce.Status, at time.Time) (bool, error) {
	return s.d.setStatus(sc, string(id), status, at)
}

func (s *Agents) ListAgents(ctx context.Context, sc commerceapp.Scope, f commerceapp.ListFilter) (commerceapp.AgentPage, error) {
	out, next := s.d.list(sc, f)
	return commerceapp.AgentPage{Agents: out, NextCursor: next}, nil
}
