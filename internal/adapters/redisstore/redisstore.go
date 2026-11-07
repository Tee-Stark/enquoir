// Package redisstore implements the rate-limit counter and the
// principal cache on Redis. Redis holds nothing that is a record: a
// lost Redis loses counts and cached lookups, nothing else. Every
// failure is reported as an error (counter) or a miss (cache), and the
// app layer falls back.
package redisstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"enquoir/internal/app"
)

var (
	_ app.Counter        = (*Counter)(nil)
	_ app.PrincipalStore = (*PrincipalStore)(nil)
)

// opTimeout bounds every call: a slow Redis must not slow log-ins.
const opTimeout = 250 * time.Millisecond

func bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, opTimeout)
}

// Counter keeps rate-limit windows and locks.
type Counter struct {
	rdb    *redis.Client
	prefix string
}

func NewCounter(rdb *redis.Client) *Counter { return &Counter{rdb: rdb, prefix: "enq:"} }

// hitScript increments a key and sets its expiry on the first hit, in
// one atomic step, returning the count and the time left in ms.
var hitScript = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then redis.call('PEXPIRE', KEYS[1], ARGV[1]); ttl = tonumber(ARGV[1]) end
return {n, ttl}
`)

func (c *Counter) Hit(ctx context.Context, key string, window time.Duration) (int64, time.Duration, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	res, err := hitScript.Run(ctx, c.rdb, []string{c.prefix + key}, window.Milliseconds()).Int64Slice()
	if err != nil {
		return 0, 0, err
	}
	return res[0], time.Duration(res[1]) * time.Millisecond, nil
}

func (c *Counter) Lock(ctx context.Context, key string, ttl time.Duration) error {
	ctx, cancel := bounded(ctx)
	defer cancel()
	return c.rdb.Set(ctx, c.prefix+key, "1", ttl).Err()
}

func (c *Counter) Locked(ctx context.Context, key string) (time.Duration, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	d, err := c.rdb.PTTL(ctx, c.prefix+key).Result()
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, nil
	}
	return d, nil
}

func (c *Counter) Reset(ctx context.Context, key string) error {
	ctx, cancel := bounded(ctx)
	defer cancel()
	return c.rdb.Del(ctx, c.prefix+key).Err()
}

// PrincipalStore caches resolved sessions and agent keys. Each value
// is stored with its owner's version; invalidating an owner bumps the
// version, so every value it owns misses at once, wherever cached.
type PrincipalStore struct {
	rdb    *redis.Client
	prefix string
}

func NewPrincipalStore(rdb *redis.Client) *PrincipalStore {
	return &PrincipalStore{rdb: rdb, prefix: "enq:pc:"}
}

type entry struct {
	Owner   string          `json:"o"`
	Version int64           `json:"v"`
	Value   json.RawMessage `json:"x"`
}

func (s *PrincipalStore) versionKey(owner string) string { return s.prefix + "ver:" + owner }

func (s *PrincipalStore) Get(ctx context.Context, key string, into any) bool {
	ctx, cancel := bounded(ctx)
	defer cancel()
	raw, err := s.rdb.Get(ctx, s.prefix+key).Bytes()
	if err != nil {
		return false
	}
	var e entry
	if json.Unmarshal(raw, &e) != nil {
		return false
	}
	v, err := s.rdb.Get(ctx, s.versionKey(e.Owner)).Int64()
	if err != nil && err != redis.Nil {
		return false
	}
	if v != e.Version {
		return false
	}
	return json.Unmarshal(e.Value, into) == nil
}

func (s *PrincipalStore) Put(ctx context.Context, key, owner string, value any, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	ctx, cancel := bounded(ctx)
	defer cancel()
	b, err := json.Marshal(value)
	if err != nil {
		return
	}
	v, err := s.rdb.Get(ctx, s.versionKey(owner)).Int64()
	if err != nil && err != redis.Nil {
		return
	}
	e, _ := json.Marshal(entry{Owner: owner, Version: v, Value: b})
	s.rdb.Set(ctx, s.prefix+key, e, ttl)
}

func (s *PrincipalStore) Forget(ctx context.Context, key string) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	s.rdb.Del(ctx, s.prefix+key)
}

// InvalidateOwner bumps owner's version. The version key outlives any
// cached value (a day against a 60-second TTL).
func (s *PrincipalStore) InvalidateOwner(ctx context.Context, owner string) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	pipe := s.rdb.TxPipeline()
	pipe.Incr(ctx, s.versionKey(owner))
	pipe.Expire(ctx, s.versionKey(owner), 24*time.Hour)
	pipe.Exec(ctx)
}
