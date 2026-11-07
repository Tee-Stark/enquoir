package main

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// An unset EMAIL_PROVIDER must stop the worker, not quietly write every
// verification and reset link - live credentials - to a local file.
func TestMailerFromEnv_TheProviderMustBeChosen(t *testing.T) {
	if _, _, err := mailerFromEnv(env(map[string]string{"RESEND_API_KEY": "re_x", "EMAIL_FROM": "a <b@c.co>"})); err == nil ||
		!strings.Contains(err.Error(), "EMAIL_PROVIDER") {
		t.Errorf("no EMAIL_PROVIDER: err = %v, want a start-up error naming it", err)
	}
}

func TestMailerFromEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		mode    string
		wantErr bool
	}{
		{"resend", map[string]string{"EMAIL_PROVIDER": "resend", "RESEND_API_KEY": "re_x", "EMAIL_FROM": "Enquoir <no-reply@enquoir.etimi.me>"}, "resend", false},
		{"resend without a key", map[string]string{"EMAIL_PROVIDER": "resend", "EMAIL_FROM": "x <a@b.co>"}, "", true},
		{"file, chosen explicitly", map[string]string{"EMAIL_PROVIDER": "file", "MAIL_FILE": "/tmp/m.jsonl"}, "file", false},
		{"file without a path", map[string]string{"EMAIL_PROVIDER": "file"}, "", true},
		{"unknown", map[string]string{"EMAIL_PROVIDER": "smtp"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, mode, err := mailerFromEnv(env(tc.env))
			if (err != nil) != tc.wantErr || mode != tc.mode || (err == nil && m == nil) {
				t.Errorf("mode %q, err %v; want mode %q, error %v", mode, err, tc.mode, tc.wantErr)
			}
		})
	}
}
