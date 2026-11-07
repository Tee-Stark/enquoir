package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
)

func withThrottle(f *accountsFixture) *apptest.Counter {
	c := apptest.NewCounter(f.clock)
	f.logIn.Throttle = &app.Throttle{Counter: c}
	return c
}

func TestLogIn_LocksForAnHourAfterTenFailures(t *testing.T) {
	f := newAccountsFixture()
	withThrottle(f)
	ctx := context.Background()
	f.signUp.Run(ctx, validSignUp)

	for i := 1; i <= 10; i++ {
		f.clock.Advance(13 * time.Second) // stay under the per-minute limit
		_, err := f.logIn.Run(ctx, "ada@example.com", "wrong wrong wrong")
		if i < 10 && !errors.Is(err, app.ErrInvalidCredentials) {
			t.Fatalf("failure %d: %v", i, err)
		}
		if i == 10 && !errors.Is(err, app.ErrAccountLocked) {
			t.Fatalf("the tenth failure: %v, want the account locked", err)
		}
	}
	f.clock.Advance(time.Minute)
	_, err := f.logIn.Run(ctx, "ada@example.com", validSignUp.Password)
	var locked *app.AccountLockedError
	if !errors.As(err, &locked) || locked.RetryAfter <= 50*time.Minute {
		t.Errorf("the right password while locked: %v, want locked for most of an hour", err)
	}
	f.clock.Advance(time.Hour)
	if _, err := f.logIn.Run(ctx, "ada@example.com", validSignUp.Password); err != nil {
		t.Errorf("after the hour: %v", err)
	}
}

