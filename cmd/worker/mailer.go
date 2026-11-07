package main

import (
	"fmt"

	"enquoir/internal/adapters/mailfile"
	"enquoir/internal/adapters/resend"
	"enquoir/internal/app"
)

// mailerFromEnv picks how email leaves, and says which. EMAIL_PROVIDER
// must be set: resend (RESEND_API_KEY, EMAIL_FROM; MAIL_FILE, if set,
// keeps a local copy) or file (MAIL_FILE, development only). There is
// no default: an omitted setting must stop the worker, not quietly
// write every verification and reset link - live credentials - to a
// local file while reporting the emails as sent.
func mailerFromEnv(getenv func(string) string) (app.Mailer, string, error) {
	file := getenv("MAIL_FILE")
	switch provider := getenv("EMAIL_PROVIDER"); provider {
	case "resend":
		key, from := getenv("RESEND_API_KEY"), getenv("EMAIL_FROM")
		if key == "" || from == "" {
			return nil, "", fmt.Errorf("EMAIL_PROVIDER=resend needs RESEND_API_KEY and EMAIL_FROM")
		}
		var m app.Mailer = resend.New(key, from, nil)
		if file != "" {
			m = mailfile.Copy(m, file)
		}
		return m, "resend", nil
	case "file":
		if file == "" {
			return nil, "", fmt.Errorf("EMAIL_PROVIDER=file needs MAIL_FILE")
		}
		return mailfile.New(file), "file", nil
	case "":
		return nil, "", fmt.Errorf("EMAIL_PROVIDER is not set: use resend, or file for development")
	default:
		return nil, "", fmt.Errorf("unknown EMAIL_PROVIDER %q: use resend or file", provider)
	}
}
