package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
)

// withEmail wires a synchronous email pipeline into the accounts
// fixture: every queued email is delivered at once to f.mailer.
func (f *accountsFixture) withEmail() *apptest.Mailer {
	mailer, outbox, tokens := &apptest.Mailer{}, apptest.NewOutbox(), apptest.NewTokens()
	deliver := &app.DeliverEmail{Outbox: outbox, Tokens: tokens, Renderer: apptest.Renderer{}, Mailer: mailer,
		Secrets: &apptest.Secrets{}, Clock: f.clock, AppBaseURL: "https://enquoir.etimi.me"}
	f.emails = &app.Emails{Outbox: outbox, Dispatcher: &apptest.Dispatcher{Deliver: deliver.Run}, IDs: &apptest.IDs{}, Clock: f.clock}
	f.tokens = tokens
	f.signUp.Emails = f.emails
	f.mailer = mailer
	return mailer
}

func TestSignUp_SendsAVerificationEmail(t *testing.T) {
	f := newAccountsFixture()
	mailer := f.withEmail()
	signed, _ := f.signUp.Run(context.Background(), validSignUp)
	if token := mailer.TokenTo("ada@example.com"); token == "" {
		t.Fatalf("no verification link sent; sent %+v", mailer.Sent())
	}
	if signed.User.EmailVerified {
		t.Error("a new user starts verified")
	}
}

func TestVerifyEmail(t *testing.T) {
	f := newAccountsFixture()
	mailer := f.withEmail()
	ctx := context.Background()
	signed, _ := f.signUp.Run(ctx, validSignUp)
	verify := &app.VerifyEmail{Tokens: f.tokens, Users: f.accounts, Clock: f.clock}

	if err := verify.Run(ctx, "tok_made_up"); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("a made-up token: %v", err)
	}
	token := mailer.TokenTo("ada@example.com")
	if err := verify.Run(ctx, token); err != nil {
		t.Fatal(err)
	}
	if u, _ := f.accounts.UserByID(ctx, signed.User.ID); !u.EmailVerified {
		t.Error("not verified")
	}
	if err := verify.Run(ctx, token); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("the same link twice: %v", err)
	}
}

func TestVerifyEmail_LinksExpireAfter24Hours(t *testing.T) {
	f := newAccountsFixture()
	mailer := f.withEmail()
	ctx := context.Background()
	f.signUp.Run(ctx, validSignUp)
	f.clock.Advance(24*time.Hour + time.Second)
	err := (&app.VerifyEmail{Tokens: f.tokens, Users: f.accounts, Clock: f.clock}).Run(ctx, mailer.TokenTo("ada@example.com"))
	if !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("an expired link: %v", err)
	}
}

func TestResendVerification(t *testing.T) {
	f := newAccountsFixture()
	mailer := f.withEmail()
	ctx := context.Background()
	signed, _ := f.signUp.Run(ctx, validSignUp)
	resend := &app.ResendVerification{Emails: f.emails}
	p := app.UserPrincipal{User: signed.User}

	if err := resend.Run(ctx, p); err != nil || len(mailer.Sent()) != 2 {
		t.Errorf("resend: %v, %d emails", err, len(mailer.Sent()))
	}
	p.User.EmailVerified = true
	if err := resend.Run(ctx, p); !errors.Is(err, app.ErrAlreadyVerified) {
		t.Errorf("resending once verified: %v", err)
	}
}
