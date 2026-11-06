// Package mailfile implements app.Mailer by appending each email to a
// file, one JSON object per line - for local development and the
// end-to-end test, which reads links back out of it. It mimics
// Resend's idempotency: a key already used is ErrAlreadySent.
package mailfile

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"enquoir/internal/app"
)

var _ app.Mailer = (*Mailer)(nil)

// Message is one line of the file.
type Message struct {
	ID             string    `json:"id"`
	To             string    `json:"to"`
	Subject        string    `json:"subject"`
	Text           string    `json:"text"`
	HTML           string    `json:"html"`
	IdempotencyKey string    `json:"idempotency_key"`
	SentAt         time.Time `json:"sent_at"`
}

// Mailer appends to path. Safe for concurrent use in one process.
type Mailer struct {
	path string
	mu   sync.Mutex
}

func New(path string) *Mailer { return &Mailer{path: path} }

func (m *Mailer) Send(ctx context.Context, e app.OutgoingEmail) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.IdempotencyKey != "" && m.used(e.IdempotencyKey) {
		return "", fmt.Errorf("mailfile: key %s already used: %w", e.IdempotencyKey, app.ErrAlreadySent)
	}
	b := make([]byte, 8)
	rand.Read(b)
	msg := Message{ID: "file_" + hex.EncodeToString(b), To: string(e.To), Subject: e.Subject, Text: e.Text, HTML: e.HTML,
		IdempotencyKey: e.IdempotencyKey, SentAt: time.Now().UTC()}
	line, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(m.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("mailfile: %v: %w", err, app.ErrAmbiguous)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return "", fmt.Errorf("mailfile: %v: %w", err, app.ErrAmbiguous)
	}
	return msg.ID, nil
}

func (m *Mailer) used(key string) bool {
	f, err := os.Open(m.path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var msg Message
		if json.Unmarshal(sc.Bytes(), &msg) == nil && msg.IdempotencyKey == key {
			return true
		}
	}
	return false
}
