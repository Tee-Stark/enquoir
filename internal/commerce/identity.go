// Package commerce is the merchant-delegated commerce domain: a
// merchant's AI shopping agent proposes purchases funded by the
// merchant's customers, and Enquoir decides whether the customer's
// authority allows them. It is a bounded context of its own, separate
// from treasury (internal/domain's intents and beneficiaries), and
// imports only the standard library and internal/domain's shared
// primitives.
package commerce

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MerchantID identifies a merchant integration: one merchant's
// connection to Enquoir, belonging to one organisation. Every commerce
// resource belongs to exactly one.
type MerchantID string

// CustomerID identifies a merchant's customer, as Enquoir knows them.
type CustomerID string

// AgentID identifies an AI agent a merchant registered. It is not a
// treasury agent (domain.AgentID): it holds no credential, it is an
// identity the merchant names in its requests.
type AgentID string

// ExternalID is the merchant's own reference for one of its records (a
// customer or an agent). It is unique per merchant, never across them,
// and compared exactly: case and punctuation are the merchant's.
type ExternalID string

const maxExternalIDLength = 255

// NewExternalID trims s and checks it is 1 to 255 characters of UTF-8
// with no spaces or control characters inside.
func NewExternalID(s string) (ExternalID, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("external ID is required")
	}
	if !utf8.ValidString(s) {
		return "", errors.New("external ID must be valid UTF-8")
	}
	if utf8.RuneCountInString(s) > maxExternalIDLength {
		return "", fmt.Errorf("external ID must be at most %d characters", maxExternalIDLength)
	}
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", errors.New("external ID must not contain spaces or control characters")
	}
	return ExternalID(s), nil
}

// Status is whether a merchant integration, customer or agent may take
// part in commerce. A disabled record keeps its history and can be
// enabled again.
type Status string

const (
	Active   Status = "active"
	Disabled Status = "disabled"
)

// ParseStatus reads a status name, case-insensitively.
func ParseStatus(s string) (Status, error) {
	switch st := Status(strings.ToLower(strings.TrimSpace(s))); st {
	case Active, Disabled:
		return st, nil
	}
	return "", fmt.Errorf("unknown status %q: must be active or disabled", s)
}

func (s Status) IsActive() bool { return s == Active }
