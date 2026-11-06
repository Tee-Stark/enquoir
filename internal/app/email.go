package app

import (
	"context"
	"errors"
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
