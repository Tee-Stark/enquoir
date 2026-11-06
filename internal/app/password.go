package app

import (
	"context"
	"errors"
	"time"

	"enquoir/internal/domain"
)

// ResetLinkTTL is how long a password-reset link works.
const ResetLinkTTL = time.Hour

// ForgotPassword emails a reset link if the address has an account.
// It answers the same either way, so it never reveals which addresses
// have accounts.
type ForgotPassword struct {
	Accounts AccountStore
	Emails   *Emails
}

func (f *ForgotPassword) Run(ctx context.Context, rawEmail string) error {
	email, err := domain.NewEmail(rawEmail)
	if err != nil {
		return nil
	}
	user, err := f.Accounts.UserByEmail(ctx, email)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Emails.Send(ctx, Email{
		UserID: user.ID, To: user.Email, Template: TemplatePasswordReset,
		Link: &LinkSpec{Purpose: PurposeResetPassword, Subject: string(user.ID), TTL: ResetLinkTTL},
	})
	return err
}

func passwordChangedEmail(u User) Email {
	return Email{UserID: u.ID, To: u.Email, Template: TemplatePasswordChanged}
}

// ResetPassword sets a new password with a reset link's token, signs
// the user out everywhere, and tells them it happened.
type ResetPassword struct {
	Tokens   TokenStore
	Users    UserStore
	Accounts AccountStore
	Sessions SessionStore
	Hasher   PasswordHasher
	Emails   *Emails
	Clock    Clock
}

func (r *ResetPassword) Run(ctx context.Context, token, newPassword string) error {
	// Checked before the link is spent, so a rejected password doesn't
	// cost the user their link.
	if err := domain.ValidatePassword(newPassword); err != nil {
		return invalid("password", err)
	}
	now := r.Clock.Now()
	t, err := r.Tokens.Consume(ctx, HashSecret(token), PurposeResetPassword, now)
	if err != nil {
		return err
	}
	user, err := r.Accounts.UserByID(ctx, domain.UserID(t.Subject))
	if err != nil {
		return err
	}
	return setPassword(ctx, r.Users, r.Sessions, r.Hasher, r.Emails, user, newPassword, "", now)
}

// ChangePassword changes the signed-in user's password, given the
// current one, and signs out every other session.
type ChangePassword struct {
	Users    UserStore
	Sessions SessionStore
	Hasher   PasswordHasher
	Emails   *Emails
	Clock    Clock
}

func (c *ChangePassword) Run(ctx context.Context, p UserPrincipal, current, newPassword string) error {
	ok, err := c.Hasher.Verify(current, p.User.PasswordHash)
	if err != nil || !ok {
		return ErrInvalidCredentials
	}
	if err := domain.ValidatePassword(newPassword); err != nil {
		return invalid("new_password", err)
	}
	return setPassword(ctx, c.Users, c.Sessions, c.Hasher, c.Emails, p.User, newPassword, p.SessionID, c.Clock.Now())
}

func setPassword(ctx context.Context, users UserStore, sessions SessionStore, hasher PasswordHasher, emails *Emails,
	user User, password, keepSession string, now time.Time) error {
	hash, err := hasher.Hash(password)
	if err != nil {
		return err
	}
	if err := users.SetPassword(ctx, user.ID, hash, now); err != nil {
		return err
	}
	if err := sessions.RevokeAllForUser(ctx, user.ID, keepSession, now); err != nil {
		return err
	}
	_, err = emails.Send(ctx, passwordChangedEmail(user))
	return err
}
