// Package app holds use cases and the ports they depend on. It imports
// internal/domain but never an adapter: adapters implement these ports,
// not the other way around.
package app

import (
	"context"

	"enquoir/internal/domain"
)

// IntentRepository persists Intents, keyed by their IntentKey so a
// resubmitted intent is recognised rather than paid twice.
type IntentRepository interface {
	// Save stores intent if its key is new. If an intent with the same
	// key already exists, Save returns that stored intent unchanged.
	Save(ctx context.Context, intent domain.Intent) (domain.Intent, error)
}

// EventLog durably records an Intent's execution history as a
// hash-chained sequence of Events.
type EventLog interface {
	// Append records the next event for intentKey, linking it to the
	// chain's current head.
	Append(ctx context.Context, intentKey domain.IntentKey, eventType, payload string) (domain.Event, error)

	// Load returns the full recorded chain for intentKey, oldest first.
	Load(ctx context.Context, intentKey domain.IntentKey) (domain.Chain, error)
}
