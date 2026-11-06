// Package chaos is the one deliberate exception to Enquoir's clean
// architecture: a kill switch the worker checks directly, with no
// port and no interface, because its only job is to end the process
// outright at an exact point - there is nothing to abstract. Nothing
// else in this codebase may call os.Exit.
package chaos

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"os"
	"sync"
	"sync/atomic"
)

// EnvVar names the one kill point the worker will die at. Unset, or
// set to anything that never matches a Point this process checks,
// KillAt does nothing - that is the only behaviour this ever changes.
const EnvVar = "ENQUOIR_CHAOS_KILL_AT"

// Point is a named moment in the create-transfer sequence a chaos run
// can kill the worker at.
type Point string

const (
	BeforeLog             Point = "before_log"
	AfterLogBeforeCall    Point = "after_log_before_call"
	DuringCall            Point = "during_call"
	AfterCallBeforeCommit Point = "after_call_before_commit"
)

// killLogLine is logged right before every chaos exit, so a kill can be
// told apart from any other exit with the same code.
const killLogLine = "CHAOS: killing the process now"

// KillAt exits the process immediately if EnvVar names point.
func KillAt(point Point) {
	if os.Getenv(EnvVar) != string(point) {
		return
	}
	slog.Default().Error(killLogLine, "kill_at", point)
	os.Exit(1)
}

// ArmTransport returns a transport that, when EnvVar names point, exits
// the process the moment a request matching match has fully left it -
// every byte handed to the kernel - and before any response is read.
// That is a request genuinely in the air: the server gets all of it,
// the caller never learns the outcome. Unarmed, it returns base itself.
//
// "Fully left" is the hard part. Go's transport reports a request
// written (httptrace's WroteRequest) before flushing its write buffer
// over HTTP/1.1, but after flushing over HTTP/2. So the armed transport
// wraps every connection it dials: over HTTP/2 it exits at WroteRequest;
// over HTTP/1.1 it exits once the next write on that connection - the
// flush - has completed.
func ArmTransport(base *http.Transport, point Point, match func(*http.Request) bool) http.RoundTripper {
	if os.Getenv(EnvVar) != string(point) {
		return base
	}

	armed := base.Clone()
	dial := armed.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	armed.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return &watchedConn{Conn: conn, point: point}, nil
	}

	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if match(req) {
			req = req.WithContext(httptrace.WithClientTrace(req.Context(), killTrace(point)))
		}
		return armed.RoundTrip(req)
	})
}

// killTrace notes which connection and protocol a request went out on,
// then kills the process once that request has left it.
func killTrace(point Point) *httptrace.ClientTrace {
	var (
		mu    sync.Mutex
		conn  *watchedConn
		http2 bool
	)
	return &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			mu.Lock()
			defer mu.Unlock()
			c := info.Conn
			if tc, ok := c.(*tls.Conn); ok {
				http2 = tc.ConnectionState().NegotiatedProtocol == "h2"
				c = tc.NetConn()
			}
			conn, _ = c.(*watchedConn)
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if http2 || conn == nil {
				// HTTP/2 has already flushed. A connection this transport
				// did not dial cannot be watched; exiting now is the best
				// left, and the kill log still marks where it happened.
				KillAt(point)
				return
			}
			conn.killAfterNextWrite.Store(true)
		},
	}
}

// watchedConn exits the process after its next completed write, once
// armed: that write is HTTP/1.1 flushing the rest of the request.
type watchedConn struct {
	net.Conn
	point              Point
	killAfterNextWrite atomic.Bool
}

func (c *watchedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil && c.killAfterNextWrite.Load() {
		KillAt(c.point)
	}
	return n, err
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// WarnIfArmed logs a loud warning once at startup if EnvVar is set to
// anything at all, so an armed worker is never mistaken for a normal
// one from its logs alone.
func WarnIfArmed(logger *slog.Logger) {
	point := os.Getenv(EnvVar)
	if point == "" {
		return
	}
	logger.Warn("CHAOS MODE ARMED: this process will exit hard at a kill point", "kill_at", point)
}
