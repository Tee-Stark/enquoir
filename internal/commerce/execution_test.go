package commerce

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func pendingExecution() Execution {
	return Execution{
		ID: "exe_1", MandateID: "mdt_1", AmountMinor: 134600, Currency: "USD",
		FundingMethod: FundingMethod{ID: "fmb_1", Provider: "stripe"},
		Status:        ExecutionPending, CreatedAt: now, ReportDeadline: now.Add(ExecutionReportDeadline),
	}
}

func report(src ReportSource, outcome ExecutionStatus, ref string) ExecutionReport {
	return ExecutionReport{Source: src, Outcome: outcome, Provider: "stripe", ProviderPaymentReference: ref, AmountMinor: 134600, Currency: "USD"}
}

func TestExecution_IdempotencyKeyAndDeadline(t *testing.T) {
	e := NewExecution("exe_1", Authorization{ID: "auth_1"}, FundingMethod{}, now)
	if e.IdempotencyKey() != "enquoir-exe_1" || !e.ReportDeadline.Equal(now.Add(30*time.Minute)) {
		t.Errorf("key %q deadline %v", e.IdempotencyKey(), e.ReportDeadline)
	}
}

func TestNewExecutionReport_Validates(t *testing.T) {
	ok := []ExecutionReport{
		report(ReportDirect, ExecutionSucceeded, "pi_123"),
		report(ReportDirect, ExecutionFailed, ""), // declined before a reference
		report(ReportDirect, ExecutionUnknown, ""),
		{Source: ReportWebhook, Outcome: ExecutionSucceeded, Provider: "Stripe", ProviderPaymentReference: "pi_1", EventID: "evt_1", AmountMinor: 1, Currency: "usd"},
	}
	for _, r := range ok {
		if _, err := NewExecutionReport(r); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	bad := map[string]struct {
		r     ExecutionReport
		field string
	}{
		"no outcome":            {ExecutionReport{Source: ReportDirect, Provider: "stripe", AmountMinor: 1, Currency: "USD"}, "outcome"},
		"pending is no outcome": {report(ReportDirect, ExecutionPending, ""), "outcome"},
		"success without a ref": {report(ReportDirect, ExecutionSucceeded, ""), "provider_payment_reference"},
		"webhook without event": {report(ReportWebhook, ExecutionSucceeded, "pi_1"), "provider_event_id"},
		"card as reference":     {report(ReportDirect, ExecutionSucceeded, "4242424242424242"), "provider_payment_reference"},
		"card in the reason": {func() ExecutionReport {
			r := report(ReportDirect, ExecutionFailed, "")
			r.FailureReason = "card 4242 4242 4242 4242 declined"
			return r
		}(), "failure_reason"},
		"no currency": {func() ExecutionReport { r := report(ReportDirect, ExecutionFailed, ""); r.Currency = ""; return r }(), "currency"},
	}
	for name, tc := range bad {
		_, err := NewExecutionReport(tc.r)
		var ferr *FieldError
		if !errors.As(err, &ferr) || ferr.Field != tc.field {
			t.Errorf("%s: %v, want an error on %s", name, err, tc.field)
		}
		if err != nil && strings.Contains(err.Error(), "4242 4242") {
			t.Errorf("%s: the error repeats card digits", name)
		}
	}
}

func TestExecution_Apply(t *testing.T) {
	type step struct {
		r      ExecutionReport
		status ExecutionStatus
		result ReportResult
		err    error
	}
	cases := map[string][]step{
		"pending to succeeded, then a duplicate": {
			{report(ReportDirect, ExecutionSucceeded, "pi_1"), ExecutionSucceeded, ReportApplied, nil},
			{report(ReportDirect, ExecutionSucceeded, "pi_1"), ExecutionSucceeded, ReportDuplicate, nil},
		},
		"a finished payment can't be contradicted": {
			{report(ReportDirect, ExecutionSucceeded, "pi_1"), ExecutionSucceeded, ReportApplied, nil},
			{report(ReportReconciliation, ExecutionFailed, "pi_1"), ExecutionSucceeded, "", ErrOutcomeConflict},
		},
		"a late unknown after the end is ignored": {
			{report(ReportDirect, ExecutionFailed, ""), ExecutionFailed, ReportApplied, nil},
			{report(ReportDirect, ExecutionUnknown, ""), ExecutionFailed, ReportStale, nil},
		},
		"unknown is not failed: only reconciliation or a webhook resolves it": {
			{report(ReportDirect, ExecutionUnknown, ""), ExecutionUnknown, ReportApplied, nil},
			{report(ReportDirect, ExecutionUnknown, ""), ExecutionUnknown, ReportDuplicate, nil},
			{report(ReportDirect, ExecutionSucceeded, "pi_1"), ExecutionUnknown, "", ErrReconciliationRequired},
			{report(ReportReconciliation, ExecutionSucceeded, "pi_1"), ExecutionSucceeded, ReportApplied, nil},
		},
		"a webhook resolves unknown": {
			{report(ReportDirect, ExecutionUnknown, ""), ExecutionUnknown, ReportApplied, nil},
			{func() ExecutionReport {
				r := report(ReportWebhook, ExecutionFailed, "pi_1")
				r.EventID = "evt_1"
				return r
			}(), ExecutionFailed, ReportApplied, nil},
		},
		"the reference, once known, can't change": {
			{report(ReportDirect, ExecutionUnknown, "pi_1"), ExecutionUnknown, ReportApplied, nil},
			{report(ReportReconciliation, ExecutionSucceeded, "pi_2"), ExecutionUnknown, "", ErrReportMismatch},
		},
	}
	for name, steps := range cases {
		e := pendingExecution()
		for i, s := range steps {
			r, err := NewExecutionReport(s.r)
			if err != nil {
				t.Fatalf("%s step %d: %v", name, i, err)
			}
			next, result, err := e.Apply(r, now.Add(time.Minute))
			if !errors.Is(err, s.err) || (err == nil && (next.Status != s.status || result != s.result)) {
				t.Errorf("%s step %d: %s %s %v, want %s %s %v", name, i, next.Status, result, err, s.status, s.result, s.err)
			}
			if err == nil {
				e = next
			}
		}
	}

	e := pendingExecution()
	for name, change := range map[string]func(*ExecutionReport){
		"another amount":   func(r *ExecutionReport) { r.AmountMinor-- },
		"another currency": func(r *ExecutionReport) { r.Currency = "EUR" },
		"another provider": func(r *ExecutionReport) { r.Provider = "adyen" },
	} {
		r := report(ReportDirect, ExecutionSucceeded, "pi_1")
		change(&r)
		r, _ = NewExecutionReport(r)
		if _, _, err := e.Apply(r, now); !errors.Is(err, ErrReportMismatch) {
			t.Errorf("%s: %v", name, err)
		}
	}
	got, _, _ := e.Apply(must(NewExecutionReport(report(ReportDirect, ExecutionSucceeded, "pi_9"))), now.Add(time.Minute))
	if got.ProviderPaymentReference != "pi_9" || got.FinishedAt == nil || !got.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Errorf("applied: %+v", got)
	}
}

func must(r ExecutionReport, err error) ExecutionReport {
	if err != nil {
		panic(err)
	}
	return r
}

func TestExecutionReport_Fingerprint(t *testing.T) {
	a := must(NewExecutionReport(ExecutionReport{Source: ReportWebhook, Outcome: ExecutionSucceeded, Provider: "stripe", ProviderPaymentReference: "pi_1", EventID: "evt_1", AmountMinor: 5, Currency: "USD"}))
	b := a
	if a.Fingerprint() != b.Fingerprint() {
		t.Error("unstable")
	}
	b.Outcome = ExecutionFailed
	if a.Fingerprint() == b.Fingerprint() {
		t.Error("another outcome, same fingerprint")
	}
}

func TestCheckout_CountsAfterPayment(t *testing.T) {
	c := Checkout{Status: CheckoutPaid}
	if !c.CountsAt(now) {
		t.Error("a paid checkout counts")
	}
	c.Status = CheckoutPaymentFailed
	if c.CountsAt(now) {
		t.Error("a failed payment is released")
	}
}
