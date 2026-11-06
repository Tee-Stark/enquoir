package httpapi

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A body over the limit is refused with 413 before anything decodes
// it into memory, on unauthenticated routes as much as on the rest.
func TestBodiesOverTheLimitAreRefused(t *testing.T) {
	f := newAuthFixture(t, true)
	_, bearer := f.newAgent(t)
	huge := `{"email":"a@example.com","password":"` + strings.Repeat("x", MaxRequestBodyBytes) + `","organisation_name":"A"}`

	for _, path := range []string{"/v1/auth/signup", "/v1/auth/login", "/v1/intents", "/v1/beneficiaries"} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(huge))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		if p := decodeProblem(t, rec); rec.Code != http.StatusRequestEntityTooLarge || p.Code != "body_too_large" {
			t.Errorf("POST %s with a %d-byte body: %d %q, want 413 body_too_large", path, len(huge), rec.Code, p.Code)
		}
	}
}

func TestBodiesUnderTheLimitStillWork(t *testing.T) {
	f := newAuthFixture(t, true)
	if rec := f.do(t, http.MethodPost, "/v1/auth/signup", signUpBody); rec.Code != http.StatusCreated {
		t.Errorf("a normal sign-up: %d %s", rec.Code, rec.Body)
	}
}

// The server never waits forever on a client: slow headers, slow
// bodies and idle connections all time out. The write timeout outlasts
// the longest request the API serves on purpose - a beneficiary
// registration waiting on its workflow - or that request would be cut off.
func TestNewServer_TimesOutSlowClients(t *testing.T) {
	srv := NewServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 || srv.WriteTimeout <= 0 || srv.IdleTimeout <= 0 {
		t.Errorf("timeouts = header %v, read %v, write %v, idle %v; want all set", srv.ReadHeaderTimeout, srv.ReadTimeout, srv.WriteTimeout, srv.IdleTimeout)
	}
	if srv.WriteTimeout <= registerTimeout {
		t.Errorf("write timeout %v does not outlast the %v a registration may wait", srv.WriteTimeout, registerTimeout)
	}
	if srv.MaxHeaderBytes <= 0 || srv.MaxHeaderBytes > 1<<20 {
		t.Errorf("MaxHeaderBytes = %d, want a set limit", srv.MaxHeaderBytes)
	}
}

// A client that opens a connection and dribbles its headers is cut off.
func TestNewServer_DropsAClientThatNeverFinishesItsHeaders(t *testing.T) {
	srv := NewServer("127.0.0.1:0", http.NotFoundHandler())
	srv.ReadHeaderTimeout = 100 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n")) // never sends the final blank line
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(conn); err != nil {
		t.Errorf("the server did not close the connection: %v", err)
	}
}
