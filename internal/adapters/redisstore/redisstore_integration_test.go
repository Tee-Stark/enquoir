//go:build integration

package redisstore

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"enquoir/internal/adapters/credentials"
)

func client(t *testing.T) *redis.Client {
	t.Helper()
	url := os.Getenv("REDIS_URL")
	if url == "" {
		url = "redis://localhost:6380/0"
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("redis at %s: %v", url, err)
	}
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

func TestCounter_WindowsAreAtomicAndExpire(t *testing.T) {
	ctx := context.Background()
	c := NewCounter(client(t))
	key := credentials.RandomIDs{}.NewID("test")

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Hit(ctx, key, time.Second) }()
	}
	wg.Wait()
	n, left, err := c.Hit(ctx, key, time.Second)
	if err != nil || n != 51 || left <= 0 || left > time.Second {
		t.Errorf("after 51 hits: n=%d left=%v err=%v", n, left, err)
	}
	time.Sleep(1100 * time.Millisecond)
	if n, _, _ := c.Hit(ctx, key, time.Second); n != 1 {
		t.Errorf("after the window: n=%d, want a fresh count", n)
	}

	if err := c.Lock(ctx, key+":lock", time.Hour); err != nil {
		t.Fatal(err)
	}
	if d, _ := c.Locked(ctx, key+":lock"); d < 59*time.Minute {
		t.Errorf("locked for %v, want about an hour", d)
	}
	if d, _ := c.Locked(ctx, key+":nothing"); d != 0 {
		t.Errorf("an unlocked key reads %v", d)
	}
	c.Reset(ctx, key)
	if n, _, _ := c.Hit(ctx, key, time.Second); n != 1 {
		t.Errorf("after reset: %d", n)
	}
}

func TestPrincipalStore_InvalidatingTheOwnerMissesEverywhere(t *testing.T) {
	ctx := context.Background()
	rdb := client(t)
	a, b := NewPrincipalStore(rdb), NewPrincipalStore(rdb) // two API instances
	owner := credentials.RandomIDs{}.NewID("user")
	type v struct{ Role string }

	a.Put(ctx, "ses:1", owner, v{"viewer"}, time.Minute)
	var got v
	if !b.Get(ctx, "ses:1", &got) || got.Role != "viewer" {
		t.Fatalf("another instance missed: %+v", got)
	}
	b.InvalidateOwner(ctx, owner)
	if a.Get(ctx, "ses:1", &got) {
		t.Error("an invalidated value still hits")
	}
	a.Put(ctx, "ses:1", owner, v{"operator"}, time.Minute)
	if !a.Get(ctx, "ses:1", &got) || got.Role != "operator" {
		t.Error("a value cached after invalidation misses")
	}
	a.Forget(ctx, "ses:1")
	if a.Get(ctx, "ses:1", &got) {
		t.Error("a forgotten value hits")
	}
}

func TestUnreachableRedisFailsFastAsAMiss(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond})
	defer rdb.Close()
	start := time.Now()
	var into struct{}
	if NewPrincipalStore(rdb).Get(context.Background(), "x", &into) {
		t.Error("an unreachable Redis hit")
	}
	if _, _, err := NewCounter(rdb).Hit(context.Background(), "x", time.Second); err == nil {
		t.Error("an unreachable Redis counted")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %v; must fail fast", d)
	}
}
