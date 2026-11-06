package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
)

type emailFixture struct {
	outbox     *apptest.Outbox
	tokens     *apptest.Tokens
	mailer     *apptest.Mailer
	dispatcher *apptest.Dispatcher
	clock      *apptest.Clock
	emails     *app.Emails
	deliver    *app.DeliverEmail
}

func newEmailFixture() *emailFixture {
	f := &emailFixture{outbox: apptest.NewOutbox(), tokens: apptest.NewTokens(), mailer: &apptest.Mailer{}, dispatcher: &apptest.Dispatcher{}, clock: apptest.NewClock()}
	f.emails = &app.Emails{Outbox: f.outbox, Dispatcher: f.dispatcher, IDs: &apptest.IDs{}, Clock: f.clock}
	f.deliver = &app.DeliverEmail{Outbox: f.outbox, Tokens: f.tokens, Renderer: apptest.Renderer{}, Mailer: f.mailer,
		Secrets: &apptest.Secrets{}, Clock: f.clock, AppBaseURL: "https://enquoir.etimi.me"}
	return f
}

var verifyEmail = app.Email{To: "ada@example.com", UserID: "usr_1", Template: app.TemplateVerifyEmail,
	Link: &app.LinkSpec{Purpose: app.PurposeVerifyEmail, Subject: "usr_1", TTL: 24 * time.Hour}}

func TestEmails_QueueThenDispatch(t *testing.T) {
	f := newEmailFixture()
	id, err := f.emails.Send(context.Background(), verifyEmail)
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	stored, _ := f.outbox.Get(context.Background(), id)
	if stored.Status != app.EmailQueued || stored.Template != app.TemplateVerifyEmail || !stored.CreatedAt.Equal(f.clock.Now()) {
		t.Errorf("queued = %+v", stored)
	}
	if got := f.dispatcher.Dispatched(); len(got) != 1 || got[0] != id {
		t.Errorf("dispatched %v, want [%s]", got, id)
	}
}

// The email is on the record before anything is sent; if the durable
// send cannot be started, the account action still succeeds and the
// email stays queued to be retried.
func TestEmails_AQueuedEmailSurvivesAFailedDispatch(t *testing.T) {
	f := newEmailFixture()
	f.dispatcher.Err = errors.New("temporal down")
	id, err := f.emails.Send(context.Background(), verifyEmail)
	if err != nil {
		t.Fatalf("a failed dispatch failed the send: %v", err)
	}
	if e, _ := f.outbox.Get(context.Background(), id); e.Status != app.EmailQueued {
		t.Errorf("status = %s, want queued", e.Status)
	}
}

func TestDeliver_MintsTheTokenAtSendTimeAndStoresOnlyItsHash(t *testing.T) {
	f := newEmailFixture()
	ctx := context.Background()
	id, _ := f.emails.Send(ctx, verifyEmail)
	if err := f.deliver.Run(ctx, id); err != nil {
		t.Fatal(err)
	}

	sent := f.mailer.Sent()
	if len(sent) != 1 || sent[0].To != "ada@example.com" || sent[0].IdempotencyKey != "email/"+id {
		t.Fatalf("sent = %+v", sent)
	}
	link := apptest.LinkIn(sent[0].Text)
	if !strings.HasPrefix(link, "https://enquoir.etimi.me/verify-email?token=") {
		t.Fatalf("link = %q", link)
	}
	token := strings.TrimPrefix(link, "https://enquoir.etimi.me/verify-email?token=")
	if f.tokens.StoresPlaintext(token) {
		t.Error("the token was stored in plaintext")
	}
	if e, _ := f.outbox.Get(ctx, id); strings.Contains(e.Params["link"], token) || e.Status != app.EmailSent || e.ProviderID == "" {
		t.Errorf("outbox row = %+v; want sent, with no link stored", e)
	}

	tok, err := f.tokens.Consume(ctx, app.HashSecret(token), app.PurposeVerifyEmail, f.clock.Now())
	if err != nil || tok.Subject != "usr_1" || !tok.ExpiresAt.Equal(f.clock.Now().Add(24*time.Hour)) || tok.EmailID != id {
		t.Errorf("token = %+v, %v", tok, err)
	}
}

func TestDeliver_Outcomes(t *testing.T) {
	cases := []struct {
		name       string
		mailErr    error
		wantErr    bool
		wantStatus app.EmailStatus
	}{
		{"sent", nil, false, app.EmailSent},
		{"already sent under the key: the earlier link stands", app.ErrAlreadySent, false, app.EmailSent},
		{"refused for good", app.ErrRejected, false, app.EmailFailed},
		{"unclear: retried by the caller", app.ErrAmbiguous, true, app.EmailQueued},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newEmailFixture()
			ctx := context.Background()
			id, _ := f.emails.Send(ctx, verifyEmail)
			f.mailer.Err = tc.mailErr
			err := f.deliver.Run(ctx, id)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if e, _ := f.outbox.Get(ctx, id); e.Status != tc.wantStatus {
				t.Errorf("status = %s, want %s", e.Status, tc.wantStatus)
			}
		})
	}
}

func TestDeliver_DoesNothingForAnEmailAlreadyDone(t *testing.T) {
	f := newEmailFixture()
	ctx := context.Background()
	id, _ := f.emails.Send(ctx, verifyEmail)
	f.deliver.Run(ctx, id)
	f.deliver.Run(ctx, id)
	if n := len(f.mailer.Sent()); n != 1 {
		t.Errorf("sent %d times, want 1", n)
	}
}

func TestDeliver_TemplatesWithoutALinkMintNoToken(t *testing.T) {
	f := newEmailFixture()
	ctx := context.Background()
	id, _ := f.emails.Send(ctx, app.Email{To: "ada@example.com", Template: app.TemplatePasswordChanged})
	if err := f.deliver.Run(ctx, id); err != nil {
		t.Fatal(err)
	}
	if f.tokens.Count() != 0 {
		t.Error("a token was minted for an email with no link")
	}
}
