package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"enquoir/internal/domain"
)

// fakeHasher is a transparent, fast stand-in for the slow hasher. It
// records every Verify so tests can check timing-equalising calls.
type fakeHasher struct {
	mu          sync.Mutex
	verifyCalls int
}

func (h *fakeHasher) Hash(pw string) (string, error) { return "hashed:" + pw, nil }

func (h *fakeHasher) Verify(pw, encoded string) (bool, error) {
	h.mu.Lock()
	h.verifyCalls++
	h.mu.Unlock()
	return encoded == "hashed:"+pw, nil
}

type seqIDs struct {
	mu sync.Mutex
	n  int
}

func (g *seqIDs) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("%s_%04d", prefix, g.n)
}

type seqSecrets struct {
	mu sync.Mutex
	n  int
}

func (g *seqSecrets) NewSecret(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("%s_secret%04d", prefix, g.n)
}

type fixedClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFixedClock() *fixedClock {
	return &fixedClock{now: time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)}
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fakeAccounts struct {
	mu    sync.Mutex
	orgs  map[domain.OrgID]Organisation
	users map[domain.UserID]User
}

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{orgs: map[domain.OrgID]Organisation{}, users: map[domain.UserID]User{}}
}

func (a *fakeAccounts) CreateOrganisationWithOwner(ctx context.Context, org Organisation, owner User) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, u := range a.users {
		if u.Email == owner.Email {
			return ErrEmailTaken
		}
	}
	a.orgs[org.ID] = org
	a.users[owner.ID] = owner
	return nil
}

func (a *fakeAccounts) UserByEmail(ctx context.Context, email domain.Email) (User, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, u := range a.users {
		if u.Email == email {
			return u, nil
		}
	}
	return User{}, ErrNotFound
}

func (a *fakeAccounts) UserWithOrganisation(ctx context.Context, id domain.UserID) (User, Organisation, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	u, ok := a.users[id]
	if !ok {
		return User{}, Organisation{}, ErrNotFound
	}
	return u, a.orgs[u.OrgID], nil
}

type fakeSessions struct {
	mu       sync.Mutex
	sessions map[string]Session
}

func newFakeSessions() *fakeSessions { return &fakeSessions{sessions: map[string]Session{}} }

func (s *fakeSessions) Create(ctx context.Context, sess Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.TokenHash] = sess
	return nil
}

func (s *fakeSessions) Get(ctx context.Context, tokenHash string) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[tokenHash]
	if !ok {
		return Session{}, ErrNotFound
	}
	return sess, nil
}

func (s *fakeSessions) Revoke(ctx context.Context, tokenHash string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[tokenHash]; ok {
		sess.RevokedAt = &at
		s.sessions[tokenHash] = sess
	}
	return nil
}

// storesPlaintext reports whether any stored value contains secret.
func (s *fakeSessions) storesPlaintext(secret string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.sessions {
		if strings.Contains(k, secret) {
			return true
		}
	}
	return false
}
