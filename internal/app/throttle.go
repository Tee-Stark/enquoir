package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"enquoir/internal/domain"
)

// Counter keeps short-lived counts and locks: rate limits and the
// log-in lockout. Implementations may be shared across API instances
// (Redis); none of this is a record.
type Counter interface {
	// Hit adds one to key, opening a window of length window at the
	// first hit, and returns the count in the window and when it ends.
	Hit(ctx context.Context, key string, window time.Duration) (count int64, resetIn time.Duration, err error)
	// Lock holds key for ttl; Locked reports how long is left (0: not locked).
	Lock(ctx context.Context, key string, ttl time.Duration) error
	Locked(ctx context.Context, key string) (time.Duration, error)
	// Reset forgets key, whether a count or a lock.
	Reset(ctx context.Context, key string) error
}

// Rule is one rate limit: at most Limit hits per Window.
type Rule struct {
	Name   string
	Limit  int64
	Window time.Duration
}

// The agreed limits.
var (
	RuleLoginPerEmail   = Rule{"login-email", 5, time.Minute}
	RuleLoginPerIP      = Rule{"login-ip", 30, time.Minute}
	RuleSignUpPerIP     = Rule{"signup-ip", 5, time.Hour}
	RuleResendPerMinute = Rule{"verify-resend-minute", 1, time.Minute}
	RuleResendPerDay    = Rule{"verify-resend-day", 5, 24 * time.Hour}
	RuleForgotPerEmail  = Rule{"forgot-email", 3, time.Hour}
)

// Lockout: this many failed log-ins within LockoutWindow lock the
// email for LockoutDuration.
const (
	LockoutThreshold = 10
	LockoutWindow    = time.Hour
	LockoutDuration  = time.Hour
)

// RateLimitedError means a rule's limit was reached.
type RateLimitedError struct {
	Rule       string
	RetryAfter time.Duration
}

func (e *RateLimitedError) Error() string {
	return fmt.Sprintf("too many attempts; try again in %s", e.RetryAfter.Round(time.Second))
}

// ErrAccountLocked is the log-in lockout, as an error to branch on.
var ErrAccountLocked = errors.New("too many failed log-ins: this account is locked")

// AccountLockedError carries how long the lock has left.
type AccountLockedError struct{ RetryAfter time.Duration }

func (e *AccountLockedError) Error() string {
	return fmt.Sprintf("%s for %s", ErrAccountLocked, e.RetryAfter.Round(time.Minute))
}
func (e *AccountLockedError) Unwrap() error { return ErrAccountLocked }

// Throttle applies rules. A nil Throttle allows everything (tooling,
// tests that are not about limits).
type Throttle struct{ Counter Counter }

// Check records a hit for subject under rule and refuses it past the
// limit. A counter that fails allows the hit: rate limiting must not
// take authentication down with it.
func (t *Throttle) Check(ctx context.Context, rule Rule, subject string) error {
	if t == nil || t.Counter == nil {
		return nil
	}
	n, resetIn, err := t.Counter.Hit(ctx, "rl:"+rule.Name+":"+subject, rule.Window)
	if err != nil {
		return nil
	}
	if n > rule.Limit {
		return &RateLimitedError{Rule: rule.Name, RetryAfter: resetIn}
	}
	return nil
}

func lockKey(email domain.Email) string     { return "login-locked:" + string(email) }
func failuresKey(email domain.Email) string { return "login-failures:" + string(email) }

// lockedFor reports how long email's log-in lock has left.
func (t *Throttle) lockedFor(ctx context.Context, email domain.Email) time.Duration {
	if t == nil || t.Counter == nil {
		return 0
	}
	d, err := t.Counter.Locked(ctx, lockKey(email))
	if err != nil {
		return 0
	}
	return d
}

// failedLogIn counts a failure and locks the email at the threshold.
// It reports whether the email is now locked.
func (t *Throttle) failedLogIn(ctx context.Context, email domain.Email) bool {
	if t == nil || t.Counter == nil {
		return false
	}
	n, _, err := t.Counter.Hit(ctx, failuresKey(email), LockoutWindow)
	if err != nil || n < LockoutThreshold {
		return false
	}
	_ = t.Counter.Lock(ctx, lockKey(email), LockoutDuration)
	_ = t.Counter.Reset(ctx, failuresKey(email))
	return true
}

// unlock lifts email's log-in lock and forgets its failures.
func (t *Throttle) unlock(ctx context.Context, email domain.Email) {
	if t != nil && t.Counter != nil {
		_ = t.Counter.Reset(ctx, lockKey(email))
		_ = t.Counter.Reset(ctx, failuresKey(email))
	}
}

func (t *Throttle) succeededLogIn(ctx context.Context, email domain.Email) {
	if t != nil && t.Counter != nil {
		_ = t.Counter.Reset(ctx, failuresKey(email))
	}
}
