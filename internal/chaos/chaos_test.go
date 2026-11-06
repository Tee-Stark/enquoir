package chaos

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestKillAt_NoopWhenEnvVarUnset(t *testing.T) {
	t.Setenv(EnvVar, "")
	KillAt(BeforeLog)
	// Reaching this line is the assertion: KillAt must not have exited.
}

func TestKillAt_NoopWhenEnvVarNamesADifferentPoint(t *testing.T) {
	t.Setenv(EnvVar, string(AfterCallBeforeCommit))
	KillAt(BeforeLog)
	// Reaching this line is the assertion.
}

// TestKillAt_ExitsWhenPointMatches re-execs this same test binary as a
// subprocess, since KillAt's whole job is to call os.Exit - the one
// behaviour a normal in-process test can never observe directly.
func TestKillAt_ExitsWhenPointMatches(t *testing.T) {
	if os.Getenv("CHAOS_TEST_SUBPROCESS") == "1" {
		t.Setenv(EnvVar, string(DuringCall))
		KillAt(DuringCall)
		t.Fatal("KillAt returned instead of exiting the process")
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestKillAt_ExitsWhenPointMatches")
	cmd.Env = append(os.Environ(), "CHAOS_TEST_SUBPROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not exit with a *exec.ExitError: %v (stderr: %s)", err, stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("subprocess exit code = %d, want 1 (stderr: %s)", exitErr.ExitCode(), stderr.String())
	}
}

func isCreate(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/transfers/create")
}

func TestArmTransport_UnarmedReturnsTransportUnchanged(t *testing.T) {
	t.Setenv(EnvVar, "")
	base := &http.Transport{}
	if got := ArmTransport(base, DuringCall, isCreate); got != http.RoundTripper(base) {
		t.Errorf("unarmed: want the transport back unchanged, got %T", got)
	}
}

func TestArmTransport_ArmedLetsNonMatchingRequestsThrough(t *testing.T) {
	t.Setenv(EnvVar, string(DuringCall))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	client := &http.Client{Transport: ArmTransport(&http.Transport{}, DuringCall, isCreate)}
	resp, err := client.Post(srv.URL+"/api/v1/authentication/login", "application/json", nil)
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	resp.Body.Close()
	// Reaching this line is the assertion: a login must never trip the kill.
}

// The point of during_call: the worker dies with the create request
// fully sent - the server has every byte of it - and no response read.
// Go's transport reports a request written at different moments per
// protocol (HTTP/1.1 before flushing its buffer, HTTP/2 after), so each
// is tested, and the server confirms which protocol it actually saw.
// Re-execs this test binary, since the behaviour under test is os.Exit.
func TestArmTransport_ExitsOnceTheMatchingRequestIsSent(t *testing.T) {
	const body = `{"request_id":"abc1234567"}`

	if url := os.Getenv("CHAOS_TEST_SERVER_URL"); url != "" {
		runArmedClient(t, url, os.Getenv("CHAOS_TEST_SERVER_CERT"), body)
		return
	}

	cases := []struct {
		name      string
		tls       bool
		http2     bool
		wantProto int
	}{
		{"HTTP/1.1 over plain TCP", false, false, 1},
		{"HTTP/1.1 over TLS", true, false, 1},
		{"HTTP/2 over TLS", true, true, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			type receipt struct {
				body  string
				proto int
			}
			received := make(chan receipt, 1)
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !isCreate(r) {
					w.WriteHeader(http.StatusCreated) // the warm-up login
					return
				}
				b, _ := io.ReadAll(r.Body)
				received <- receipt{string(b), r.ProtoMajor}
				// Never answer: the only way this request ends is the client dying.
				<-r.Context().Done()
			}))
			srv.EnableHTTP2 = tc.http2
			var certPEM string
			if tc.tls {
				srv.StartTLS()
				certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
			} else {
				srv.Start()
			}
			defer srv.Close()

			cmd := exec.Command(os.Args[0], "-test.run=TestArmTransport_ExitsOnceTheMatchingRequestIsSent")
			cmd.Env = append(os.Environ(), "CHAOS_TEST_SERVER_URL="+srv.URL, "CHAOS_TEST_SERVER_CERT="+certPEM)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr

			err := cmd.Run()
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("subprocess: want exit code 1, got %v (stderr: %s)", err, stderr.String())
			}
			// A failed test also exits 1; only the kill switch logs this.
			if !strings.Contains(stderr.String(), killLogLine) {
				t.Fatalf("subprocess exited 1 without the kill switch firing (stderr: %s)", stderr.String())
			}
			select {
			case got := <-received:
				if got.proto != tc.wantProto {
					t.Fatalf("server saw HTTP/%d, want HTTP/%d: the case did not test what it names", got.proto, tc.wantProto)
				}
				if got.body != body {
					t.Errorf("server received %q, want the full body %q", got.body, body)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the process exited but the server never received the request")
			}
		})
	}
}

// runArmedClient is the subprocess half: it sends the create the way the
// worker does - a transport that offers HTTP/2, over a connection the
// login already opened - armed to die once sent. The warm connection
// matters: on it, nothing else may ever be written after the request,
// so the kill cannot wait for some later write to come along.
func runArmedClient(t *testing.T, url, certPEM, body string) {
	t.Setenv(EnvVar, string(DuringCall))
	base := &http.Transport{ForceAttemptHTTP2: true}
	if certPEM != "" {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM([]byte(certPEM))
		base.TLSClientConfig = &tls.Config{RootCAs: pool}
	}
	client := &http.Client{Transport: ArmTransport(base, DuringCall, isCreate), Timeout: 3 * time.Second}

	login, err := client.Post(url+"/api/v1/authentication/login", "application/json", nil)
	if err != nil {
		t.Fatalf("warm-up login: %v", err)
	}
	io.Copy(io.Discard, login.Body)
	login.Body.Close()
	time.Sleep(100 * time.Millisecond) // let connection setup traffic settle

	resp, err := client.Post(url+"/api/v1/transfers/create", "application/json", strings.NewReader(body))
	if err == nil {
		resp.Body.Close()
	}
	t.Fatalf("the request completed (err %v) instead of the process exiting", err)
}

func TestWarnIfArmed_LogsWhenSet(t *testing.T) {
	t.Setenv(EnvVar, string(DuringCall))
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	WarnIfArmed(logger)

	if !strings.Contains(buf.String(), "CHAOS") {
		t.Errorf("expected a chaos warning to be logged, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), string(DuringCall)) {
		t.Errorf("expected the warning to name the kill point, got: %s", buf.String())
	}
}

func TestWarnIfArmed_SilentWhenUnset(t *testing.T) {
	t.Setenv(EnvVar, "")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	WarnIfArmed(logger)

	if buf.Len() != 0 {
		t.Errorf("expected no log output when unset, got: %s", buf.String())
	}
}
