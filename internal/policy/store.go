package policy

import (
	"context"

	"enquoir/internal/domain"
)

// Store persists policy versions and evaluates a candidate payment
// against an organisation's current one. Both bounded contexts
// (treasury and commerce) share one Store, so a rolling total is one
// organisation-wide figure across both kinds of spending, never two
// separate totals.
//
// EvaluateAndReserve locks the organisation for the duration of the
// evaluation, so two concurrent evaluations never read usage against
// a torn or stale view of each other. It does not, by itself, make
// the candidate payment's amount count towards future usage - the
// caller does that by persisting the payment (as an Intent or a
// Checkout) once Allow or Escalate comes back; a payment that is
// never persisted never counts. A caller that is slow to persist
// after a successful evaluation leaves a brief window in which a
// different, concurrent evaluation would not yet see it - a known,
// accepted limitation (see docs/progress.md), not a silent gap.
type Store interface {
	// CurrentPolicy is an organisation's latest version, or
	// DefaultPolicyVersion if it has never edited one.
	CurrentPolicy(ctx context.Context, org domain.OrgID) (PolicyVersion, error)
	// SaveNewVersion stores v as the next version after the
	// organisation's current one (its own Version field is ignored and
	// overwritten), under the same organisation lock EvaluateAndReserve
	// uses, so an edit can't race a concurrent one onto the same number.
	SaveNewVersion(ctx context.Context, v PolicyVersion) (PolicyVersion, error)
	// EvaluateAndReserve decides in against the organisation's current
	// policy and its rolling usage as of now.
	EvaluateAndReserve(ctx context.Context, org domain.OrgID, in EvaluationInput) (Decision, error)
}
