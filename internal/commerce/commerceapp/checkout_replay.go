package commerceapp

import (
	"context"
	"sort"
	"strings"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/domain"
)

// Checkout replay step types.
const (
	CheckoutStepSubmission      = "submission"       // the merchant submitted the checkout
	CheckoutStepDecision        = "decision"         // authorized, denied, rejected, canceled, revoked or expired
	CheckoutStepGrant           = "grant"            // a grant was issued
	CheckoutStepRedemption      = "redemption"       // the grant was redeemed; an execution was created
	CheckoutStepExecutionReport = "execution_report" // a later report on that execution
)

// Actors in a checkout replay. The merchant's own backend is the only
// machine caller for checkout mutations (AGENTS.md: "Three authorities
// stay separate" / "the merchant backend is the only machine caller for
// now") - a checkout's named agent is never a credentialed caller the
// way a treasury agent is, so it is never an actor here, only ever
// named in a step's title if at all.
const (
	CheckoutActorMerchant = "merchant"
	CheckoutActorEnquoir  = "enquoir"
	CheckoutActorProvider = "provider"
)

// CheckoutDecisionDetail is the outcome of a "decision" step.
type CheckoutDecisionDetail struct {
	Status  commerce.CheckoutStatus
	Reasons []commerce.Denial
}

// CheckoutReplayStep is one moment in a checkout's life, ready to
// animate - the checkout counterpart of app.ReplayStep.
type CheckoutReplayStep struct {
	Index          int
	At             time.Time
	OffsetMS       int64 // since the first step
	DeltaMS        int64 // since the previous step
	Type           string
	Actor          string
	Title          string
	Decision       *CheckoutDecisionDetail  // for "decision" steps
	ExecutionEvent *commerce.ExecutionEvent // for "redemption"/"execution_report" steps
}

// CheckoutReplay is every step a checkout went through, in order.
type CheckoutReplay struct {
	Checkout   CheckoutView
	Execution  *commerce.Execution
	StartedAt  time.Time
	EndedAt    time.Time
	DurationMS int64
	Steps      []CheckoutReplayStep
}

// BuildCheckoutReplay merges a checkout's lifecycle timestamps -
// submission, decision, cancellation or revocation, grant issuance -
// with its execution's report history (if it has one) into one
// timeline, the same way app.BuildReplay merges a payment's events and
// attempts. now resolves whether an unredeemed, undecided or
// unanswered checkout should show as having expired.
//
// Known gap: a grant *reissue* overwrites Authorization.GrantIssuedAt
// in place - there is no commerce_grant_events history table, so a
// checkout whose grant was reissued only ever shows the latest
// grant-issued moment, never the first.
func BuildCheckoutReplay(c commerce.Checkout, e *commerce.Execution, history []commerce.ExecutionEvent, now time.Time) CheckoutReplay {
	r := CheckoutReplay{Checkout: checkoutView(c, now), Execution: e}

	type entry struct {
		step     CheckoutReplayStep
		priority int
		seq      int
	}
	var entries []entry
	entries = append(entries, entry{
		step: CheckoutReplayStep{At: c.CreatedAt, Type: CheckoutStepSubmission, Actor: CheckoutActorMerchant, Title: "Merchant submitted the checkout"},
	})
	if s, ok := decisionStep(c); ok {
		entries = append(entries, entry{step: s, priority: 1})
	}
	if s, ok := expiredUndecidedStep(c, now); ok {
		entries = append(entries, entry{step: s, priority: 1})
	}
	if s, ok := canceledStep(c); ok {
		entries = append(entries, entry{step: s, priority: 1})
	}
	if s, ok := revokedStep(c); ok {
		entries = append(entries, entry{step: s, priority: 1})
	}
	if s, ok := grantExpiredStep(c, now); ok {
		entries = append(entries, entry{step: s, priority: 1})
	}
	if c.Authorization != nil && c.Authorization.GrantIssuedAt != nil {
		entries = append(entries, entry{
			step:     CheckoutReplayStep{At: *c.Authorization.GrantIssuedAt, Type: CheckoutStepGrant, Actor: CheckoutActorEnquoir, Title: "Grant issued"},
			priority: 2,
		})
	}
	redeemed := false
	for i := range history {
		ev := history[i]
		if ev.Kind == commerce.EventRedeemed && !redeemed {
			redeemed = true
			entries = append(entries, entry{
				step:     CheckoutReplayStep{At: ev.At, Type: CheckoutStepRedemption, Actor: CheckoutActorMerchant, Title: "Grant redeemed; execution created", ExecutionEvent: &ev},
				priority: 3, seq: i,
			})
			continue
		}
		entries = append(entries, entry{
			step:     CheckoutReplayStep{At: ev.At, Type: CheckoutStepExecutionReport, Actor: reportActor(ev.Source), Title: executionReportTitle(ev), ExecutionEvent: &ev},
			priority: 4, seq: i,
		})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if !a.step.At.Equal(b.step.At) {
			return a.step.At.Before(b.step.At)
		}
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		return a.seq < b.seq
	})

	for i, en := range entries {
		s := en.step
		s.Index = i
		if i > 0 {
			s.OffsetMS = s.At.Sub(entries[0].step.At).Milliseconds()
			s.DeltaMS = s.At.Sub(r.Steps[i-1].At).Milliseconds()
		}
		r.Steps = append(r.Steps, s)
	}
	if len(r.Steps) > 0 {
		r.StartedAt, r.EndedAt = r.Steps[0].At, r.Steps[len(r.Steps)-1].At
		r.DurationMS = r.EndedAt.Sub(r.StartedAt).Milliseconds()
	}
	return r
}

