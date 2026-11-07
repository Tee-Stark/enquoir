package redisstore

import (
	"context"
	"testing"
	"time"
)

func TestMemoryCounter(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	c := NewMemoryCounter()
	c.now = func() time.Time { return now }
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if n, _, _ := c.Hit(ctx, "k", time.Minute); n != int64(i) {
			t.Fatalf("hit %d: %d", i, n)
		}
	}
	now = now.Add(time.Minute)
	if n, _, _ := c.Hit(ctx, "k", time.Minute); n != 1 {
		t.Errorf("after the window: %d", n)
	}
	c.Lock(ctx, "l", time.Hour)
	if d, _ := c.Locked(ctx, "l"); d != time.Hour {
		t.Errorf("locked %v", d)
	}
	now = now.Add(time.Hour)
	if d, _ := c.Locked(ctx, "l"); d != 0 {
		t.Errorf("expired lock reads %v", d)
	}
}
