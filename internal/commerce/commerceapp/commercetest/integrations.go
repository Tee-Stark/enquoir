// Package commercetest holds in-memory fakes of the commerce ports, for
// tests in any package. They follow the Postgres adapters' contracts:
// every read and write is scoped, and another tenant's record is
// app.ErrNotFound.
package commercetest

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

var _ commerceapp.IntegrationStore = (*Integrations)(nil)

// Integrations is an in-memory commerceapp.IntegrationStore.
type Integrations struct {
	mu        sync.Mutex
	merchants map[commerce.MerchantID]commerce.Merchant
	keys      []commerceapp.IntegrationKey
	revoked   map[string]bool
	lookups   map[string]int

	// BeforeChange, if set, runs before each change that ends a key's
	// access is applied, outside the lock: a request racing the change.
	BeforeChange func()
}

func NewIntegrations() *Integrations {
	return &Integrations{merchants: map[commerce.MerchantID]commerce.Merchant{}, revoked: map[string]bool{}, lookups: map[string]int{}}
}

func (s *Integrations) beforeChange() {
	if s.BeforeChange != nil {
		s.BeforeChange()
	}
}

func (s *Integrations) CreateIntegration(ctx context.Context, m commerce.Merchant, key commerceapp.IntegrationKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.merchants[m.ID] = m
	s.keys = append(s.keys, key)
	return nil
}

// activeKey is the merchant's unrevoked key, if any. Callers hold mu.
func (s *Integrations) activeKey(id commerce.MerchantID) *commerceapp.IntegrationKey {
	for i := range s.keys {
		if s.keys[i].MerchantID == id && !s.revoked[s.keys[i].ID] {
			k := s.keys[i]
			return &k
		}
	}
	return nil
}

func (s *Integrations) ListIntegrations(ctx context.Context, org domain.OrgID) ([]commerceapp.Integration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []commerceapp.Integration
	for _, m := range s.merchants {
		if m.OrgID == org {
			out = append(out, commerceapp.Integration{Merchant: m, Key: s.activeKey(m.ID)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Integrations) GetIntegration(ctx context.Context, org domain.OrgID, id commerce.MerchantID) (commerceapp.Integration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.merchants[id]
	if !ok || m.OrgID != org {
		return commerceapp.Integration{}, app.ErrNotFound
	}
	return commerceapp.Integration{Merchant: m, Key: s.activeKey(id)}, nil
}

func (s *Integrations) owned(org domain.OrgID, id commerce.MerchantID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.merchants[id]
	return ok && m.OrgID == org
}

func (s *Integrations) SetIntegrationStatus(ctx context.Context, org domain.OrgID, id commerce.MerchantID, status commerce.Status, at time.Time) (bool, error) {
	if !s.owned(org, id) {
		return false, app.ErrNotFound
	}
	s.beforeChange()
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.merchants[id]
	if m.Status == status {
		return false, nil
	}
	m.Status, m.DisabledAt = status, nil
	if status == commerce.Disabled {
		m.DisabledAt = &at
	}
	s.merchants[id] = m
	return true, nil
}

func (s *Integrations) ReplaceKey(ctx context.Context, org domain.OrgID, id commerce.MerchantID, key commerceapp.IntegrationKey, at time.Time) error {
	if !s.owned(org, id) {
		return app.ErrNotFound
	}
	s.beforeChange()
	s.mu.Lock()
	defer s.mu.Unlock()
	if k := s.activeKey(id); k != nil {
		s.revoked[k.ID] = true
	}
	s.keys = append(s.keys, key)
	return nil
}

func (s *Integrations) RevokeKey(ctx context.Context, org domain.OrgID, id commerce.MerchantID, at time.Time) (bool, error) {
	if !s.owned(org, id) {
		return false, app.ErrNotFound
	}
	s.beforeChange()
	s.mu.Lock()
	defer s.mu.Unlock()
	k := s.activeKey(id)
	if k == nil {
		return false, nil
	}
	s.revoked[k.ID] = true
	return true, nil
}

func (s *Integrations) ActiveKeyByHash(ctx context.Context, hash string) (commerceapp.IntegrationKey, commerce.Merchant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lookups[hash]++
	for _, k := range s.keys {
		if k.SecretHash == hash && !s.revoked[k.ID] {
			if m := s.merchants[k.MerchantID]; m.Status.IsActive() {
				return k, m, nil
			}
		}
	}
	return commerceapp.IntegrationKey{}, commerce.Merchant{}, app.ErrNotFound
}

func (s *Integrations) TouchKey(ctx context.Context, keyID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.keys {
		if s.keys[i].ID == keyID {
			s.keys[i].LastUsedAt = &at
		}
	}
	return nil
}

// LookupsOf counts how often a secret's hash was looked up.
func (s *Integrations) LookupsOf(secret string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookups[app.HashSecret(secret)]
}

// StoresPlaintext reports whether any stored field holds secret.
func (s *Integrations) StoresPlaintext(secret string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, k := range s.keys {
		if strings.Contains(k.ID+k.Prefix+k.SecretHash, secret) {
			return true
		}
	}
	return false
}
