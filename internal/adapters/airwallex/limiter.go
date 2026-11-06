package airwallex

import (
	"context"
	"fmt"

	"enquoir/internal/app"
)

// MaxConcurrentRequests is the Airwallex sandbox's documented limit of
// requests in flight at once for one account (AGENTS.md: "10
// concurrent").
const MaxConcurrentRequests = 10

// limiter bounds requests in flight. Exceeding the rail's limit only
// buys 429s, backoff and, if they persist, ambiguous outcomes that send
// payments through UNKNOWN for nothing.
type limiter struct {
	slots chan struct{}
}

func newLimiter(n int) *limiter { return &limiter{slots: make(chan struct{}, n)} }

// defaultLimiter is shared by every Client and Simulator in the
// process: the limit belongs to the Airwallex account, not to either
// type. It bounds one process; several workers together can still
// exceed it.
var defaultLimiter = newLimiter(MaxConcurrentRequests)

// acquire waits for a slot. Giving up while waiting sends nothing, but
// the caller only knows it got no answer, so it is ambiguous like any
// other unanswered call.
func (l *limiter) acquire(ctx context.Context) error {
	select {
	case l.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("airwallex: waiting for a free request slot: %v: %w", ctx.Err(), app.ErrAmbiguous)
	}
}

func (l *limiter) release() { <-l.slots }
