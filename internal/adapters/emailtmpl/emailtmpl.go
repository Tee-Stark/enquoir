// Package emailtmpl renders Enquoir's emails from templates embedded in
// the binary. A template is named by its app.TemplateID; nothing about
// it is stored anywhere else.
package emailtmpl

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	"net/url"
	"strings"
	texttemplate "text/template"

	"enquoir/internal/app"
)

//go:embed templates/*.tmpl
var files embed.FS

var _ app.EmailRenderer = (*Renderer)(nil)

// Renderer renders the embedded templates with a Brand.
type Renderer struct {
	brand Brand
	html  map[app.TemplateID]*htmltemplate.Template
	text  map[app.TemplateID]*texttemplate.Template
}

// Templates is every template ID, for previews and checks.
var Templates = []app.TemplateID{app.TemplateVerifyEmail, app.TemplateInvitation, app.TemplatePasswordReset, app.TemplatePasswordChanged,
	app.TemplateMandateConsent, app.TemplateMandateRevocation, app.TemplateCheckoutApproval}

// New parses every embedded template with DefaultBrand. A broken
// template panics here, at start-up, never mid-send.
func New() *Renderer { return NewWithBrand(DefaultBrand) }

func NewWithBrand(b Brand) *Renderer {
	r := &Renderer{brand: b, html: map[app.TemplateID]*htmltemplate.Template{}, text: map[app.TemplateID]*texttemplate.Template{}}
	for _, id := range Templates {
		r.html[id] = htmltemplate.Must(htmltemplate.New("layout.html.tmpl").Option("missingkey=error").
			ParseFS(files, "templates/layout.html.tmpl", "templates/"+string(id)+".html.tmpl"))
		r.text[id] = texttemplate.Must(texttemplate.New("layout.txt.tmpl").Option("missingkey=error").
			ParseFS(files, "templates/layout.txt.tmpl", "templates/"+string(id)+".txt.tmpl"))
	}
	return r
}

type data struct {
	Brand Brand
	P     map[string]string
}

func (r *Renderer) Render(id app.TemplateID, params map[string]string) (app.RenderedEmail, error) {
	h, ok := r.html[id]
	if !ok {
		return app.RenderedEmail{}, fmt.Errorf("emailtmpl: unknown template %q", id)
	}
	if params == nil {
		params = map[string]string{}
	}
	if _, has := params["link"]; !has {
		params["link"] = "" // optional: templates test it with if
	}
	if link := params["link"]; link != "" && !safeLink(link) {
		return app.RenderedEmail{}, fmt.Errorf("emailtmpl: refusing link %q: must be https (or http on localhost)", link)
	}
	d := data{Brand: r.brand, P: params}

	var html, text, subject bytes.Buffer
	if err := h.Execute(&html, d); err != nil {
		return app.RenderedEmail{}, fmt.Errorf("emailtmpl: %s html: %w", id, err)
	}
	if err := r.text[id].Execute(&text, d); err != nil {
		return app.RenderedEmail{}, fmt.Errorf("emailtmpl: %s text: %w", id, err)
	}
	if err := r.text[id].ExecuteTemplate(&subject, "subject", d); err != nil {
		return app.RenderedEmail{}, fmt.Errorf("emailtmpl: %s subject: %w", id, err)
	}
	return app.RenderedEmail{
		Subject: strings.Join(strings.Fields(subject.String()), " "),
		HTML:    html.String(),
		Text:    strings.TrimSpace(text.String()) + "\n",
	}, nil
}

func safeLink(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	switch u.Scheme {
	case "https":
		return true
	case "http":
		h := u.Hostname()
		return h == "localhost" || h == "127.0.0.1"
	}
	return false
}
