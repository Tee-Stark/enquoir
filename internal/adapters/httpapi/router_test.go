package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"enquoir/internal/app"
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
	mu    sync.Mutex
	calls []domain.IntentKey
}

func (s *fakeStarter) Start(ctx context.Context, intent domain.Intent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, intent.Key)
	return nil
}

func (s *fakeStarter) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func newTestRouter() (http.Handler, *fakeEventLog, *fakeStarter) {
	events := newFakeEventLog()
	starter := &fakeStarter{}
	h := &Handler{
		Submit: &app.SubmitIntent{Starter: starter, Events: events},
		Get:    &app.GetIntent{Events: events},
	}
	return NewRouter(h), events, starter
}

func postIntent(t *testing.T, router http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/intents", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

const validIntentJSON = `{"payee":"payee-1","amount":"10.50","currency":"USD","reference":"INV-1001","reason":"invoice settlement"}`

func TestSubmitIntent_ValidRequestReturnsKeyAndState(t *testing.T) {
	router, _, starter := newTestRouter()

	rec := postIntent(t, router, validIntentJSON)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp submitIntentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Key == "" {
		t.Error("expected a non-empty key")
	}
	if resp.State != string(domain.PROPOSED) {
		t.Errorf("state = %q, want %q", resp.State, domain.PROPOSED)
	}
	if starter.callCount() != 1 {
		t.Errorf("Starter called %d times, want 1", starter.callCount())
	}
}

func TestSubmitIntent_SameIntentTwiceReturnsSameKeyAndStartsNothingNew(t *testing.T) {
	router, _, starter := newTestRouter()

	first := postIntent(t, router, validIntentJSON)
	second := postIntent(t, router, validIntentJSON)

	var firstResp, secondResp submitIntentResponse
	json.Unmarshal(first.Body.Bytes(), &firstResp)
	json.Unmarshal(second.Body.Bytes(), &secondResp)

	if firstResp.Key != secondResp.Key {
		t.Errorf("same intent produced different keys: %q vs %q", firstResp.Key, secondResp.Key)
	}
	if starter.callCount() != 2 {
		t.Errorf("Starter called %d times, want 2 (attach semantics are the starter's job, not this handler's)", starter.callCount())
	}
}

func TestSubmitIntent_RejectsInvalidInput(t *testing.T) {
	cases := map[string]string{
		"missing payee":        `{"amount":"10.50","currency":"USD","reference":"INV-1","reason":"x"}`,
		"zero amount":          `{"payee":"p","amount":"0.00","currency":"USD","reference":"INV-1","reason":"x"}`,
		"unsupported currency": `{"payee":"p","amount":"10.50","currency":"JPY","reference":"INV-1","reason":"x"}`,
		"malformed JSON":       `{not json`,
		"blank reference":      `{"payee":"p","amount":"10.50","currency":"USD","reference":"  ","reason":"x"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			router, _, _ := newTestRouter()
			rec := postIntent(t, router, body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

func TestGetIntent_ReturnsStateAndHistory(t *testing.T) {
	router, events, _ := newTestRouter()

	intent, err := domain.NewIntent("payee-1", 1050, "USD", "INV-2", "test")
	if err != nil {
		t.Fatalf("building intent: %v", err)
	}
	ctx := context.Background()
	events.Append(ctx, intent.Key, "PROPOSED", "")
	events.Append(ctx, intent.Key, "AUTHORIZED", "")

	req := httptest.NewRequest(http.MethodGet, "/v1/intents/"+string(intent.Key), nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	var resp getIntentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.State != string(domain.AUTHORIZED) {
		t.Errorf("state = %q, want %q", resp.State, domain.AUTHORIZED)
	}
	if len(resp.Events) != 2 {
		t.Errorf("len(events) = %d, want 2", len(resp.Events))
	}
	if resp.Events[0].Type != "PROPOSED" || resp.Events[1].Type != "AUTHORIZED" {
		t.Errorf("events out of order or wrong: %+v", resp.Events)
	}
}

func TestGetIntent_UnknownKeyReturnsNotFound(t *testing.T) {
	router, _, _ := newTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/v1/intents/does-not-exist", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
}

func TestHealthz_ReturnsOK(t *testing.T) {
	router, _, _ := newTestRouter()

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
}
