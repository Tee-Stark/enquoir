package domain

import "fmt"

// State is a point in an Intent's lifecycle.
type State string

const (
	PROPOSED State = "PROPOSED"
	// AWAITING_APPROVAL is a payment the organisation's policy escalated:
	// above the approval threshold, waiting for an owner or admin to
	// approve or reject it, durably, for up to 24 hours (Phase 6d).
	AWAITING_APPROVAL State = "AWAITING_APPROVAL"
	AUTHORIZED        State = "AUTHORIZED"
	IN_FLIGHT         State = "IN_FLIGHT"
	UNKNOWN           State = "UNKNOWN"
	APPLIED           State = "APPLIED"
	FAILED            State = "FAILED"
	BLOCKED           State = "BLOCKED"
)

// legalTransitions is the full lifecycle graph. FAILED is deliberately not
// terminal: a reconciler may re-authorize a failed attempt or give up and
// block it. Only APPLIED and BLOCKED have no way out.
//
// AWAITING_APPROVAL is only reached from PROPOSED: the policy decision
// that escalates a payment is made before it is ever moved to
// AUTHORIZED, so there is no AUTHORIZED -> AWAITING_APPROVAL edge to
// exercise. Approval re-checks the limits and moves to AUTHORIZED;
// rejection or a 24-hour timeout moves to BLOCKED.
var legalTransitions = map[State]map[State]bool{
	PROPOSED:          {AUTHORIZED: true, BLOCKED: true, AWAITING_APPROVAL: true},
	AWAITING_APPROVAL: {AUTHORIZED: true, BLOCKED: true},
	AUTHORIZED:        {IN_FLIGHT: true, BLOCKED: true},
	IN_FLIGHT:         {APPLIED: true, FAILED: true, UNKNOWN: true},
	UNKNOWN:           {APPLIED: true, FAILED: true, IN_FLIGHT: true},
	FAILED:            {AUTHORIZED: true, BLOCKED: true},
	APPLIED:           {},
	BLOCKED:           {},
}

// Transition reports an error unless moving from this state to to is a
// legal step in the lifecycle.
func (s State) Transition(to State) error {
	if legalTransitions[s][to] {
		return nil
	}
	return fmt.Errorf("illegal transition from %s to %s", s, to)
}

// IsTerminal reports whether no further transition out of this state is
// ever legal.
func (s State) IsTerminal() bool {
	return len(legalTransitions[s]) == 0
}
