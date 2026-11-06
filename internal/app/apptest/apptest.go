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
