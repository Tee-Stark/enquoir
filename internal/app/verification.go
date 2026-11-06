package app

import (
	"context"
	"errors"
	"time"

	"enquoir/internal/domain"
)

// VerificationLinkTTL is how long a verification link works.
const VerificationLinkTTL = 24 * time.Hour

// ErrAlreadyVerified means there is nothing left to verify.
var ErrAlreadyVerified = errors.New("this email address is already verified")

// ErrEmailUnverified means the action needs a verified email address.
var ErrEmailUnverified = errors.New("verify your email address first")

// UserStore changes a user's own record.
type UserStore interface {
	MarkEmailVerified(ctx context.Context, user domain.UserID, at time.Time) error
	// SetPassword stores a new password hash and its change time.
	SetPassword(ctx context.Context, user domain.UserID, hash string, at time.Time) error
}

func verificationEmail(u User) Email {
	return Email{
		UserID: u.ID, To: u.Email, Template: TemplateVerifyEmail,
		Link: &LinkSpec{Purpose: PurposeVerifyEmail, Subject: string(u.ID), TTL: VerificationLinkTTL},
	}
}

// VerifyEmail spends a verification link.
type VerifyEmail struct {
	Tokens TokenStore
	Users  UserStore
	Clock  Clock
	Cache  *Cache
}

func (v *VerifyEmail) Run(ctx context.Context, token string) error {
	now := v.Clock.Now()
	t, err := v.Tokens.Consume(ctx, HashSecret(token), PurposeVerifyEmail, now)
	if err != nil {
		return err
	}
	if err := v.Users.MarkEmailVerified(ctx, domain.UserID(t.Subject), now); err != nil {
		return err
	}
	v.Cache.InvalidateUser(ctx, domain.UserID(t.Subject))
	return nil
}

// ResendVerification sends the signed-in user a fresh link, at most
// once a minute and five times a day.
type ResendVerification struct {
	Emails   *Emails
	Throttle *Throttle
}

func (r *ResendVerification) Run(ctx context.Context, p UserPrincipal) error {
	if p.User.EmailVerified {
		return ErrAlreadyVerified
	}
	for _, rule := range []Rule{RuleResendPerMinute, RuleResendPerDay} {
		if err := r.Throttle.Check(ctx, rule, string(p.User.ID)); err != nil {
			return err
		}
	}
	_, err := r.Emails.Send(ctx, verificationEmail(p.User))
	return err
}
