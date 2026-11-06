package airwallex

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"enquoir/internal/app"
)

// However many payments are in flight, no more than the limiter's
// size of requests are in the air at once.
func TestTransport_CapsConcurrentRequests(t *testing.T) {
	const limit, callers = 3, 12
	var inFlight, peak int32
	release := make(chan struct{})
	ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			p := atomic.LoadInt32(&peak)
			if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
				break
			}
		}
		<-release
		atomic.AddInt32(&inFlight, -1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"tr_1","status":"SENT"}`))
	})
	c := newTestClient(t, ts, &fakeClock{now: time.Now()})
	c.t.limiter = newLimiter(limit)
	c.t.ensureToken(t.Context()) // log in first, outside the measurement

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.GetTransfer(context.Background(), "tr_1"); err != nil {
				t.Errorf("GetTransfer: %v", err)
			}
		}()
	}
	time.Sleep(200 * time.Millisecond) // let every caller reach the limiter
	if got := atomic.LoadInt32(&inFlight); got != limit {
		t.Errorf("%d requests in flight while %d callers wait, want exactly %d", got, callers, limit)
	}
	close(release)
	wg.Wait()
	if p := atomic.LoadInt32(&peak); p > limit {
		t.Errorf("peak of %d concurrent requests, want at most %d", p, limit)
	}
}

// A caller that gives up waiting for a slot sends nothing.
func TestTransport_GivingUpWhileWaitingSendsNothing(t *testing.T) {
	var calls int32
	ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	})
	c := newTestClient(t, ts, &fakeClock{now: time.Now()})
	c.t.limiter = newLimiter(1)
	c.t.limiter.slots <- struct{}{} // the only slot is taken

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.GetTransfer(ctx, "tr_1")
	if !errors.Is(err, app.ErrAmbiguous) {
		t.Errorf("err = %v, want ErrAmbiguous", err)
	}
	if n := atomic.LoadInt32(&calls); n != 0 {
		t.Errorf("%d requests reached the rail, want 0", n)
	}
}

// Client and Simulator draw on one process-wide limit by default: it
// is one Airwallex account's limit, whichever type makes the call.
func TestTransport_ClientAndSimulatorShareTheDefaultLimit(t *testing.T) {
	c, err := NewClient(SandboxBaseURL, "id", "key", nil, &fakeClock{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSimulator(SandboxBaseURL, "id", "key", nil, &fakeClock{now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if c.t.limiter != s.t.limiter || cap(c.t.limiter.slots) != MaxConcurrentRequests {
		t.Errorf("client and simulator limits differ, or the default is not %d", MaxConcurrentRequests)
	}
}
