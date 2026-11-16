package commerce

import (
	"errors"
	"strings"
	"testing"
	"time"

	"enquoir/internal/domain"
)

var now = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)

func TestParseApprovalMode(t *testing.T) {
	for in, want := range map[string]ApprovalMode{"autonomous": Autonomous, " Per_Transaction ": PerTransaction} {
		if got, err := ParseApprovalMode(in); err != nil || got != want {
			t.Errorf("ParseApprovalMode(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "auto", "manual"} {
		if _, err := ParseApprovalMode(in); err == nil {
			t.Errorf("ParseApprovalMode(%q) accepted", in)
		}
	}
}

func TestNewMandateScope(t *testing.T) {
	s, err := NewMandateScope("usd", 150000, 300000, 3, []string{" Electronics", "books", "electronics"})
	if err != nil {
		t.Fatal(err)
	}
	want := MandateScope{Currency: domain.USD, MaxPerTransactionMinor: 150000, MaxTotalMinor: 300000, MaxTransactions: 3, AllowedCategories: []string{"books", "electronics"}}
	if s.Currency != want.Currency || s.MaxPerTransactionMinor != want.MaxPerTransactionMinor || s.MaxTotalMinor != want.MaxTotalMinor ||
		s.MaxTransactions != want.MaxTransactions || strings.Join(s.AllowedCategories, ",") != "books,electronics" {
		t.Errorf("scope = %+v, want %+v", s, want)
	}
	if s, err := NewMandateScope("GBP", 100, 100, 0, nil); err != nil || s.MaxTransactions != 0 || s.AllowedCategories == nil {
		t.Errorf("no count, no categories: %+v, %v (categories must be empty, not nil)", s, err)
	}

	bad := []struct {
		name         string
		currency     string
		perTx, total int64
		count        int
		categories   []string
		field        string
	}{
		{"unsupported currency", "NGN", 100, 100, 0, nil, "scope.currency"},
		{"zero per transaction", "USD", 0, 100, 0, nil, "scope.max_transaction_minor"},
		{"negative total", "USD", 100, -1, 0, nil, "scope.max_cumulative_minor"},
		{"per transaction above total", "USD", 200, 100, 0, nil, "scope.max_cumulative_minor"},
		{"negative count", "USD", 100, 100, -1, nil, "scope.max_transactions"},
		{"absurd count", "USD", 100, 100, 100001, nil, "scope.max_transactions"},
		{"bad category", "USD", 100, 100, 0, []string{"home & garden"}, "scope.allowed_categories"},
		{"blank category", "USD", 100, 100, 0, []string{" "}, "scope.allowed_categories"},
		{"too many categories", "USD", 100, 100, 0, make([]string, 51), "scope.allowed_categories"},
	}
	for _, tc := range bad {
		_, err := NewMandateScope(domain.Currency(tc.currency), tc.perTx, tc.total, tc.count, tc.categories)
		var ferr *FieldError
		if !errors.As(err, &ferr) || ferr.Field != tc.field {
			t.Errorf("%s: %v, want an error on %s", tc.name, err, tc.field)
		}
	}
}

func testTerms(t *testing.T) MandateTerms {
	t.Helper()
	scope, err := NewMandateScope(domain.USD, 150000, 300000, 3, []string{"electronics"})
	if err != nil {
		t.Fatal(err)
	}
	terms, err := NewMandateTerms(MandateParties{
		OrgID: "org_1", MerchantID: "mi_1", MerchantName: "Acme Electronics", CustomerID: "cus_1",
		AgentID: "cagt_1", AgentName: "Acme Shopping Assistant",
		FundingMethodID: "fmb_1", FundingDisplay: FundingDisplay{Brand: "visa", LastFour: "4242"},
	}, Autonomous, scope, now.Add(30*24*time.Hour+500*time.Millisecond), now)
	if err != nil {
		t.Fatal(err)
	}
	return terms
}

func TestNewMandateTerms_ExpiryBounds(t *testing.T) {
	terms := testTerms(t)
	if !terms.ExpiresAt.Equal(now.Add(30*24*time.Hour)) || terms.ExpiresAt.Location() != time.UTC {
		t.Errorf("expires_at = %v, want truncated to the second, in UTC", terms.ExpiresAt)
	}
	scope := terms.Scope
	for name, at := range map[string]time.Time{
		"in the past":  now.Add(-time.Second),
		"now":          now,
		"over 90 days": now.Add(90*24*time.Hour + time.Second),
		"zero":         {},
	} {
		_, err := NewMandateTerms(terms.Parties, Autonomous, scope, at, now)
		var ferr *FieldError
		if !errors.As(err, &ferr) || ferr.Field != "expires_at" {
			t.Errorf("expiry %s: %v, want an error on expires_at", name, err)
		}
	}
	if _, err := NewMandateTerms(terms.Parties, Autonomous, scope, now.Add(MaxMandateDuration), now); err != nil {
		t.Errorf("exactly 90 days: %v", err)
	}
}

// The hash binds every term the customer is shown. Change any one and
// it changes; build the same terms twice and it doesn't.
func TestMandateTerms_HashBindsEveryTerm(t *testing.T) {
	base := testTerms(t)
	if base.Hash() != testTerms(t).Hash() || !strings.HasPrefix(base.Hash(), "sha256:") || len(base.Hash()) != 7+64 {
		t.Fatalf("hash %q is not stable sha256", base.Hash())
	}
	changes := map[string]func(*MandateTerms){
		"merchant name":   func(m *MandateTerms) { m.Parties.MerchantName = "Acme" },
		"customer":        func(m *MandateTerms) { m.Parties.CustomerID = "cus_2" },
		"agent":           func(m *MandateTerms) { m.Parties.AgentID = "cagt_2" },
		"agent name":      func(m *MandateTerms) { m.Parties.AgentName = "Bot" },
		"funding method":  func(m *MandateTerms) { m.Parties.FundingMethodID = "fmb_2" },
		"card shown":      func(m *MandateTerms) { m.Parties.FundingDisplay.LastFour = "0005" },
		"approval mode":   func(m *MandateTerms) { m.ApprovalMode = PerTransaction },
		"currency":        func(m *MandateTerms) { m.Scope.Currency = domain.EUR },
		"per transaction": func(m *MandateTerms) { m.Scope.MaxPerTransactionMinor++ },
		"total":           func(m *MandateTerms) { m.Scope.MaxTotalMinor++ },
		"count":           func(m *MandateTerms) { m.Scope.MaxTransactions = 0 },
		"categories":      func(m *MandateTerms) { m.Scope.AllowedCategories = []string{"books"} },
		"expiry":          func(m *MandateTerms) { m.ExpiresAt = m.ExpiresAt.Add(time.Second) },
		"organisation":    func(m *MandateTerms) { m.Parties.OrgID = "org_2" },
	}
	for name, change := range changes {
		m := testTerms(t)
		change(&m)
		if m.Hash() == base.Hash() {
			t.Errorf("changing the %s left the hash unchanged", name)
		}
	}
}

// The snapshot round-trips: what is stored is exactly what was hashed.
func TestMandateTerms_CanonicalRoundTrips(t *testing.T) {
	terms := testTerms(t)
	back, err := ParseMandateTerms(terms.Canonical())
	if err != nil {
		t.Fatal(err)
	}
	if back.Hash() != terms.Hash() || string(back.Canonical()) != string(terms.Canonical()) {
		t.Errorf("round trip changed the terms:\n%s\n%s", terms.Canonical(), back.Canonical())
	}
	if _, err := ParseMandateTerms([]byte(`{"version":"mandate-terms/v0"}`)); err == nil {
		t.Error("parsed terms of an unknown version")
	}
}

func TestMandateRequest_StatusAt(t *testing.T) {
	r := MandateRequest{Status: RequestPending, ExpiresAt: now.Add(time.Hour)}
	if r.StatusAt(now) != RequestPending || r.StatusAt(now.Add(time.Hour)) != RequestExpired {
		t.Error("a pending request expires at its expiry")
	}
	r.Status = RequestApproved
	if r.StatusAt(now.Add(2*time.Hour)) != RequestApproved {
		t.Error("a decided request keeps its decision")
	}
}

// Revoked beats expired beats suspended beats active.
func TestMandate_StatusAt(t *testing.T) {
	m := Mandate{Terms: testTerms(t), Revoked: false}
	all := Dependencies{IntegrationActive: true, CustomerActive: true, AgentActive: true, FundingMethodActive: true}
	if got := m.StatusAt(now, all); got != MandateActive {
		t.Errorf("all active: %s", got)
	}
	for name, deps := range map[string]Dependencies{
		"integration": {CustomerActive: true, AgentActive: true, FundingMethodActive: true},
		"customer":    {IntegrationActive: true, AgentActive: true, FundingMethodActive: true},
		"agent":       {IntegrationActive: true, CustomerActive: true, FundingMethodActive: true},
		"binding":     {IntegrationActive: true, CustomerActive: true, AgentActive: true},
	} {
		if got := m.StatusAt(now, deps); got != MandateSuspended {
			t.Errorf("%s disabled: %s, want suspended", name, got)
		}
	}
	if got := m.StatusAt(m.Terms.ExpiresAt, Dependencies{}); got != MandateExpired {
		t.Errorf("at expiry, suspended too: %s, want expired", got)
	}
	m.Revoked = true
	if got := m.StatusAt(m.Terms.ExpiresAt, Dependencies{}); got != MandateRevoked {
		t.Errorf("revoked and expired: %s, want revoked", got)
	}
}

// A revocation waits for the customer's confirmation. Meanwhile the
// mandate grants nothing; unconfirmed, the request lapses after an hour.
func TestMandate_StatusWhileARevocationAwaitsConfirmation(t *testing.T) {
	all := Dependencies{IntegrationActive: true, CustomerActive: true, AgentActive: true, FundingMethodActive: true}
	m := Mandate{Terms: testTerms(t)}
	m.RequestRevocation(now)
	if got := m.StatusAt(now.Add(59*time.Minute), all); got != MandateRevocationPending {
		t.Errorf("within the hour: %s, want revocation_pending", got)
	}
	if got := m.StatusAt(now.Add(time.Hour), all); got != MandateActive {
		t.Errorf("after the hour, unconfirmed: %s, want active again", got)
	}
	if got := m.StatusAt(now.Add(time.Minute), Dependencies{}); got != MandateRevocationPending {
		t.Errorf("pending beats suspended: %s", got)
	}
	if !m.RevocationPendingAt(now) || m.RevocationPendingAt(now.Add(time.Hour)) {
		t.Error("RevocationPendingAt disagrees with the hour")
	}
	if got, err := ParseMandateStatus("revocation_pending"); err != nil || got != MandateRevocationPending {
		t.Errorf("parse: %s %v", got, err)
	}
}
