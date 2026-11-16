package app

import (
	"context"
	"errors"
	"fmt"

	"enquoir/internal/domain"
)

// ErrNotAwaitingApproval means the payment named is not currently
// waiting for an escalation decision - already decided, or never
// escalated at all.
var ErrNotAwaitingApproval = errors.New("payment is not awaiting approval")

// ErrApproverExcluded means the acting user created the paying
// agent, and another eligible owner or admin exists to decide
// instead - waived only when nobody else is eligible (a confirmed
// product decision, not an oversight).
var ErrApproverExcluded = errors.New("the agent's creator cannot approve its own payment while another owner or admin can")

// EscalationSignaler sends an approve/reject decision to the payment
// workflow waiting for it, durably, in AWAITING_APPROVAL.
type EscalationSignaler interface {
	SignalApproval(ctx context.Context, key domain.IntentKey, approved bool, decidedBy string) error
}

func anotherEligibleApproverExists(members []Member, exclude domain.UserID) bool {
	for _, m := range members {
		if m.User.ID != exclude && (m.Role == domain.Owner || m.Role == domain.Admin) {
			return true
		}
	}
	return false
}

// DecideEscalatedPayment approves or rejects a payment waiting in
// AWAITING_APPROVAL - one decision resolves it; the workflow itself
// re-checks the policy on approval and denies on timeout.
type DecideEscalatedPayment struct {
	Payments PaymentReader
	Agents   AgentStore
	Members  MemberStore
	Signaler EscalationSignaler
	Audit    *Audit
}

func (d *DecideEscalatedPayment) Run(ctx context.Context, org domain.OrgID, key domain.IntentKey, approve bool) error {
	payment, err := d.Payments.GetPayment(ctx, org, key)
	if err != nil {
		return err
	}
	if payment.State != string(domain.AWAITING_APPROVAL) {
		return ErrNotAwaitingApproval
	}

	actor := ActorFrom(ctx)
	// Rejecting your own agent's payment is not a conflict of interest
	// the way approving it is - only approval is excluded.
	if approve && payment.AgentID != "" {
		agent, err := d.Agents.GetAgent(ctx, org, payment.AgentID)
		if err != nil {
			return err
		}
		if agent.CreatedBy != "" && agent.CreatedBy == actor.UserID {
			members, err := d.Members.ListMembers(ctx, org)
			if err != nil {
				return err
			}
			if anotherEligibleApproverExists(members, actor.UserID) {
				return ErrApproverExcluded
			}
		}
	}

	if err := d.Signaler.SignalApproval(ctx, key, approve, string(actor.UserID)); err != nil {
		return fmt.Errorf("signalling the decision: %w", err)
	}

	action := AuditPaymentApproved
	if !approve {
		action = AuditPaymentRejected
	}
	d.Audit.record(ctx, org, action, "intent", string(key), nil)
	return nil
}