// decisionStep is the checkout's initial decision, if it has been
// decided. c.Status may have since moved on (redeemed, paid, ...), so
// the decision itself is inferred from what the decision left behind:
// an authorization means it was authorized; recorded denials mean it
// was denied; neither means the customer rejected it.
func decisionStep(c commerce.Checkout) (CheckoutReplayStep, bool) {
	if c.DecidedAt == nil {
		return CheckoutReplayStep{}, false
	}
	s := CheckoutReplayStep{At: *c.DecidedAt, Type: CheckoutStepDecision}
	switch {
	case c.Authorization != nil:
		s.Decision = &CheckoutDecisionDetail{Status: commerce.CheckoutAuthorized}
		s.Actor, s.Title = CheckoutActorEnquoir, "Authorised against the mandate"
	case len(c.Denials) > 0:
		s.Decision = &CheckoutDecisionDetail{Status: commerce.CheckoutDenied, Reasons: c.Denials}
		s.Actor, s.Title = CheckoutActorEnquoir, "Denied: "+denialSummary(c.Denials)
	default:
		s.Decision = &CheckoutDecisionDetail{Status: commerce.CheckoutRejected}
		s.Actor, s.Title = CheckoutActorEnquoir, "Rejected by the customer"
	}
	return s, true
}

// expiredUndecidedStep is a checkout left awaiting the customer that
// nobody ever answered.
func expiredUndecidedStep(c commerce.Checkout, now time.Time) (CheckoutReplayStep, bool) {
	if c.DecidedAt != nil || c.ApprovalExpiresAt == nil || !now.After(*c.ApprovalExpiresAt) {
		return CheckoutReplayStep{}, false
	}
	return CheckoutReplayStep{
		At: *c.ApprovalExpiresAt, Type: CheckoutStepDecision, Actor: CheckoutActorEnquoir, Title: "Expired: the customer never responded",
		Decision: &CheckoutDecisionDetail{Status: commerce.CheckoutExpired},
	}, true
}

// grantExpiredStep is an authorized, unredeemed grant that ran out.
func grantExpiredStep(c commerce.Checkout, now time.Time) (CheckoutReplayStep, bool) {
	a := c.Authorization
	if a == nil || a.RedeemedAt != nil || a.RevokedAt != nil || !now.After(a.ExpiresAt) {
		return CheckoutReplayStep{}, false
	}
	return CheckoutReplayStep{
		At: a.ExpiresAt, Type: CheckoutStepDecision, Actor: CheckoutActorEnquoir, Title: "Grant expired, unredeemed",
		Decision: &CheckoutDecisionDetail{Status: commerce.CheckoutExpired},
	}, true
}

