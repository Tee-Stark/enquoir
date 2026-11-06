// Package credentials implements the password hashing, ID and secret
// generation ports with the standard library alone.
package credentials

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"enquoir/internal/app"
)

var (
	_ app.PasswordHasher  = (*PBKDF2Hasher)(nil)
	_ app.IDGenerator     = RandomIDs{}
	_ app.SecretGenerator = RandomSecrets{}
)

const (
	// DefaultIterations is OWASP's current recommendation for
	// PBKDF2-HMAC-SHA256.
	DefaultIterations = 600_000
	saltBytes         = 16
	keyBytes          = 32
	scheme            = "pbkdf2-sha256"
)

var b64 = base64.RawStdEncoding

// PBKDF2Hasher hashes passwords as
// "pbkdf2-sha256$<iterations>$<salt>$<key>", base64 without padding.
// The iteration count is stored with each hash, so it can be raised
// without invalidating older ones.
type PBKDF2Hasher struct {
	Iterations int // DefaultIterations when zero
}

func (h *PBKDF2Hasher) Hash(password string) (string, error) {
	iter := h.Iterations
	if iter == 0 {
		iter = DefaultIterations
	}
	salt := make([]byte, saltBytes)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, password, salt, iter, keyBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", scheme, iter, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

func (h *PBKDF2Hasher) Verify(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 || parts[0] != scheme {
		return false, errors.New("credentials: unrecognised password hash format")
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false, errors.New("credentials: bad iteration count in password hash")
	}
	salt, err := b64.DecodeString(parts[2])
	if err != nil {
		return false, errors.New("credentials: bad salt in password hash")
	}
	want, err := b64.DecodeString(parts[3])
	if err != nil || len(want) == 0 {
		return false, errors.New("credentials: bad key in password hash")
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// RandomIDs mints "<prefix>_<24 hex>" IDs: 96 random bits.
type RandomIDs struct{}

func (RandomIDs) NewID(prefix string) string {
	b := make([]byte, 12)
	rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// RandomSecrets mints "<prefix>_<base64url>" secrets: 256 random bits.
type RandomSecrets struct{}

func (RandomSecrets) NewSecret(prefix string) string {
	b := make([]byte, 32)
	rand.Read(b)
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(b)
}