func TestLogIn_ASuccessClearsTheFailures(t *testing.T) {
	f := newAccountsFixture()
	withThrottle(f)
	ctx := context.Background()
	f.signUp.Run(ctx, validSignUp)
	for round := 0; round < 3; round++ {
		for i := 0; i < 4; i++ {
			f.clock.Advance(13 * time.Second)
			f.logIn.Run(ctx, "ada@example.com", "wrong wrong wrong")
		}
		f.clock.Advance(13 * time.Second)
		if _, err := f.logIn.Run(ctx, "ada@example.com", validSignUp.Password); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
}

func TestLogIn_UnknownEmailsLockTooRevealingNothing(t *testing.T) {
	f := newAccountsFixture()
	withThrottle(f)
	var err error
	for i := 0; i < 10; i++ {
		f.clock.Advance(13 * time.Second)
		_, err = f.logIn.Run(context.Background(), "nobody@example.com", "wrong wrong wrong")
	}
	if !errors.Is(err, app.ErrAccountLocked) {
		t.Errorf("an unknown email: %v, want it locked like a real one", err)
	}
}

func TestLogIn_FiveAttemptsAMinutePerEmail(t *testing.T) {
	f := newAccountsFixture()
	withThrottle(f)
	ctx := context.Background()
	f.signUp.Run(ctx, validSignUp)
	for i := 0; i < 5; i++ {
		f.logIn.Run(ctx, "ada@example.com", "wrong wrong wrong")
	}
	_, err := f.logIn.Run(ctx, "ada@example.com", validSignUp.Password)
	var limited *app.RateLimitedError
	if !errors.As(err, &limited) || limited.RetryAfter <= 0 {
		t.Errorf("a sixth attempt in a minute: %v", err)
	}
}

// Rate limiting must never take authentication down with it - but a
// counter that is down must not switch limits and lockout off either:
// the fallback keeps them, per process, and the failure is reported.
func TestThrottle_ACounterThatIsDownFallsBackAndIsReported(t *testing.T) {
	f := newAccountsFixture()
	primary := apptest.NewCounter(f.clock)
	primary.Err = errors.New("redis: connection refused")
	var reported []error
	th := &app.Throttle{Counter: primary, Fallback: apptest.NewCounter(f.clock), OnError: func(err error) { reported = append(reported, err) }}
	f.logIn.Throttle = th
	ctx := context.Background()
	f.signUp.Run(ctx, validSignUp)

	if _, err := f.logIn.Run(ctx, "ada@example.com", validSignUp.Password); err != nil {
		t.Fatalf("log-in with the counter down: %v", err)
	}
	if len(reported) == 0 {
		t.Error("the counter failure was not reported")
	}
	var err error
	for i := 0; i < 10; i++ {
		f.clock.Advance(13 * time.Second)
		_, err = f.logIn.Run(ctx, "ada@example.com", "wrong wrong wrong")
	}
	if !errors.Is(err, app.ErrAccountLocked) {
		t.Errorf("ten failures with the counter down: %v, want the lockout to hold", err)
	}
}

func TestResendAndForgot_AreLimited(t *testing.T) {
	f := newAccountsFixture()
	f.withEmail()
	ctx := context.Background()
	c := apptest.NewCounter(f.clock)
	signed, _ := f.signUp.Run(ctx, validSignUp)

	resend := &app.ResendVerification{Emails: f.emails, Throttle: &app.Throttle{Counter: c}}
	p := app.UserPrincipal{User: signed.User}
	var limited *app.RateLimitedError
	if err := resend.Run(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := resend.Run(ctx, p); !errors.As(err, &limited) {
		t.Errorf("a second resend within a minute: %v", err)
	}
	for i := 0; i < 4; i++ {
		f.clock.Advance(time.Minute)
		resend.Run(ctx, p)
	}
	f.clock.Advance(time.Minute)
	if err := resend.Run(ctx, p); !errors.As(err, &limited) {
		t.Errorf("a sixth resend in a day: %v", err)
	}

	forgot := &app.ForgotPassword{Accounts: f.accounts, Emails: f.emails, Throttle: &app.Throttle{Counter: c}}
	for i := 0; i < 3; i++ {
		forgot.Run(ctx, "ada@example.com")
	}
	if err := forgot.Run(ctx, "ada@example.com"); !errors.As(err, &limited) {
		t.Errorf("a fourth reset request in an hour: %v", err)
	}
}

// The lockout message promises a reset unlocks the account sooner: a
// reset proves the address, so it must.
func TestResetPassword_UnlocksALockedAccount(t *testing.T) {
	f, forgot, reset, _ := passwordFixture(t)
	c := withThrottle(f)
	reset.Throttle = &app.Throttle{Counter: c}
	ctx := context.Background()
	f.signUp.Run(ctx, validSignUp)
	for i := 0; i < 10; i++ {
		f.clock.Advance(13 * time.Second)
		f.logIn.Run(ctx, "ada@example.com", "wrong wrong wrong")
	}
	forgot.Run(ctx, "ada@example.com")
	if err := reset.Run(ctx, f.mailer.TokenTo("ada@example.com"), "a brand new passphrase"); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Minute)
	if _, err := f.logIn.Run(ctx, "ada@example.com", "a brand new passphrase"); err != nil {
		t.Errorf("logging in after a reset: %v, want the lock cleared", err)
	}
}

// A stolen session must not be a way to guess the account's password.
func TestChangePassword_GuessesAtTheCurrentPasswordAreLimited(t *testing.T) {
	f, _, _, change := passwordFixture(t)
	change.Throttle = &app.Throttle{Counter: apptest.NewCounter(f.clock)}
	ctx := context.Background()
	signed, _ := f.signUp.Run(ctx, validSignUp)
	p, _ := f.resolve.Run(ctx, signed.Session.Token)
	for i := 0; i < 5; i++ {
		if err := change.Run(ctx, p, "guess guess guess", "a brand new passphrase"); !errors.Is(err, app.ErrInvalidCredentials) {
			t.Fatalf("guess %d: %v", i, err)
		}
	}
	var limited *app.RateLimitedError
	if err := change.Run(ctx, p, validSignUp.Password, "a brand new passphrase"); !errors.As(err, &limited) {
		t.Errorf("a sixth attempt within 15 minutes: %v, want rate limited", err)
	}
	f.clock.Advance(15 * time.Minute)
	if err := change.Run(ctx, p, validSignUp.Password, "a brand new passphrase"); err != nil {
		t.Errorf("after the window: %v", err)
	}
}