// canceledStep is the merchant cancelling an authorized or awaiting
// checkout.
func canceledStep(c commerce.Checkout) (CheckoutReplayStep, bool) {
	if c.CanceledAt == nil {
		return CheckoutReplayStep{}, false
	}
	return CheckoutReplayStep{
		At: *c.CanceledAt, Type: CheckoutStepDecision, Actor: CheckoutActorMerchant, Title: "Canceled by the merchant",
		Decision: &CheckoutDecisionDetail{Status: commerce.CheckoutCanceled},
	}, true
}

// revokedStep is the mandate-revocation cascade: distinct from a
// cancellation, which also revokes the authorization at the same
// instant (and is reported by canceledStep instead).
func revokedStep(c commerce.Checkout) (CheckoutReplayStep, bool) {
	a := c.Authorization
	if a == nil || a.RevokedAt == nil || c.CanceledAt != nil {
		return CheckoutReplayStep{}, false
	}
	return CheckoutReplayStep{
		At: *a.RevokedAt, Type: CheckoutStepDecision, Actor: CheckoutActorEnquoir, Title: "Revoked: the mandate was revoked",
		Decision: &CheckoutDecisionDetail{Status: commerce.CheckoutRevoked},
	}, true
}

func denialSummary(d []commerce.Denial) string {
	if len(d) == 0 {
		return "no reason recorded"
	}
	codes := make([]string, len(d))
	for i, x := range d {
		codes[i] = string(x.Code)
	}
	return strings.Join(codes, ", ")
}

// reportActor is who the execution event's report came from: the
// merchant's own call (direct or its own reconciliation lookup), a PSP
// webhook, or Enquoir's own sweep.
func reportActor(src commerce.ReportSource) string {
	switch src {
	case commerce.ReportWebhook:
		return CheckoutActorProvider
	case commerce.ReportSweep:
		return CheckoutActorEnquoir
	default:
		return CheckoutActorMerchant
	}
}

func executionReportTitle(ev commerce.ExecutionEvent) string {
	switch ev.Kind {
	case commerce.EventReported:
		return "Reported " + string(ev.Outcome)
	case commerce.EventDuplicate:
		return "Duplicate report, ignored"
	case commerce.EventIgnored:
		return "Report ignored"
	case commerce.EventRefused:
		if ev.Detail == "" {
			return "Report refused"
		}
		return "Report refused: " + ev.Detail
	case commerce.EventSwept:
		return "Swept: no outcome reported before the deadline"
	default:
		return string(ev.Kind)
	}
}

// ReplayCheckout builds a checkout's replay, composing a GetCheckoutForOrg
// with, if it redeemed a grant, its execution and that execution's
// history - the same three-call composition commerceapp.GetExecution
// already uses for ExecutionDetail.
type ReplayCheckout struct {
	Reader CommerceOrgReader
	Clock  app.Clock
}

func (r *ReplayCheckout) Run(ctx context.Context, org domain.OrgID, id commerce.CheckoutID) (CheckoutReplay, error) {
	c, err := r.Reader.GetCheckoutForOrg(ctx, org, id)
	if err != nil {
		return CheckoutReplay{}, err
	}
	var exec *commerce.Execution
	var history []commerce.ExecutionEvent
	if c.Authorization != nil && c.Authorization.ExecutionID != "" {
		e, err := r.Reader.GetExecutionForOrg(ctx, org, c.Authorization.ExecutionID)
		if err != nil {
			return CheckoutReplay{}, err
		}
		exec = &e
		if history, err = r.Reader.ExecutionHistoryForOrg(ctx, org, e.ID); err != nil {
			return CheckoutReplay{}, err
		}
	}
	return BuildCheckoutReplay(c, exec, history, r.Clock.Now()), nil
}
