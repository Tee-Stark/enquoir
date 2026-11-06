package mailfile

import (
	"bufio"
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
