package workflow

import (
	"context"
	"sync"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

type fakeIntentRepository struct {
	mu     sync.Mutex
	stored map[domain.IntentKey]domain.Intent
}

func newFakeIntentRepository() *fakeIntentRepository {
	return &fakeIntentRepository{stored: map[domain.IntentKey]domain.Intent{}}
}

func (r *fakeIntentRepository) Save(ctx context.Context, intent domain.Intent) (domain.Intent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.stored[intent.Key]; ok {
		return existing, nil
	}
	r.stored[intent.Key] = intent
	return intent, nil
}

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

// fakeRail lets each test script exactly what the rail does, since the
// right next move (retry, look up, give up) depends on call order.
type fakeRail struct {
	mu          sync.Mutex
	createCalls int

	createFunc func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error)
	getFunc    func(ctx context.Context, transferID string) (app.Transfer, error)
	findFunc   func(ctx context.Context, requestID string) (app.Transfer, bool, error)
}

func (r *fakeRail) CreateTransfer(ctx context.Context, intent domain.Intent) (app.Transfer, error) {
	r.mu.Lock()
	r.createCalls++
	callNum := r.createCalls
	r.mu.Unlock()
	return r.createFunc(ctx, intent, callNum)
}

func (r *fakeRail) GetTransfer(ctx context.Context, transferID string) (app.Transfer, error) {
	return r.getFunc(ctx, transferID)
}

func (r *fakeRail) FindTransferByRequestID(ctx context.Context, requestID string) (app.Transfer, bool, error) {
	return r.findFunc(ctx, requestID)
}

func (r *fakeRail) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.createCalls
}
