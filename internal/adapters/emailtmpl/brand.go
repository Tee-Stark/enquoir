package emailtmpl

import "html/template"

// Brand is every visual choice in Enquoir's emails, in one place: a
// rebrand changes this file and nothing else. These are placeholders
// until the brand is decided.
type Brand struct {
	Name       string // product name, shown as the wordmark
	Tagline    string
	Ink        string // header and button background
	Accent     string // the wordmark's accent mark and links
	Text       string // body text
	Muted      string // secondary text
	Background string // page behind the card
	Card       string
	Border     string
	// Font is a CSS font stack; template.CSS because it is a trusted
	// constant with quotes, which html/template would otherwise reject.
	Font template.CSS
}

// DefaultBrand is the placeholder look: deep ink, a teal accent, a
// plain system font stack.
var DefaultBrand = Brand{
	Name:       "Enquoir",
	Tagline:    "Safe control for agentic finance",
	Ink:        "#0B1F33",
	Accent:     "#14B8A6",
	Text:       "#1F2937",
	Muted:      "#6B7280",
	Background: "#F3F5F8",
	Card:       "#FFFFFF",
	Border:     "#E5E7EB",
	Font:       "-apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif",
}
