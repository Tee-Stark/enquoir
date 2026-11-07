package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"enquoir/internal/app"
)

func passwordFixture(t *testing.T) (*accountsFixture, *app.ForgotPassword, *app.ResetPassword, *app.ChangePassword) {
	f := newAccountsFixture()
	f.withEmail()
	forgot := &app.ForgotPassword{Accounts: f.accounts, Emails: f.emails}
	reset := &app.ResetPassword{Tokens: f.tokens, Users: f.accounts, Accounts: f.accounts, Sessions: f.sessions, Hasher: f.hasher, Emails: f.emails, Clock: f.clock}
	change := &app.ChangePassword{Users: f.accounts, Accounts: f.accounts, Sessions: f.sessions, Hasher: f.hasher, Emails: f.emails, Clock: f.clock}
	return f, forgot, reset, change
}

func TestForgotPassword_SendsALinkOnlyForARealAccountButAnswersTheSame(t *testing.T) {
	f, forgot, _, _ := passwordFixture(t)
	ctx := context.Background()
	f.signUp.Run(ctx, validSignUp)
	before := len(f.mailer.Sent())

	if err := forgot.Run(ctx, "ADA@example.com"); err != nil {
		t.Fatal(err)
	}
	if err := forgot.Run(ctx, "nobody@example.com"); err != nil {
		t.Errorf("an unknown email: %v, want nil - the answer must not reveal accounts", err)
	}
	if err := forgot.Run(ctx, "not an email"); err != nil {
		t.Errorf("garbage: %v, want nil", err)
	}
	sent := f.mailer.Sent()[before:]
	if len(sent) != 1 || sent[0].To != "ada@example.com" || sent[0].Subject != string(app.TemplatePasswordReset) {
		t.Errorf("sent = %+v, want one reset email to ada", sent)
	}
}

func TestResetPassword(t *testing.T) {
	f, forgot, reset, _ := passwordFixture(t)
	ctx := context.Background()
	signed, _ := f.signUp.Run(ctx, validSignUp)
	forgot.Run(ctx, "ada@example.com")
	token := f.mailer.TokenTo("ada@example.com")

	var verr *app.ValidationError
	if err := reset.Run(ctx, token, "short"); !errors.As(err, &verr) {
		t.Fatalf("a weak password: %v, want a ValidationError", err)
	}
	if err := reset.Run(ctx, token, "a brand new passphrase"); err != nil {
		t.Fatalf("the link must survive a rejected password: %v", err)
	}
	if _, err := f.logIn.Run(ctx, "ada@example.com", "a brand new passphrase"); err != nil {
		t.Errorf("log in with the new password: %v", err)
	}
	if _, err := f.resolve.Run(ctx, signed.Session.Token); !errors.Is(err, app.ErrUnauthenticated) {
		t.Error("a session from before the reset still works")
	}
	if last := f.mailer.Sent()[len(f.mailer.Sent())-1]; last.Subject != string(app.TemplatePasswordChanged) {
		t.Errorf("last email = %q, want the password-changed notice", last.Subject)
	}
	if err := reset.Run(ctx, token, "yet another passphrase"); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("the link twice: %v", err)
	}
}

func TestResetPassword_LinksExpireAfterAnHour(t *testing.T) {
	f, forgot, reset, _ := passwordFixture(t)
	ctx := context.Background()
	f.signUp.Run(ctx, validSignUp)
	forgot.Run(ctx, "ada@example.com")
	f.clock.Advance(time.Hour + time.Second)
	if err := reset.Run(ctx, f.mailer.TokenTo("ada@example.com"), "a brand new passphrase"); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("an expired link: %v", err)
	}
}

func TestChangePassword(t *testing.T) {
	f, _, _, change := passwordFixture(t)
	ctx := context.Background()
	signed, _ := f.signUp.Run(ctx, validSignUp)
	other, _ := f.logIn.Run(ctx, "ada@example.com", validSignUp.Password)
	p, _ := f.resolve.Run(ctx, signed.Session.Token)

	if err := change.Run(ctx, p, "wrong wrong wrong", "a brand new passphrase"); !errors.Is(err, app.ErrInvalidCredentials) {
		t.Errorf("wrong current password: %v", err)
	}
	if err := change.Run(ctx, p, validSignUp.Password, "a brand new passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.resolve.Run(ctx, signed.Session.Token); err != nil {
		t.Errorf("the session that changed it was signed out: %v", err)
	}
	if _, err := f.resolve.Run(ctx, other.Session.Token); !errors.Is(err, app.ErrUnauthenticated) {
		t.Error("another session survived a password change")
	}
}
