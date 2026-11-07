package app

import (
	"context"
	"time"

	"enquoir/internal/domain"
)

// Ordering rule: every change that ends access is committed to the
// store first, and the cache cleared after. Cleared first, a request
// racing the change could re-cache the old access as still valid.

// PrincipalCacheTTL bounds how long a resolved session or agent key is
// served from the cache. Every change that affects access invalidates
// it at once; the TTL only bounds a missed invalidation.
const PrincipalCacheTTL = 60 * time.Second

// PrincipalStore caches small values by key. Each value belongs to an
// owner (a user or an agent), recorded with it; invalidating the owner
// makes every value it owns miss. Failures are misses: the caller goes
// to the record.
type PrincipalStore interface {
	Get(ctx context.Context, key string, into any) bool
	Put(ctx context.Context, key, owner string, value any, ttl time.Duration)
	Forget(ctx context.Context, key string)
	InvalidateOwner(ctx context.Context, owner string)
}

// Cache is the principal cache as the use cases see it. A nil *Cache,
// or one with no Store, caches nothing.
type Cache struct{ Store PrincipalStore }

func (c *Cache) on() bool { return c != nil && c.Store != nil }

func userOwner(id domain.UserID) string   { return "user:" + string(id) }
func agentOwner(id domain.AgentID) string { return "agent:" + string(id) }
func sessionKey(tokenHash string) string  { return "ses:" + tokenHash }
func agentKeyKey(keyHash string) string   { return "agk:" + keyHash }

func (c *Cache) session(ctx context.Context, hash string, now time.Time) (UserPrincipal, bool) {
	if !c.on() {
		return UserPrincipal{}, false
	}
	var p UserPrincipal
	if !c.Store.Get(ctx, sessionKey(hash), &p) || p.User.ID == "" || !now.Before(p.ExpiresAt) {
		return UserPrincipal{}, false
	}
	return p, true
}

func (c *Cache) putSession(ctx context.Context, hash string, p UserPrincipal, now time.Time) {
	if !c.on() {
		return
	}
	ttl := PrincipalCacheTTL
	if left := p.ExpiresAt.Sub(now); left < ttl {
		ttl = left
	}
	p.User.PasswordHash = "" // never cached
	c.Store.Put(ctx, sessionKey(hash), userOwner(p.User.ID), p, ttl)
}

func (c *Cache) agent(ctx context.Context, hash string) (AgentPrincipal, bool) {
	if !c.on() {
		return AgentPrincipal{}, false
	}
	var p AgentPrincipal
	if !c.Store.Get(ctx, agentKeyKey(hash), &p) || p.AgentID == "" {
		return AgentPrincipal{}, false
	}
	return p, true
}

func (c *Cache) putAgent(ctx context.Context, hash string, p AgentPrincipal) {
	if c.on() {
		c.Store.Put(ctx, agentKeyKey(hash), agentOwner(p.AgentID), p, PrincipalCacheTTL)
	}
}

// ForgetSession drops one session's entry (log-out, switching away).
func (c *Cache) ForgetSession(ctx context.Context, token string) {
	if c.on() {
		c.Store.Forget(ctx, sessionKey(HashSecret(token)))
	}
}

// InvalidateUser drops every cached session of user.
func (c *Cache) InvalidateUser(ctx context.Context, user domain.UserID) {
	if c.on() {
		c.Store.InvalidateOwner(ctx, userOwner(user))
	}
}

// InvalidateAgent drops every cached key of agent.
func (c *Cache) InvalidateAgent(ctx context.Context, agent domain.AgentID) {
	if c.on() {
		c.Store.InvalidateOwner(ctx, agentOwner(agent))
	}
}
