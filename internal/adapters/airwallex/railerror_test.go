package airwallex

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"enquoir/internal/app"
)

// A failed call the rail answered carries its HTTP status and the
// rail's error code, for the attempt log, while still being the
// rejection or ambiguity the workflow branches on, with the same text.
func TestRailErrors_CarryStatusAndCode(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		kind     error
		wantCode string
	}{
		{"clear rejection", http.StatusBadRequest, `{"code":"fee_unknown","message":"fee problem"}`, app.ErrRejected, "fee_unknown"},
		{"server error", http.StatusInternalServerError, `{"code":"operation_failed","message":"boom"}`, app.ErrAmbiguous, "operation_failed"},
		{"no code in the body", http.StatusBadGateway, `<html>bad gateway</html>`, app.ErrAmbiguous, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			})
			c := newTestClient(t, ts, &fakeClock{now: time.Now()})

			_, err := c.CreateTransfer(t.Context(), testIntent(t))
			var re *app.RailError
			if !errors.As(err, &re) {
				t.Fatalf("err = %v (%T), want an *app.RailError", err, err)
			}
			if re.HTTPStatus != tc.status || re.Code != tc.wantCode {
				t.Errorf("status/code = %d/%q, want %d/%q", re.HTTPStatus, re.Code, tc.status, tc.wantCode)
			}
			if !errors.Is(err, tc.kind) {
				t.Errorf("err does not match %v", tc.kind)
			}
			if !strings.Contains(err.Error(), tc.body) {
				t.Errorf("message %q lost the rail's body", err.Error())
			}
		})
	}
}

func TestRailErrors_GetAndFindToo(t *testing.T) {
	ts, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"code":"unavailable"}`))
	})
	c := newTestClient(t, ts, &fakeClock{now: time.Now()})
	var re *app.RailError
	if _, err := c.GetTransfer(t.Context(), "tr_1"); !errors.As(err, &re) || re.HTTPStatus != 503 {
		t.Errorf("GetTransfer: %v", err)
	}
	if _, _, err := c.FindTransferByRequestID(t.Context(), "req_1234567"); !errors.As(err, &re) || re.Code != "unavailable" {
		t.Errorf("FindTransferByRequestID: %v", err)
	}
}
