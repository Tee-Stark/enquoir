package httpapi

import (
	"net/http"
	"time"
)

// MaxRequestBodyBytes caps every request body. The largest legitimate
// body, a beneficiary registration, is a few hundred bytes.
const MaxRequestBodyBytes = 64 << 10

// limitBody stops reading any body past MaxRequestBodyBytes, so no
// request - authenticated or not - can make the server hold an
// arbitrarily large body in memory.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// Server timeouts. WriteTimeout must outlast registerTimeout, the
// longest any request waits on purpose.
const (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 15 * time.Second
	writeTimeout      = registerTimeout + 15*time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 64 << 10
)

// NewServer is the API's HTTP server, with every timeout set: Go's
// zero-value server waits on a slow or stalled client forever, and a
// handful of them can exhaust connections and goroutines.
func NewServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
}
