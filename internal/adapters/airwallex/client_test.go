package airwallex

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

type fakeClock struct{ now time.Time }

func (f *fakeClock) Now() time.Time { return f.now }

func testIntent(t *testing.T) domain.Intent {
	t.Helper()
	intent, err := domain.NewIntent("beneficiary-123", 10050, "USD", "INV-1001", "test payment")
	if err != nil {
		t.Fatalf("building test intent: %v", err)
	}
	return intent
}

func noSleep(time.Duration) {}

func TestNewClient_RefusesNonSandboxBaseURL(t *testing.T) {
	_, err := NewClient("https://api.airwallex.com", "id", "key", nil, &fakeClock{})
	if err == nil {
		t.Fatal("NewClient with a production base URL: want error, got nil")
	}
}

func TestNewSimulator_RefusesNonSandboxBaseURL(t *testing.T) {
	_, err := NewSimulator("https://api.airwallex.com", "id", "key", nil, &fakeClock{})
	if err == nil {
		t.Fatal("NewSimulator with a production base URL: want error, got nil")
	}
}

// server builds a fake Airwallex sandbox with a login endpoint and
// a caller-supplied handler for everything else, counting logins.
func server(t *testing.T, handle func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *int32) {
	t.Helper()
	var logins int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/authentication/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-client-id") == "" || r.Header.Get("x-api-key") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&logins, 1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"test-token"}`))
	})
	mux.HandleFunc("/", handle)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, &logins
}

func newTestClient(t *testing.T, ts *httptest.Server, clock app.Clock) *Client {
	t.Helper()
	c, err := newClientWithBaseURL(ts.URL, "client-id", "api-key", ts.Client(), clock)
	if err != nil {
		t.Fatalf("building test client: %v", err)
	}
	c.t.sleep = noSleep
	return c
}

func readBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading request body: %v", err)
	}
	return body
}

func extractJSONString(t *testing.T, body []byte, key string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("unmarshalling request body %s: %v", body, err)
	}
	v, _ := m[key].(string)
	return v
}

func TestClient_CachesAndRefreshesToken(t *testing.T) {
	clock := &fakeClock{now: time.Now()}
	ts, logins := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"tr_1","status":"SENT"}`))
	})
	c := newTestClient(t, ts, clock)

	if _, err := c.CreateTransfer(t.Context(), testIntent(t)); err != nil {
		t.Fatalf("first CreateTransfer: %v", err)
	}
	if _, err := c.CreateTransfer(t.Context(), testIntent(t)); err != nil {
		t.Fatalf("second CreateTransfer: %v", err)
	}
	if got := atomic.LoadInt32(logins); got != 1 {
		t.Errorf("expected 1 login for two calls within the token's lifetime, got %d", got)
	}

	clock.now = clock.now.Add(31 * time.Minute)
	if _, err := c.CreateTransfer(t.Context(), testIntent(t)); err != nil {
		t.Fatalf("third CreateTransfer: %v", err)
	}
	if got := atomic.LoadInt32(logins); got != 2 {
		t.Errorf("expected a second login after the token expired, got %d logins", got)
	}
}

func TestClient_CreateTransfer_ConvertsMinorToMajorUnits(t *testing.T) {
	var gotAmount string
	ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		gotAmount = extractJSONString(t, body, "transfer_amount")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"tr_1","status":"SENT"}`))
	})
	c := newTestClient(t, ts, &fakeClock{now: time.Now()})

	if _, err := c.CreateTransfer(t.Context(), testIntent(t)); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if gotAmount != "100.50" {
		t.Errorf("transfer_amount = %q, want %q", gotAmount, "100.50")
	}
}

func TestClient_CreateTransfer_PassesRequestIDThroughUnchanged(t *testing.T) {
	var gotRequestID string
	ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		body := readBody(t, r)
		gotRequestID = extractJSONString(t, body, "request_id")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"tr_1","status":"SENT"}`))
	})
	c := newTestClient(t, ts, &fakeClock{now: time.Now()})

	intent := testIntent(t)
	if _, err := c.CreateTransfer(t.Context(), intent); err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if gotRequestID != string(intent.Key) {
		t.Errorf("request_id = %q, want %q", gotRequestID, intent.Key)
	}
}

func TestClient_CreateTransfer_ClearRejectionMapsToErrRejected(t *testing.T) {
	ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		w.Write([]byte(`{"code":"beneficiary_invalid","message":"no such beneficiary"}`))
	})
	c := newTestClient(t, ts, &fakeClock{now: time.Now()})

	_, err := c.CreateTransfer(t.Context(), testIntent(t))
	if !errors.Is(err, app.ErrRejected) {
		t.Errorf("CreateTransfer error = %v, want it to wrap app.ErrRejected", err)
	}
}

func TestClient_CreateTransfer_ServerErrorMapsToErrAmbiguous(t *testing.T) {
	ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"message":"boom"}`))
	})
	c := newTestClient(t, ts, &fakeClock{now: time.Now()})

	_, err := c.CreateTransfer(t.Context(), testIntent(t))
	if !errors.Is(err, app.ErrAmbiguous) {
		t.Errorf("CreateTransfer error = %v, want it to wrap app.ErrAmbiguous", err)
	}
}

