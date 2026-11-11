package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"enquoir/internal/domain"
)

// TemplateID names an email template. Templates are embedded in the
// binary; only the ID is ever stored.
type TemplateID string

const (
	TemplateVerifyEmail     TemplateID = "verify_email"
	TemplateInvitation      TemplateID = "invitation"
	TemplatePasswordReset   TemplateID = "password_reset"
	TemplatePasswordChanged TemplateID = "password_changed"
	// Commerce: a customer's consent link, and notice that a mandate was revoked.
	TemplateMandateConsent TemplateID = "mandate_consent"
	TemplateMandateRevoked TemplateID = "mandate_revoked"
)

// RenderedEmail is a template filled in: ready to send.
type RenderedEmail struct {
	Subject string
	HTML    string
	Text    string
}

// EmailRenderer fills in an embedded template. Params are plain,
// non-secret strings; the renderer escapes them.
type EmailRenderer interface {
	Render(id TemplateID, params map[string]string) (RenderedEmail, error)
}

// OutgoingEmail is one message for a Mailer.
type OutgoingEmail struct {
	To             domain.Email
	Subject        string
	HTML           string
	Text           string
	IdempotencyKey string // the same key never sends twice
	Tags           map[string]string
}

// Mailer sends email. Errors wrap ErrAmbiguous when the outcome is
// unclear (worth retrying with the same idempotency key), or
// ErrRejected when the provider refused it for good.
type Mailer interface {
	Send(ctx context.Context, m OutgoingEmail) (providerID string, err error)
}

// ErrAlreadySent means the provider already sent a message under this
// idempotency key - with an earlier link, which is still valid.
var ErrAlreadySent = errors.New("already sent under this idempotency key")

// EmailStatus is where a queued email stands.
type EmailStatus string

const (
	EmailQueued EmailStatus = "queued"
	EmailSent   EmailStatus = "sent"
	EmailFailed EmailStatus = "failed"
)

// Email is a row of the email outbox: what to send to whom, never the
// rendered message and never a secret. A template that carries a link
// names its purpose; the link's token is minted when it is sent.
type Email struct {
	ID         string
	OrgID      domain.OrgID // empty for account emails
	UserID     domain.UserID
	To         domain.Email
	Template   TemplateID
	Params     map[string]string
	Link       *LinkSpec
	Status     EmailStatus
	ProviderID string
	Error      string
	CreatedAt  time.Time
	SentAt     *time.Time
}

// LinkSpec says which one-time link an email carries.
type LinkSpec struct {
	Purpose TokenPurpose
	Subject string // the user or invitation the token acts on
	TTL     time.Duration
}

// EmailOutbox stores queued emails and their fate.
type EmailOutbox interface {
	Queue(ctx context.Context, e Email) error
	// Get returns ErrNotFound for an unknown ID.
	Get(ctx context.Context, id string) (Email, error)
	MarkSent(ctx context.Context, id, providerID string, at time.Time) error
	MarkFailed(ctx context.Context, id, reason string, at time.Time) error
}

// EmailDispatcher starts the durable send of a queued email. Starting
// it twice for one email is harmless.
type EmailDispatcher interface {
	Dispatch(ctx context.Context, emailID string) error
}

// Emails queues emails for durable sending.
type Emails struct {
	Outbox     EmailOutbox
	Dispatcher EmailDispatcher
	IDs        IDGenerator
	Clock      Clock
}

// Send records e in the outbox and starts its durable send. Only a
// failure to record it is an error: once queued, a failed dispatch
// leaves it queued for a retry (a resend, or the worker's sweep),
// rather than failing the account action that asked for it.
func (s *Emails) Send(ctx context.Context, e Email) (string, error) {
	e.ID = s.IDs.NewID("eml")
	e.Status = EmailQueued
	e.CreatedAt = s.Clock.Now()
	if e.Params == nil {
		e.Params = map[string]string{}
	}
	if err := s.Outbox.Queue(ctx, e); err != nil {
		return "", fmt.Errorf("queueing email: %w", err)
	}
	_ = s.Dispatcher.Dispatch(ctx, e.ID)
	return e.ID, nil
}

// linkPaths are where the frontend serves each link (docs/frontend.md).
var linkPaths = map[TokenPurpose]string{
	PurposeVerifyEmail:    "/verify-email",
	PurposeResetPassword:  "/reset-password",
	PurposeInvitation:     "/invitations/accept",
	PurposeMandateConsent: "/consent",
}

// DeliverEmail sends one queued email. It is what the durable send
// runs, and may run more than once for one email: the idempotency key
// is the email's ID, so the provider sends it at most once.
type DeliverEmail struct {
	Outbox     EmailOutbox
	Tokens     TokenStore
	Renderer   EmailRenderer
	Mailer     Mailer
	Secrets    SecretGenerator
	Clock      Clock
	AppBaseURL string // e.g. https://enquoir.etimi.me
}

// Run returns an error only when the outcome is unclear and a retry
// may help; a sent, already-sent or refused email returns nil.
//
// A link's token is minted here, at send time, and only its hash is
// stored: the plaintext exists only in this call. A retry mints a
// fresh token; if an earlier attempt was in fact delivered, the
// provider recognises the idempotency key and the earlier link, still
// valid, is the one the recipient has.
func (d *DeliverEmail) Run(ctx context.Context, emailID string) error {
	e, err := d.Outbox.Get(ctx, emailID)
	if err != nil {
		return err
	}
	if e.Status != EmailQueued {
		return nil
	}

	params := make(map[string]string, len(e.Params)+1)
	for k, v := range e.Params {
		params[k] = v
	}
	now := d.Clock.Now()
	if e.Link != nil {
		token := d.Secrets.NewSecret("tok")
		if err := d.Tokens.Create(ctx, OneTimeToken{
			Hash: HashSecret(token), Purpose: e.Link.Purpose, Subject: e.Link.Subject, EmailID: e.ID,
			CreatedAt: now, ExpiresAt: now.Add(e.Link.TTL),
		}); err != nil {
			return fmt.Errorf("storing the link token: %w", err)
		}
		params["link"] = strings.TrimRight(d.AppBaseURL, "/") + linkPaths[e.Link.Purpose] + "?token=" + url.QueryEscape(token)
	}

	rendered, err := d.Renderer.Render(e.Template, params)
	if err != nil {
		return d.Outbox.MarkFailed(ctx, e.ID, "rendering: "+err.Error(), now)
	}
	providerID, err := d.Mailer.Send(ctx, OutgoingEmail{
		To: e.To, Subject: rendered.Subject, HTML: rendered.HTML, Text: rendered.Text,
		IdempotencyKey: "email/" + e.ID, Tags: map[string]string{"template": string(e.Template)},
	})
	switch {
	case err == nil, errors.Is(err, ErrAlreadySent):
		return d.Outbox.MarkSent(ctx, e.ID, providerID, d.Clock.Now())
	case errors.Is(err, ErrRejected):
		return d.Outbox.MarkFailed(ctx, e.ID, err.Error(), d.Clock.Now())
	default:
		return err
	}
}

// GiveUpEmail records that an email's durable send ran out of retries.
type GiveUpEmail struct {
	Outbox EmailOutbox
	Clock  Clock
}

func (g *GiveUpEmail) Run(ctx context.Context, emailID, reason string) error {
	return g.Outbox.MarkFailed(ctx, emailID, "gave up: "+reason, g.Clock.Now())
}
