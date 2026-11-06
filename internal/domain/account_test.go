package domain

import (
	"strings"
	"testing"
)

func TestNewEmail_NormalisesCaseAndSpace(t *testing.T) {
	got, err := NewEmail("  Ada.Lovelace@Example.COM ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "ada.lovelace@example.com" {
		t.Errorf("got %q, want ada.lovelace@example.com", got)
	}
}

func TestNewEmail_RejectsInvalid(t *testing.T) {
	cases := map[string]string{
		"empty":          "",
		"no at":          "ada.example.com",
		"two ats":        "ada@lovelace@example.com",
		"no local part":  "@example.com",
		"no domain":      "ada@",
		"no dot":         "ada@localhost",
		"space inside":   "ada lovelace@example.com",
		"dot at end":     "ada@example.",
		"too long":       strings.Repeat("a", 250) + "@example.com",
		"local too long": strings.Repeat("a", 65) + "@example.com",
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewEmail(in); err == nil {
				t.Errorf("NewEmail(%q): want an error", in)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	cases := []struct {
		name string
		pw   string
		ok   bool
	}{
		{"12 characters", "correcthorse", true},
		{"long passphrase", "correct horse battery staple", true},
		{"11 characters", "correcthors", false},
		{"multibyte counts characters, not bytes", "pässwörtpäss", true},
		{"only spaces", "            ", false},
		{"over 128 characters", strings.Repeat("a", 129), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePassword(tc.pw)
			if (err == nil) != tc.ok {
				t.Errorf("ValidatePassword(%q) = %v, want ok=%v", tc.pw, err, tc.ok)
			}
		})
	}
}

func TestNewName_TrimsAndBoundsLength(t *testing.T) {
	got, err := NewName("  Acme Treasury  ")
	if err != nil || got != "Acme Treasury" {
		t.Errorf("NewName = %q, %v; want \"Acme Treasury\", nil", got, err)
	}
	for _, bad := range []string{"", "   ", strings.Repeat("x", 101)} {
		if _, err := NewName(bad); err == nil {
			t.Errorf("NewName(%q): want an error", bad)
		}
	}
}
