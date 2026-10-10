package app

import (
	"context"
	"errors"

	"enquoir/internal/domain"
)

// ErrIntentNotFound means no event has ever been recorded for a key -
// as far as this system has observed, that intent does not exist.
var ErrIntentNotFound = errors.New("intent not found")

// PaymentStarter starts, or attaches to, the durable workflow that
// carries one Intent to a terminal state. Submitting the same intent
// twice must attach to the run already in progress, never start a
// second one.
type PaymentStarter interface {
	Start(ctx context.Context, intent domain.Intent) error
}

// SubmitIntentResult is what submitting an intent resolves to: its
// key, and its state as currently recorded - PROPOSED for a brand new
// submission, or whatever a run already in progress has reached for
// one that attaches instead.
type SubmitIntentResult struct {
	Key   domain.IntentKey
	State domain.State
}

// SubmitIntent is the submit-intent use case.
type SubmitIntent struct {
	Starter PaymentStarter
	Events  EventLog
}

func (s *SubmitIntent) Run(ctx context.Context, intent domain.Intent) (SubmitIntentResult, error) {
	if err := s.Starter.Start(ctx, intent); err != nil {
		return SubmitIntentResult{}, err
	}

	state := domain.PROPOSED
	if chain, err := s.Events.Load(ctx, intent.Key); err == nil && len(chain) > 0 {
		state = domain.State(chain[len(chain)-1].Type)
	}

	return SubmitIntentResult{Key: intent.Key, State: state}, nil
}

// GetIntentResult is one intent's recorded state and full event
// history, oldest first.
type GetIntentResult struct {
	Key    domain.IntentKey
	State  domain.State
	Events domain.Chain
}

// GetIntent is the get-intent-by-key use case.
type GetIntent struct {
	Events EventLog
}

func (g *GetIntent) Run(ctx context.Context, key domain.IntentKey) (GetIntentResult, error) {
	chain, err := g.Events.Load(ctx, key)
	if err != nil {
		return GetIntentResult{}, err
	}
	if len(chain) == 0 {
		return GetIntentResult{}, ErrIntentNotFound
	}

	return GetIntentResult{
		Key:    key,
		State:  domain.State(chain[len(chain)-1].Type),
		Events: chain,
	}, nil
}
