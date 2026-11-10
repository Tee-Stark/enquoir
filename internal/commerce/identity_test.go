package commerce

import (
	"strings"
	"testing"
)

func TestNewExternalID(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    ExternalID
		wantErr string
	}{
		{name: "kept as given", in: "cust_42", want: "cust_42"},
		{name: "trimmed", in: "  cust_42 \n", want: "cust_42"},
		{name: "case is significant", in: "Cust_42", want: "Cust_42"},
		{name: "inner punctuation allowed", in: "shop-1:user/42@eu", want: "shop-1:user/42@eu"},
		{name: "longest allowed", in: strings.Repeat("a", 255), want: ExternalID(strings.Repeat("a", 255))},
		{name: "empty", in: "", wantErr: "required"},
		{name: "blank", in: "   ", wantErr: "required"},
		{name: "too long", in: strings.Repeat("a", 256), wantErr: "at most 255"},
		{name: "inner space", in: "cust 42", wantErr: "spaces or control characters"},
		{name: "control character", in: "cust\x0042", wantErr: "spaces or control characters"},
		{name: "not UTF-8", in: "cust\xff", wantErr: "UTF-8"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewExternalID(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("NewExternalID(%q) = %q, %v; want an error containing %q", tc.in, got, err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("NewExternalID(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}
}

func TestParseStatus(t *testing.T) {
	for in, want := range map[string]Status{"active": Active, "disabled": Disabled, " Disabled ": Disabled} {
		if got, err := ParseStatus(in); err != nil || got != want {
			t.Errorf("ParseStatus(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "revoked", "enabled"} {
		if _, err := ParseStatus(in); err == nil {
			t.Errorf("ParseStatus(%q) accepted an unknown status", in)
		}
	}
}

func TestStatusActive(t *testing.T) {
	if !Active.IsActive() || Disabled.IsActive() {
		t.Error("only Active is active")
	}
}
