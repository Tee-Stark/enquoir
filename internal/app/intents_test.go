package app

import (
	"context"
	"errors"
	"sync"
	"testing"

	"enquoir/internal/domain"
)

type fakeEventLog struct {
	mu     sync.Mutex
	chains map[domain.IntentKey]domain.Chain
}

func newFakeEventLog() *fakeEventLog {
	return &fakeEventLog{chains: map[domain.IntentKey]domain.Chain{}}
}

func (l *fakeEventLog) Append(ctx context.Context, key domain.IntentKey, eventType, payload string) (domain.Event, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	chain := l.chains[key].Append(key, eventType, payload)
	l.chains[key] = chain
	return chain[len(chain)-1], nil
}

func (l *fakeEventLog) Load(ctx context.Context, key domain.IntentKey) (domain.Chain, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.chains[key], nil
}

type fakeStarter struct {
	startCalls int
	startErr   error
}

func (s *fakeStarter) Start(ctx context.Context, intent domain.Intent) error {
	s.startCalls++
	return s.startErr
}

func testIntent(t *testing.T) domain.Intent {
	t.Helper()
	intent, err := domain.NewIntent("payee-1", 10050, "USD", "ref-"+t.Name(), "test")
	if err != nil {
		t.Fatalf("building test intent: %v", err)
	}
	return intent
}

func TestSubmitIntent_FreshSubmissionReturnsProposed(t *testing.T) {
	intent := testIntent(t)
	starter := &fakeStarter{}
	use := &SubmitIntent{Starter: starter, Events: newFakeEventLog()}

	result, err := use.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if result.Key != intent.Key {
		t.Errorf("Key = %q, want %q", result.Key, intent.Key)
	}
	if result.State != domain.PROPOSED {
		t.Errorf("State = %q, want %q", result.State, domain.PROPOSED)
	}
	if starter.startCalls != 1 {
		t.Errorf("Start called %d times, want 1", starter.startCalls)
	}
}

func TestSubmitIntent_AttachingReturnsCurrentRecordedState(t *testing.T) {
	intent := testIntent(t)
	events := newFakeEventLog()
	events.Append(context.Background(), intent.Key, "PROPOSED", "")
	events.Append(context.Background(), intent.Key, "AUTHORIZED", "")
	events.Append(context.Background(), intent.Key, "APPLIED", "tr_1")

	use := &SubmitIntent{Starter: &fakeStarter{}, Events: events}

	result, err := use.Run(context.Background(), intent)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if result.State != domain.APPLIED {
		t.Errorf("State = %q, want %q (the already-recorded state, not a stale guess)", result.State, domain.APPLIED)
	}
}

func TestSubmitIntent_PropagatesStarterError(t *testing.T) {
	intent := testIntent(t)
	wantErr := errors.New("temporal unavailable")
	use := &SubmitIntent{Starter: &fakeStarter{startErr: wantErr}, Events: newFakeEventLog()}

	_, err := use.Run(context.Background(), intent)
	if !errors.Is(err, wantErr) {
		t.Errorf("Run error = %v, want it to wrap %v", err, wantErr)
	}
}

func TestGetIntent_ReturnsStateAndHistory(t *testing.T) {
	intent := testIntent(t)
	events := newFakeEventLog()
	events.Append(context.Background(), intent.Key, "PROPOSED", "")
	events.Append(context.Background(), intent.Key, "AUTHORIZED", "")

	use := &GetIntent{Events: events}
	result, err := use.Run(context.Background(), intent.Key)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}
	if result.State != domain.AUTHORIZED {
		t.Errorf("State = %q, want %q", result.State, domain.AUTHORIZED)
	}
	if len(result.Events) != 2 {
		t.Errorf("len(Events) = %d, want 2", len(result.Events))
	}
	if err := result.Events.Verify(); err != nil {
		t.Errorf("event chain failed to verify: %v", err)
	}
}

func TestGetIntent_UnknownKeyReturnsNotFound(t *testing.T) {
	use := &GetIntent{Events: newFakeEventLog()}
	_, err := use.Run(context.Background(), domain.IntentKey("never-submitted"))
	if !errors.Is(err, ErrIntentNotFound) {
		t.Errorf("Run error = %v, want ErrIntentNotFound", err)
	}
}
