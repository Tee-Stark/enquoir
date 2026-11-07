package redisstore

import (
	"context"
	"sync"
	"time"

	"enquoir/internal/app"
)

var _ app.Counter = (*MemoryCounter)(nil)

// MemoryCounter is a per-process app.Counter, for when there is no
// Redis: limits still hold, per instance rather than across them.
type MemoryCounter struct {
	mu    sync.Mutex
	hits  map[string]memEntry
	locks map[string]time.Time
	now   func() time.Time
}

type memEntry struct {
	n     int64
	until time.Time
}

func NewMemoryCounter() *MemoryCounter {
	return &MemoryCounter{hits: map[string]memEntry{}, locks: map[string]time.Time{}, now: time.Now}
}

func (c *MemoryCounter) Hit(ctx context.Context, key string, window time.Duration) (int64, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.hits) > 100_000 {
		c.sweep(now)
	}
	e := c.hits[key]
	if !now.Before(e.until) {
		e = memEntry{until: now.Add(window)}
	}
	e.n++
	c.hits[key] = e
	return e.n, e.until.Sub(now), nil
}

func (c *MemoryCounter) sweep(now time.Time) {
	for k, e := range c.hits {
		if !now.Before(e.until) {
			delete(c.hits, k)
		}
	}
}

func (c *MemoryCounter) Lock(ctx context.Context, key string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.locks[key] = c.now().Add(ttl)
	return nil
}

func (c *MemoryCounter) Locked(ctx context.Context, key string) (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if left := c.locks[key].Sub(c.now()); left > 0 {
		return left, nil
	}
	delete(c.locks, key)
	return 0, nil
}

func (c *MemoryCounter) Reset(ctx context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.hits, key)
	delete(c.locks, key)
	return nil
}
