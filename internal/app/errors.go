package app

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

var (
	// ErrNotFound means a store holds no such record (for this
	// organisation: another tenant's record is not found, never
	// forbidden).
	ErrNotFound = errors.New("not found")

	// ErrUnauthenticated means a credential is missing, unknown,
	// expired or revoked.
	ErrUnauthenticated = errors.New("unauthenticated")

	// ErrInvalidCredentials means an email and password did not match
	// an account. Deliberately silent on which part was wrong.
	ErrInvalidCredentials = errors.New("invalid email or password")

	// ErrEmailTaken means an account already uses that email.
	ErrEmailTaken = errors.New("email already registered")

	// ErrNoOrganisation means the credentials are right but the user
	// belongs to no organisation (they were removed from all of them).
	ErrNoOrganisation = errors.New("this account belongs to no organisation")

	// ErrForbidden means the signed-in user's role does not allow it.
	ErrForbidden = errors.New("your role does not allow this")
)

// ValidationError is input that failed validation, naming the field.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

func invalid(field string, err error) *ValidationError {
	return &ValidationError{Field: field, Message: err.Error()}
}

// HashSecret is how session tokens and API keys are stored: the
// SHA-256 of a high-entropy secret, hex-encoded. A slow hash buys
// nothing for 256-bit random secrets and would cost every request.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
