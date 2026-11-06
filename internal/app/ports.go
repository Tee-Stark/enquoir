// Package app holds use cases and the ports they depend on. It imports
// internal/domain but never an adapter: adapters implement these ports,
// not the other way around.
package app

import (
	"context"

	"enquoir/internal/domain"
)

// IntentRepository persists Intents, keyed by their IntentKey so a
// resubmitted intent is recognised rather than paid twice. Tenants are
// isolated: every method is scoped by organisation, even though the
// key itself already embeds one - explicit scoping is a second,
// independent guard against a cross-tenant read or write, not just the
// key's own uncollidability.
type IntentRepository interface {
	// Save stores intent if its key is new. If an intent with the same
	// key already exists, Save returns that stored intent unchanged.
	Save(ctx context.Context, intent domain.Intent) (domain.Intent, error)
}

// EventLog durably records an Intent's execution history as a
// hash-chained sequence of Events, scoped by organisation.
type EventLog interface {
	// Append records the next event for intentKey, linking it to the
	// chain's current head.
	Append(ctx context.Context, org domain.OrgID, intentKey domain.IntentKey, eventType, payload string) (domain.Event, error)

	// Load returns the full recorded chain for intentKey, oldest first.
	// A key belonging to a different organisation returns an empty
	// chain, the same as a key that was never used - "not found", never
	// "forbidden".
	Load(ctx context.Context, org domain.OrgID, intentKey domain.IntentKey) (domain.Chain, error)
}
