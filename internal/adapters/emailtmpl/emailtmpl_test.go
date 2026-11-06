package emailtmpl

import (
	"strings"
	"testing"

	"enquoir/internal/app"
)

var sample = map[app.TemplateID]map[string]string{
	app.TemplateVerifyEmail:     {"link": "https://enquoir.etimi.me/verify-email?token=t1"},
	app.TemplateInvitation:      {"link": "https://enquoir.etimi.me/invitations/accept?token=t2", "organisation": "Acme Treasury", "inviter": "ada@example.com", "role": "operator"},
	app.TemplatePasswordReset:   {"link": "https://enquoir.etimi.me/reset-password?token=t3"},
	app.TemplatePasswordChanged: {},
}

func TestEveryTemplateRendersSubjectHTMLAndText(t *testing.T) {
	r := New()
	for id, params := range sample {
		t.Run(string(id), func(t *testing.T) {
			e, err := r.Render(id, params)
			if err != nil {
				t.Fatal(err)
			}
			if e.Subject == "" || !strings.Contains(e.HTML, "<html") || e.Text == "" {
				t.Errorf("rendered = %+v", e)
			}
			if link := params["link"]; link != "" {
				if !strings.Contains(e.HTML, `href="`+link+`"`) || !strings.Contains(e.Text, link) {
					t.Errorf("the link is not both a button and written out in HTML and text")
				}
			}
			// ZgotmplZ is html/template's mark for a value it refused.
			if strings.Contains(e.HTML, "{{") || strings.Contains(e.Text, "{{") || strings.Contains(e.HTML, "no value") || strings.Contains(e.HTML, "ZgotmplZ") {
				t.Error("unrendered template syntax in the output")
			}
		})
	}
}

func TestInvitationNamesTheOrganisationInviterAndRole(t *testing.T) {
	e, _ := New().Render(app.TemplateInvitation, sample[app.TemplateInvitation])
	for _, want := range []string{"Acme Treasury", "ada@example.com", "operator"} {
		if !strings.Contains(e.Subject+e.HTML, want) || !strings.Contains(e.Text, want) {
			t.Errorf("invitation does not mention %q", want)
		}
	}
}

// Organisation names and inviter emails are chosen by users: they must
// never become markup in someone else's inbox.
func TestValuesAreEscaped(t *testing.T) {
	params := map[string]string{"link": "https://enquoir.etimi.me/x?token=t", "organisation": `<a href="https://evil.example">Click</a>`, "inviter": "x@y.co", "role": "viewer"}
	e, err := New().Render(app.TemplateInvitation, params)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.HTML, `<a href="https://evil.example">`) {
		t.Error("an organisation name was rendered as markup")
	}
	if strings.ContainsAny(e.Subject, "\r\n") {
		t.Error("the subject has a line break")
	}
}

func TestMissingParamsAndUnknownTemplatesFail(t *testing.T) {
	if _, err := New().Render(app.TemplateInvitation, map[string]string{"link": "https://x"}); err == nil {
		t.Error("an invitation without an organisation rendered")
	}
	if _, err := New().Render("welcome_aboard", nil); err == nil {
		t.Error("an unknown template rendered")
	}
}

// Links are always https into the app, never javascript: or similar.
func TestNonHTTPSLinksAreRefused(t *testing.T) {
	for _, link := range []string{"javascript:alert(1)", "http://enquoir.etimi.me/x"} {
		if _, err := New().Render(app.TemplateVerifyEmail, map[string]string{"link": link}); err == nil {
			t.Errorf("link %q rendered", link)
		}
	}
}
