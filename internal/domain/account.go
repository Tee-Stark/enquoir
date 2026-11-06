package domain

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// OrgID identifies an organisation: one Enquoir Cloud tenant. Every
// intent, beneficiary, agent and attempt belongs to exactly one.
type OrgID string

// UserID identifies a person who signs in to the dashboard.
type UserID string

// AgentID identifies an AI agent that submits intents with its API key.
type AgentID string

// Email is a normalised (trimmed, lower-cased) email address.
type Email string

const (
	maxEmailLength      = 254
	maxEmailLocalLength = 64
	minPasswordLength   = 12
	maxPasswordLength   = 128
	maxNameLength       = 100
)

// NewEmail trims and lower-cases s and checks it has the shape of an
// email address. It does not prove the address exists; verification
// does that.
func NewEmail(s string) (Email, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", errors.New("email is required")
	}
	if len(s) > maxEmailLength {
		return "", fmt.Errorf("email must be at most %d characters", maxEmailLength)
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return "", errors.New("email must not contain spaces")
	}
	local, domain, ok := strings.Cut(s, "@")
	if !ok || strings.Contains(domain, "@") {
		return "", errors.New("email must contain exactly one @")
	}
	if local == "" || len(local) > maxEmailLocalLength {
		return "", fmt.Errorf("email must have a local part of 1 to %d characters", maxEmailLocalLength)
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return "", errors.New("email domain must contain a dot")
	}
	for _, l := range labels {
		if l == "" {
			return "", errors.New("email domain is malformed")
		}
	}
	return Email(s), nil
}

// ValidatePassword enforces the password policy: 12 to 128 characters,
// not only whitespace. Length is what matters; no composition rules.
func ValidatePassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < minPasswordLength {
		return fmt.Errorf("password must be at least %d characters", minPasswordLength)
	}
	if n > maxPasswordLength {
		return fmt.Errorf("password must be at most %d characters", maxPasswordLength)
	}
	if strings.TrimSpace(pw) == "" {
		return errors.New("password must not be only whitespace")
	}
	return nil
}

// NewName trims a display name (an organisation's or an agent's) and
// checks it is 1 to 100 characters.
func NewName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("name is required")
	}
	if utf8.RuneCountInString(s) > maxNameLength {
		return "", fmt.Errorf("name must be at most %d characters", maxNameLength)
	}
	return s, nil
}
