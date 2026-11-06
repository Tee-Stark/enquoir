// Command emailpreview renders every email template with sample values
// into a directory (default email-previews/), as .html and .txt files,
// with an index.html linking them - for looking at the designs in a
// browser.
package main

import (
	"fmt"
	"html"
	"os"
	"path/filepath"

	"enquoir/internal/adapters/emailtmpl"
	"enquoir/internal/app"
)

var samples = map[app.TemplateID]map[string]string{
	app.TemplateVerifyEmail:     {"link": "https://enquoir.etimi.me/verify-email?token=sample"},
	app.TemplateInvitation:      {"link": "https://enquoir.etimi.me/invitations/accept?token=sample", "organisation": "Acme Treasury", "inviter": "ada@acme.example", "role": "operator"},
	app.TemplatePasswordReset:   {"link": "https://enquoir.etimi.me/reset-password?token=sample"},
	app.TemplatePasswordChanged: {},
}

func main() {
	dir := "email-previews"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(err)
	}
	r := emailtmpl.New()
	index := "<!doctype html><meta charset=utf-8><title>Enquoir email previews</title><body style=\"font-family:sans-serif\"><h1>Email previews</h1><ul>"
	for _, id := range emailtmpl.Templates {
		e, err := r.Render(id, samples[id])
		if err != nil {
			fail(err)
		}
		write(filepath.Join(dir, string(id)+".html"), e.HTML)
		write(filepath.Join(dir, string(id)+".txt"), "Subject: "+e.Subject+"\n\n"+e.Text)
		index += fmt.Sprintf(`<li><a href="%s.html">%s</a> (<a href="%s.txt">text</a>): %s</li>`, id, id, id, html.EscapeString(e.Subject))
	}
	write(filepath.Join(dir, "index.html"), index+"</ul>")
	fmt.Println("previews written to", filepath.Join(dir, "index.html"))
}

func write(path, s string) {
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "emailpreview:", err)
	os.Exit(1)
}
