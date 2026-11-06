package mailfile

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"enquoir/internal/app"
)

func TestSend_AppendsOneJSONLinePerEmailAndHonoursIdempotency(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.jsonl")
	m := New(path)
	e := app.OutgoingEmail{To: "ada@example.com", Subject: "s", HTML: "<p>h</p>", Text: "t https://x/verify-email?token=abc", IdempotencyKey: "email/1"}

	id, err := m.Send(t.Context(), e)
	if err != nil || id == "" {
		t.Fatalf("Send = %q, %v", id, err)
	}
	e.Text = "a different link"
	if _, err := m.Send(t.Context(), e); !errors.Is(err, app.ErrAlreadySent) {
		t.Errorf("same key again: err = %v, want ErrAlreadySent, like Resend", err)
	}
	e.IdempotencyKey = "email/2"
	m.Send(t.Context(), e)

	f, _ := os.Open(path)
	defer f.Close()
	var lines []Message
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var msg Message
		if err := json.Unmarshal(sc.Bytes(), &msg); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, msg)
	}
	if len(lines) != 2 || lines[0].To != "ada@example.com" || lines[0].Text != "t https://x/verify-email?token=abc" || lines[0].ID != id {
		t.Errorf("file = %+v", lines)
	}
}

func TestCopy_KeepsACopyOfWhatThePrimaryAccepted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "copy.jsonl")
	refusing := New(filepath.Join(t.TempDir(), "x.jsonl"))
	m := Copy(refusing, path)
	e := app.OutgoingEmail{To: "a@b.co", Text: "one", IdempotencyKey: "k1"}
	m.Send(t.Context(), e)
	m.Send(t.Context(), e) // refused by the primary as already sent: no second copy
	b, _ := os.ReadFile(path)
	if n := len(splitLines(b)); n != 1 {
		t.Errorf("%d copies, want 1", n)
	}
}

func splitLines(b []byte) []string {
	var out []string
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(l) > 0 {
			out = append(out, string(l))
		}
	}
	return out
}