func TestClient_CreateTransfer_DroppedConnectionMapsToErrAmbiguous(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "authentication/login") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"token":"test-token"}`))
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("test server does not support hijacking")
		}
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	t.Cleanup(ts.Close)

	c := newTestClient(t, ts, &fakeClock{now: time.Now()})
	_, err := c.CreateTransfer(t.Context(), testIntent(t))
	if !errors.Is(err, app.ErrAmbiguous) {
		t.Errorf("CreateTransfer error = %v, want it to wrap app.ErrAmbiguous", err)
	}
}

func TestClient_CreateTransfer_DuplicateRequestIDResolvesViaLookup(t *testing.T) {
	intent := testIntent(t)
	ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/transfers/create"):
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"code":"duplicate_request_id","message":"already used"}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/transfers"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"items":[{"id":"tr_existing","request_id":"` + string(intent.Key) + `","status":"SENT"}]}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	})
	c := newTestClient(t, ts, &fakeClock{now: time.Now()})

	transfer, err := c.CreateTransfer(t.Context(), intent)
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if transfer.ID != "tr_existing" {
		t.Errorf("transfer.ID = %q, want %q", transfer.ID, "tr_existing")
	}
}

func TestClient_GetTransfer_MapsStatusAndFailureFields(t *testing.T) {
	ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"tr_1","status":"CANCELLED","failure_type":"bank_return","failure_reason":"account closed"}`))
	})
	c := newTestClient(t, ts, &fakeClock{now: time.Now()})

	transfer, err := c.GetTransfer(t.Context(), "tr_1")
	if err != nil {
		t.Fatalf("GetTransfer: %v", err)
	}
	if transfer.Status != app.TransferCancelled {
		t.Errorf("Status = %q, want %q", transfer.Status, app.TransferCancelled)
	}
	if transfer.FailureType != "bank_return" || transfer.FailureReason != "account closed" {
		t.Errorf("failure fields not carried through: %+v", transfer)
	}
}

func TestClient_GetTransfer_SentIsNotFinal(t *testing.T) {
	ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"tr_1","status":"SENT"}`))
	})
	c := newTestClient(t, ts, &fakeClock{now: time.Now()})

	transfer, err := c.GetTransfer(t.Context(), "tr_1")
	if err != nil {
		t.Fatalf("GetTransfer: %v", err)
	}
	if transfer.Status != app.TransferSent {
		t.Errorf("Status = %q, want %q", transfer.Status, app.TransferSent)
	}
}

func TestClient_BacksOffOnRateLimit(t *testing.T) {
	var attempts int32
	ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"message":"slow down"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"tr_1","status":"SENT"}`))
	})
	c := newTestClient(t, ts, &fakeClock{now: time.Now()})

	var slept []time.Duration
	c.t.sleep = func(d time.Duration) { slept = append(slept, d) }

	transfer, err := c.CreateTransfer(t.Context(), testIntent(t))
	if err != nil {
		t.Fatalf("CreateTransfer: %v", err)
	}
	if transfer.ID != "tr_1" {
		t.Errorf("transfer.ID = %q, want %q", transfer.ID, "tr_1")
	}
	if len(slept) != 1 {
		t.Errorf("expected exactly one backoff sleep, got %d (%v)", len(slept), slept)
	}
	if atomic.LoadInt32(&attempts) != 2 {
		t.Errorf("expected exactly 2 attempts, got %d", attempts)
	}
}

func TestClient_FindTransferByRequestID_NotFound(t *testing.T) {
	ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[]}`))
	})
	c := newTestClient(t, ts, &fakeClock{now: time.Now()})

	_, ok, err := c.FindTransferByRequestID(t.Context(), "nonexistent")
	if err != nil {
		t.Fatalf("FindTransferByRequestID: %v", err)
	}
	if ok {
		t.Error("expected ok=false for a request ID with no matching transfer")
	}
}

func TestClient_FindTransferByRequestID_MatchesExactlyAndRefusesDuplicates(t *testing.T) {
	const requestID = "abc123def456"
	tests := []struct {
		name    string
		body    string
		wantID  string
		wantOK  bool
		wantErr error
	}{
		{"exact match is found", `{"items":[{"id":"tr_1","request_id":"abc123def456","status":"SENT"}]}`, "tr_1", true, nil},
		// The docs only say the list endpoint filters on request_id, not
		// that it matches exactly, so another transfer must not count.
		{"other request id is not found", `{"items":[{"id":"tr_2","request_id":"zzz999","status":"PAID"}]}`, "", false, nil},
		{"two matches means the rail already holds a double payment", `{"items":[` +
			`{"id":"tr_1","request_id":"abc123def456","status":"PAID"},` +
			`{"id":"tr_2","request_id":"abc123def456","status":"PAID"}]}`, "", false, app.ErrDuplicateTransfers},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tt.body))
			})
			c := newTestClient(t, ts, &fakeClock{now: time.Now()})

			transfer, ok, err := c.FindTransferByRequestID(t.Context(), requestID)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("FindTransferByRequestID error = %v, want it to wrap %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("FindTransferByRequestID: %v", err)
			}
			if transfer.ID != tt.wantID || ok != tt.wantOK {
				t.Errorf("FindTransferByRequestID = (%q, %v), want (%q, %v)", transfer.ID, ok, tt.wantID, tt.wantOK)
			}
		})
	}
}
