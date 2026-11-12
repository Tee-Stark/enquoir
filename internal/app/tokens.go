package app

import (
	"context"
	"errors"
	"time"
)

// TokenPurpose is what a one-time link does.
type TokenPurpose string

const (
	PurposeVerifyEmail   TokenPurpose = "verify_email"
	PurposeResetPassword TokenPurpose = "reset_password"
	PurposeInvitation    TokenPurpose = "invitation"
	// PurposeMandateConsent is a commerce customer's consent link; its
	// subject is the mandate request.
	PurposeMandateConsent TokenPurpose = "mandate_consent"
	// PurposeMandateRevocation confirms a revocation the merchant asked
	// for; its subject is the mandate.
	PurposeMandateRevocation TokenPurpose = "mandate_revocation"
)

// OneTimeToken is a link token as stored: only its hash. Subject is
// what it acts on - a user ID, or an invitation ID.
type OneTimeToken struct {
	Hash      string
	Purpose   TokenPurpose
	Subject   string
	EmailID   string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
}

// ErrTokenInvalid means a link token is unknown, used, expired, or for
// something else. Deliberately one error: the caller needs no more.
var ErrTokenInvalid = errors.New("this link is invalid or has expired")

// TokenStore keeps one-time link tokens by hash.
type TokenStore interface {
	Create(ctx context.Context, t OneTimeToken) error
	// Consume marks the token used if it is unused, unexpired and for
	// purpose, and returns it - atomically, so a token works once - and
	// marks every other unused token of the same purpose and subject
	// used too: one link spends them all. ErrTokenInvalid otherwise.
	Consume(ctx context.Context, hash string, purpose TokenPurpose, now time.Time) (OneTimeToken, error)
	// Peek returns the token if Consume would accept it now, without
	// spending it.
	Peek(ctx context.Context, hash string, purpose TokenPurpose, now time.Time) (OneTimeToken, error)
}
