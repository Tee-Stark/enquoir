package postgres

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// A cursor is the (time, id) of the last row of a page, opaque to
// callers: lists are ordered by time then id, both descending.
func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id))
}

// ErrBadCursor means a cursor was not one this package produced.
var ErrBadCursor = errors.New("invalid cursor")

func decodeCursor(c string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return time.Time{}, "", ErrBadCursor
	}
	ts, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return time.Time{}, "", ErrBadCursor
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, "", ErrBadCursor
	}
	return t, id, nil
}

// where accumulates SQL conditions with numbered parameters.
type where struct {
	conds []string
	args  []any
}

func (w *where) add(cond string, args ...any) {
	for _, a := range args {
		w.args = append(w.args, a)
		cond = strings.Replace(cond, "?", "$"+itoa(len(w.args)), 1)
	}
	w.conds = append(w.conds, cond)
}

func (w *where) sql() string {
	if len(w.conds) == 0 {
		return "TRUE"
	}
	return strings.Join(w.conds, " AND ")
}

func (w *where) next(arg any) string {
	w.args = append(w.args, arg)
	return "$" + itoa(len(w.args))
}

func itoa(n int) string {
	const digits = "0123456789"
	if n < 10 {
		return digits[n : n+1]
	}
	return itoa(n/10) + digits[n%10:n%10+1]
}
