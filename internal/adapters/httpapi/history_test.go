package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

// newOrg signs up a fresh organisation, returning its ID and session.
func (f *authFixture) newOrg(t *testing.T) (domain.OrgID, *http.Cookie) {
	t.Helper()
	email := fmt.Sprintf("owner-%d@example.com", testEmailCounter.Add(1))
	rec := f.do(t, http.MethodPost, "/v1/auth/signup", fmt.Sprintf(`{"email":%q,"password":"correct horse battery","organisation_name":"Org"}`, email))
	var body authBody
	json.Unmarshal(rec.Body.Bytes(), &body)
	return domain.OrgID(body.Organisation.ID), sessionCookie(t, rec)
}

func (f *authFixture) seedPayment(t *testing.T, org domain.OrgID, ref string, history ...string) app.Payment {
	t.Helper()
	intent, _ := domain.NewIntent(org, "ben_1", 1050, "USD", ref, "invoice")
	state := app.StateReceived
	for _, h := range history {
		f.events.Append(context.Background(), org, intent.Key, h, "")
		state = h
	}
	now := f.clock.Now()
	p := app.Payment{Key: intent.Key, OrgID: org, State: state, Payee: "ben_1", PayeeName: "Acme Ltd", Amount: intent.Amount,
		Reference: ref, AgentID: "agt_1", AgentName: "Invoice bot", CreatedAt: now, Submissions: 2, RailCalls: 3}
	if state == "APPLIED" {
		p.TransferID = "tr_1"
		p.StateChangedAt = &now
	}
	f.payments.Add(p)
	return p
}

func TestPayments_ListAndDetail(t *testing.T) {
	f := newAuthFixture(t, true)
	org, cookie := f.newOrg(t)
	p := f.seedPayment(t, org, "INV-1", "PROPOSED", "AUTHORIZED", "IN_FLIGHT", "APPLIED")
	f.seedPayment(t, org, "INV-2")
	ctx := context.Background()
	f.attempts.Record(ctx, app.Attempt{ID: "att_1", OrgID: org, Kind: app.KindAgentSubmission, Operation: app.OpSubmitIntent,
		Outcome: app.OutcomeStarted, IntentKey: p.Key, AgentID: "agt_1", Details: map[string]string{"amount": "10.50"}, StartedAt: f.clock.Now()})
	done := f.clock.Now().Add(1500 * time.Millisecond)
	f.attempts.Record(ctx, app.Attempt{ID: "att_2", OrgID: org, Kind: app.KindRailCall, Operation: app.OpCreateTransfer,
		Outcome: app.OutcomeRejected, IntentKey: p.Key, HTTPStatus: 400, ErrorCode: "fee_unknown", ActivityAttempt: 1,
		StartedAt: f.clock.Now(), FinishedAt: &done})

	rec := f.do(t, http.MethodGet, "/v1/payments", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	var list struct {
		Data []struct {
			Key, State string
			Terminal   bool
			Amount     struct{ Amount string }
			Payee      struct{ Name *string }
		}
		NextCursor *string `json:"next_cursor"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Data) != 2 || list.Data[0].Amount.Amount != "10.50" || list.Data[0].Payee.Name == nil {
		t.Errorf("list = %s", rec.Body)
	}

	rec = f.do(t, http.MethodGet, "/v1/payments/"+string(p.Key), "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail: %d %s", rec.Code, rec.Body)
	}
	var d struct {
		State    string
		Terminal bool
		Events   []struct{ Type, Hash string }
		Chain    struct{ Valid bool }
		Attempts []struct {
			ID         string
			ErrorCode  *string `json:"error_code"`
			DurationMS *int64  `json:"duration_ms"`
		}
	}
	json.Unmarshal(rec.Body.Bytes(), &d)
	if d.State != "APPLIED" || !d.Terminal || len(d.Events) != 4 || d.Events[0].Hash == "" || !d.Chain.Valid {
		t.Errorf("detail = %s", rec.Body)
	}
	if len(d.Attempts) != 2 || d.Attempts[0].ID != "att_1" || d.Attempts[1].ErrorCode == nil || *d.Attempts[1].DurationMS != 1500 {
		t.Errorf("attempts = %s", rec.Body)
	}
}

func TestHistory_IsSessionOnlyAndScoped(t *testing.T) {
	f := newAuthFixture(t, true)
	org, _ := f.newOrg(t)
	_, otherCookie := f.newOrg(t)
	p := f.seedPayment(t, org, "INV-1", "PROPOSED")
	_, bearer := f.newAgent(t)

	if rec := f.do(t, http.MethodGet, "/v1/payments/"+string(p.Key), "", otherCookie); rec.Code != http.StatusNotFound {
		t.Errorf("another organisation's payment: %d, want 404", rec.Code)
	}
	var list struct{ Data []any }
	json.Unmarshal(f.do(t, http.MethodGet, "/v1/payments", "", otherCookie).Body.Bytes(), &list)
	if len(list.Data) != 0 {
		t.Errorf("another organisation lists %d payments, want 0", len(list.Data))
	}
	for _, path := range []string{"/v1/payments", "/v1/attempts", "/v1/beneficiaries"} {
		if rec := f.do(t, http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session: %d, want 401", path, rec.Code)
		}
		req := f.do(t, http.MethodGet, path, "", &http.Cookie{Name: SessionCookieName, Value: bearer})
		if req.Code != http.StatusUnauthorized {
			t.Errorf("%s with an agent key: %d, want 401 (dashboard reads are session-only)", path, req.Code)
		}
	}
}

func TestHistory_QueryValidation(t *testing.T) {
	f := newAuthFixture(t, true)
	_, cookie := f.newOrg(t)
	for _, path := range []string{
		"/v1/payments?from=yesterday",
		"/v1/payments?state=PAID",
		"/v1/payments?limit=lots",
		"/v1/attempts?kind=webhook",
		"/v1/attempts?outcome=ok,maybe",
	} {
		rec := f.do(t, http.MethodGet, path, "", cookie)
		if p := decodeProblem(t, rec); rec.Code != 400 || p.Code != "validation_failed" {
			t.Errorf("%s: %d %q, want 400 validation_failed", path, rec.Code, p.Code)
		}
	}
	if rec := f.do(t, http.MethodGet, "/v1/attempts?kind=rail_call&outcome=interrupted,rejected&from=2026-11-01T00:00:00Z", "", cookie); rec.Code != 200 {
		t.Errorf("a valid attempts query: %d %s", rec.Code, rec.Body)
	}
}

func TestAttempts_ListFiltersByKind(t *testing.T) {
	f := newAuthFixture(t, true)
	org, cookie := f.newOrg(t)
	ctx := context.Background()
	f.attempts.Record(ctx, app.Attempt{ID: "att_s", OrgID: org, Kind: app.KindAgentSubmission, Operation: app.OpSubmitIntent, Outcome: app.OutcomeInvalid, StartedAt: f.clock.Now()})
	f.attempts.Record(ctx, app.Attempt{ID: "att_r", OrgID: org, Kind: app.KindRailCall, Operation: app.OpGetTransfer, Outcome: app.OutcomeOK, StartedAt: f.clock.Now()})

	var list struct {
		Data []struct{ ID, Kind string }
	}
	json.Unmarshal(f.do(t, http.MethodGet, "/v1/attempts?kind=rail_call", "", cookie).Body.Bytes(), &list)
	if len(list.Data) != 1 || list.Data[0].ID != "att_r" {
		t.Errorf("rail calls = %+v", list.Data)
	}
}
