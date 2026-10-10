package chaos

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
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
