package commerceapp_test

import (
	"testing"
	"time"

	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

var replayT0 = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

func replayAt(ms int) time.Time { return replayT0.Add(time.Duration(ms) * time.Millisecond) }

func TestBuildCheckoutReplay_DeniedWithNoGrant(t *testing.T) {
	c := commerce.Checkout{
		ID:        "chk_1",
		CreatedAt: replayAt(0),
		DecidedAt: ptrTime(replayAt(5)),
		Denials:   []commerce.Denial{{Code: commerce.DenialOverBudget}},
	}
	r := commerceapp.BuildCheckoutReplay(c, nil, nil, replayAt(1000))
	if len(r.Steps) != 2 {
		t.Fatalf("steps = %+v", r.Steps)
	}
	if r.Steps[0].Type != commerceapp.CheckoutStepSubmission {
		t.Errorf("step 0 = %s", r.Steps[0].Type)
	}
	d := r.Steps[1]
	if d.Type != commerceapp.CheckoutStepDecision || d.Decision == nil || d.Decision.Status != commerce.CheckoutDenied || d.DeltaMS != 5 {
		t.Errorf("step 1 = %+v", d)
	}
	if d.Actor != commerceapp.CheckoutActorEnquoir {
		t.Errorf("actor = %s", d.Actor)
	}
}

func TestBuildCheckoutReplay_AuthorizedThenExpiredUngranted(t *testing.T) {
	exp := replayAt(900000) // 15 minutes
	c := commerce.Checkout{
		ID: "chk_2", CreatedAt: replayAt(0), DecidedAt: ptrTime(replayAt(5)),
		Authorization: &commerce.Authorization{ID: "auth_1", CheckoutID: "chk_2", ExpiresAt: exp},
	}
	// Before expiry: just submission + decision.
	before := commerceapp.BuildCheckoutReplay(c, nil, nil, replayAt(5))
	if len(before.Steps) != 2 {
		t.Fatalf("before expiry: %+v", before.Steps)
	}
	// After expiry: a terminal "expired" decision step is added.
	after := commerceapp.BuildCheckoutReplay(c, nil, nil, exp.Add(time.Second))
	if len(after.Steps) != 3 {
		t.Fatalf("after expiry: %+v", after.Steps)
	}
	last := after.Steps[2]
	if last.Type != commerceapp.CheckoutStepDecision || last.Decision == nil || last.Decision.Status != commerce.CheckoutExpired || !last.At.Equal(exp) {
		t.Errorf("expired step = %+v", last)
	}
}

func TestBuildCheckoutReplay_FullLifecycleToSucceededReport(t *testing.T) {
	c := commerce.Checkout{
		ID: "chk_3", CreatedAt: replayAt(0), DecidedAt: ptrTime(replayAt(5)),
		Authorization: &commerce.Authorization{
			ID: "auth_1", CheckoutID: "chk_3", GrantIssuedAt: ptrTime(replayAt(10)),
			RedeemedAt: ptrTime(replayAt(100)), ExecutionID: "exe_1",
		},
	}
	e := commerce.Execution{ID: "exe_1", CheckoutID: "chk_3", Status: commerce.ExecutionSucceeded}
	history := []commerce.ExecutionEvent{
		{Sequence: 1, Kind: commerce.EventRedeemed, StatusAfter: commerce.ExecutionPending, At: replayAt(100)},
		{Sequence: 2, Kind: commerce.EventReported, Source: commerce.ReportDirect, Outcome: commerce.ExecutionSucceeded, StatusAfter: commerce.ExecutionSucceeded, At: replayAt(200)},
	}
	r := commerceapp.BuildCheckoutReplay(c, &e, history, replayAt(1000))
	if len(r.Steps) != 5 {
		t.Fatalf("steps = %+v", r.Steps)
	}
	wantTypes := []string{
		commerceapp.CheckoutStepSubmission, commerceapp.CheckoutStepDecision, commerceapp.CheckoutStepGrant,
		commerceapp.CheckoutStepRedemption, commerceapp.CheckoutStepExecutionReport,
	}
	for i, want := range wantTypes {
		if r.Steps[i].Type != want {
			t.Errorf("step %d = %s, want %s", i, r.Steps[i].Type, want)
		}
	}
	redemption := r.Steps[3]
	if redemption.Actor != commerceapp.CheckoutActorMerchant || redemption.ExecutionEvent == nil || redemption.ExecutionEvent.Kind != commerce.EventRedeemed {
		t.Errorf("redemption step = %+v", redemption)
	}
	report := r.Steps[4]
	if report.Actor != commerceapp.CheckoutActorMerchant || report.DeltaMS != 100 {
		t.Errorf("report step = %+v", report)
	}
	if r.DurationMS != 200 || r.Execution == nil || r.Execution.ID != "exe_1" {
		t.Errorf("replay = %+v", r)
	}
}

// A PSP webhook that doesn't match the execution's own record is
// recorded as its own refused step, and attributed to the provider.
func TestBuildCheckoutReplay_RefusedWebhookReportIsItsOwnStep(t *testing.T) {
	c := commerce.Checkout{
		ID: "chk_4", CreatedAt: replayAt(0), DecidedAt: ptrTime(replayAt(5)),
		Authorization: &commerce.Authorization{ID: "auth_1", CheckoutID: "chk_4", RedeemedAt: ptrTime(replayAt(10)), ExecutionID: "exe_1"},
	}
	e := commerce.Execution{ID: "exe_1", CheckoutID: "chk_4", Status: commerce.ExecutionPending}
	history := []commerce.ExecutionEvent{
		{Sequence: 1, Kind: commerce.EventRedeemed, At: replayAt(10)},
		{Sequence: 2, Kind: commerce.EventRefused, Source: commerce.ReportWebhook, Detail: "amount mismatch", At: replayAt(50)},
	}
	r := commerceapp.BuildCheckoutReplay(c, &e, history, replayAt(1000))
	refused := r.Steps[len(r.Steps)-1]
	if refused.Type != commerceapp.CheckoutStepExecutionReport || refused.Actor != commerceapp.CheckoutActorProvider || refused.Title != "Report refused: amount mismatch" {
		t.Errorf("refused step = %+v", refused)
	}
}

// Confirming a mandate's revocation revokes an unredeemed grant - a
// distinct decision step from cancellation, since nobody cancelled it.
func TestBuildCheckoutReplay_MandateRevocationCascade(t *testing.T) {
	c := commerce.Checkout{
		ID: "chk_5", CreatedAt: replayAt(0), DecidedAt: ptrTime(replayAt(5)),
		Authorization: &commerce.Authorization{ID: "auth_1", CheckoutID: "chk_5", GrantIssuedAt: ptrTime(replayAt(10)), RevokedAt: ptrTime(replayAt(500))},
	}
	r := commerceapp.BuildCheckoutReplay(c, nil, nil, replayAt(1000))
	if len(r.Steps) != 4 {
		t.Fatalf("steps = %+v", r.Steps)
	}
	revoked := r.Steps[3]
	if revoked.Type != commerceapp.CheckoutStepDecision || revoked.Decision == nil || revoked.Decision.Status != commerce.CheckoutRevoked || revoked.Actor != commerceapp.CheckoutActorEnquoir {
		t.Errorf("revoked step = %+v", revoked)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
