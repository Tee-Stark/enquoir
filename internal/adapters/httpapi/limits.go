package httpapi

import "net/http"

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
