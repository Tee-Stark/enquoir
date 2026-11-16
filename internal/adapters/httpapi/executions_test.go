package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type reportBody struct {
	executionBody
	Result         string  `json:"result"`
	PaymentRef     *string `json:"provider_payment_reference"`
	IdempotencyKey string  `json:"idempotency_key"`
	ReportDeadline string  `json:"report_deadline"`
	History        []struct {
		Kind   string  `json:"kind"`
		Source *string `json:"source"`
	} `json:"history"`
}

// redeemedExecution submits and redeems an autonomous checkout of total.
func (f *authFixture) redeemedExecution(t *testing.T, w mandateWorld, m, ext string, total int64) executionBody {
	t.Helper()
	c := f.submitForGrant(t, w, w.checkoutJSON(ext, m, "USD", total))
	rec := f.doBearer(t, http.MethodPost, "/v1/commerce/grants/redeem", redeemJSON(*c.Authorization.Grant, ext, c.Hash), w.key)
	var e executionBody
	json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusCreated {
		t.Fatalf("redeem: %d %s", rec.Code, rec.Body)
	}
	return e
}

func outcomeJSON(outcome, ref string, total int64, extra string) string {
	r := ""
	if ref != "" {
		r = fmt.Sprintf(`"provider_payment_reference":%q,`, ref)
	}
	return fmt.Sprintf(`{"outcome":%q,%s"provider":"stripe","amount_minor":%d,"currency":"USD"%s}`, outcome, r, total, extra)
}

func (f *authFixture) report(t *testing.T, key, path, body string) (int, reportBody) {
	t.Helper()
	rec := f.doBearer(t, http.MethodPost, path, body, key)
	var r reportBody
	json.Unmarshal(rec.Body.Bytes(), &r)
	return rec.Code, r
}

func TestExecutions_ReportOutcomesAndHistory(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)
	m := f.approvedMandate(t, w, "m1", "autonomous")
	e := f.redeemedExecution(t, w, m, "c1", 100000)
	path := "/v1/commerce/executions/" + e.ID

	var got reportBody
	json.Unmarshal(f.doBearer(t, http.MethodGet, path, "", w.key).Body.Bytes(), &got)
	if got.IdempotencyKey != "enquoir-"+e.ID || got.ReportDeadline == "" || got.Status != "pending" || len(got.History) != 1 {
		t.Fatalf("execution: %+v", got)
	}
	code, r := f.report(t, w.key, path+"/outcome", outcomeJSON("succeeded", "pi_1", 100000, ""))
	if code != http.StatusOK || r.Result != "applied" || r.Status != "succeeded" || r.PaymentRef == nil || *r.PaymentRef != "pi_1" {
		t.Fatalf("report: %d %+v", code, r)
	}
	if code, r := f.report(t, w.key, path+"/outcome", outcomeJSON("succeeded", "pi_1", 100000, "")); code != 200 || r.Result != "duplicate" {
		t.Errorf("repeat: %d %s", code, r.Result)
	}
	rec := f.doBearer(t, http.MethodPost, path+"/reconcile", outcomeJSON("failed", "pi_1", 100000, ""), w.key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "outcome_conflict" {
		t.Errorf("contradiction: %d %q", rec.Code, p.Code)
	}
	rec = f.doBearer(t, http.MethodPost, path+"/outcome", outcomeJSON("succeeded", "pi_1", 99999, ""), w.key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusUnprocessableEntity || p.Code != "report_mismatch" {
		t.Errorf("another amount: %d %q", rec.Code, p.Code)
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, path, "", w.key).Body.Bytes(), &got)
	var kinds []string
	for _, h := range got.History {
		kinds = append(kinds, h.Kind)
	}
	if strings.Join(kinds, ",") != "redeemed,reported,duplicate,refused,refused" {
		t.Errorf("history = %v", kinds)
	}
	var ck checkoutBody
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/checkouts/"+e.CheckoutID, "", w.key).Body.Bytes(), &ck)
	if ck.Status != "paid" {
		t.Errorf("checkout = %s", ck.Status)
	}
	rec = f.doBearer(t, http.MethodPost, path+"/outcome", strings.Replace(outcomeJSON("failed", "", 100000, ""), `{`, `{"card_number":"4111111111111111",`, 1), w.key)
	if p := decodeProblem(t, rec); rec.Code != 400 || p.Field != "card_number" {
		t.Errorf("card data: %d %+v", rec.Code, p)
	}
	other := f.newMandateWorld(t)
	if rec := f.doBearer(t, http.MethodPost, path+"/outcome", outcomeJSON("failed", "", 100000, ""), other.key); rec.Code != http.StatusNotFound {
		t.Errorf("another merchant: %d", rec.Code)
	}
}

func TestExecutions_UnknownWebhooksAndReconciliation(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)
	m := f.approvedMandate(t, w, "m1", "autonomous")
	e := f.redeemedExecution(t, w, m, "c1", 1000)
	path := "/v1/commerce/executions/" + e.ID
	if code, r := f.report(t, w.key, path+"/outcome", outcomeJSON("unknown", "", 1000, "")); code != 200 || r.Status != "unknown" {
		t.Fatalf("unknown: %d %+v", code, r)
	}
	rec := f.doBearer(t, http.MethodPost, path+"/outcome", outcomeJSON("succeeded", "pi_1", 1000, ""), w.key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "reconciliation_required" {
		t.Errorf("a direct report on unknown: %d %q", rec.Code, p.Code)
	}
	var list struct {
		Data []executionBody `json:"data"`
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/executions?status=unknown", "", w.key).Body.Bytes(), &list)
	if len(list.Data) != 1 || list.Data[0].ID != e.ID {
		t.Errorf("to reconcile: %+v", list.Data)
	}
	hook := outcomeJSON("succeeded", "pi_1", 1000, `,"provider_event_id":"evt_1"`)
	if code, r := f.report(t, w.key, path+"/outcome", hook); code != 200 || r.Result != "applied" || r.Status != "succeeded" {
		t.Fatalf("webhook: %d %+v", code, r)
	}
	if code, r := f.report(t, w.key, path+"/outcome", hook); code != 200 || r.Result != "duplicate" {
		t.Errorf("redelivered webhook: %d %s", code, r.Result)
	}
	rec = f.doBearer(t, http.MethodPost, path+"/outcome", outcomeJSON("failed", "pi_1", 1000, `,"provider_event_id":"evt_1"`), w.key)
	if p := decodeProblem(t, rec); rec.Code != http.StatusConflict || p.Code != "event_conflict" {
		t.Errorf("same event, other content: %d %q", rec.Code, p.Code)
	}
	e2 := f.redeemedExecution(t, w, m, "c2", 1000)
	f.report(t, w.key, "/v1/commerce/executions/"+e2.ID+"/outcome", outcomeJSON("unknown", "", 1000, ""))
	if code, r := f.report(t, w.key, "/v1/commerce/executions/"+e2.ID+"/reconcile", outcomeJSON("failed", "", 1000, "")); code != 200 || r.Status != "failed" {
		t.Errorf("reconcile: %d %+v", code, r)
	}
	rec = f.doBearer(t, http.MethodPost, "/v1/commerce/executions/"+e2.ID+"/reconcile", outcomeJSON("failed", "", 1000, `,"provider_event_id":"evt_9"`), w.key)
	if p := decodeProblem(t, rec); rec.Code != 400 || p.Field != "provider_event_id" {
		t.Errorf("an event ID on a reconciliation: %d %+v", rec.Code, p)
	}
}

func TestCheckoutsListAndMandateUsage(t *testing.T) {
	f := newAuthFixture(t, true)
	w := f.newMandateWorld(t)
	m := f.approvedMandate(t, w, "m1", "autonomous")
	paid := f.redeemedExecution(t, w, m, "c1", 100000)
	f.report(t, w.key, "/v1/commerce/executions/"+paid.ID+"/outcome", outcomeJSON("succeeded", "pi_1", 100000, ""))
	f.clock.Advance(time.Second)
	f.submitForGrant(t, w, w.checkoutJSON("c2", m, "USD", 50000))
	f.clock.Advance(time.Second)
	f.submitForGrant(t, w, w.checkoutJSON("c3", m, "EUR", 1000))

	var list struct {
		Data       []checkoutBody `json:"data"`
		NextCursor *string        `json:"next_cursor"`
	}
	rec := f.doBearer(t, http.MethodGet, "/v1/commerce/checkouts?mandate_id="+m+"&limit=2", "", w.key)
	json.Unmarshal(rec.Body.Bytes(), &list)
	if rec.Code != 200 || len(list.Data) != 2 || list.Data[0].ExternalID != "c3" || list.NextCursor == nil {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/checkouts?status=paid", "", w.key).Body.Bytes(), &list)
	if len(list.Data) != 1 || list.Data[0].ExternalID != "c1" {
		t.Errorf("paid: %+v", list.Data)
	}
	if rec := f.doBearer(t, http.MethodGet, "/v1/commerce/checkouts?status=lost", "", w.key); rec.Code != 400 {
		t.Errorf("bad status: %d", rec.Code)
	}

	var md struct {
		Usage *struct {
			Purchases     int    `json:"purchases"`
			PurchasesLeft *int   `json:"purchases_left"`
			SpentMinor    int64  `json:"spent_minor"`
			PaidMinor     int64  `json:"paid_minor"`
			HeldMinor     int64  `json:"held_minor"`
			Remaining     int64  `json:"remaining_minor"`
			Currency      string `json:"currency"`
		} `json:"usage"`
	}
	json.Unmarshal(f.doBearer(t, http.MethodGet, "/v1/commerce/mandates/"+m, "", w.key).Body.Bytes(), &md)
	u := md.Usage
	if u == nil || u.Purchases != 2 || u.PurchasesLeft == nil || *u.PurchasesLeft != 1 || u.SpentMinor != 150000 || u.PaidMinor != 100000 ||
		u.HeldMinor != 50000 || u.Remaining != 150000 || u.Currency != "USD" {
		t.Errorf("usage = %+v", u)
	}
}

// Members see a merchant's commerce activity, read-only, on the dashboard.
func TestDashboard_MerchantActivity(t *testing.T) {
	f := newAuthFixture(t, true)
	org, owner := f.newOrg(t)
	issued := f.createIntegration(t, owner, "Acme")
	key := issued.Key.Secret
	var c customerBody
	json.Unmarshal(f.doBearer(t, http.MethodPost, "/v1/commerce/customers", `{"external_id":"cust_42","email":"ada@example.com"}`, key).Body.Bytes(), &c)
	var a commerceAgentBody
	json.Unmarshal(f.doBearer(t, http.MethodPost, "/v1/commerce/agents", `{"external_id":"assistant","name":"Bot"}`, key).Body.Bytes(), &a)
	var card fundingBody
	json.Unmarshal(f.doBearer(t, http.MethodPost, "/v1/commerce/funding-methods", fundingJSON(c.ID, "mfm_1"), key).Body.Bytes(), &card)
	w := mandateWorld{key: key, customer: c.ID, agent: a.ID, card: card.ID}
	m := f.approvedMandate(t, w, "m1", "autonomous")
	f.redeemedExecution(t, w, m, "c1", 1000)

	viewer := f.memberAs(t, org, "viewer")
	base := "/v1/merchant-integrations/" + issued.Integration.ID
	for _, path := range []string{base + "/checkouts", base + "/mandates", base + "/executions"} {
		rec := f.do(t, http.MethodGet, path, "", viewer)
		var list struct {
			Data []json.RawMessage `json:"data"`
		}
		json.Unmarshal(rec.Body.Bytes(), &list)
		if rec.Code != 200 || len(list.Data) != 1 {
			t.Errorf("viewer GET %s: %d %s", path, rec.Code, rec.Body)
		}
	}
	_, stranger := f.newOrg(t)
	if rec := f.do(t, http.MethodGet, base+"/checkouts", "", stranger); rec.Code != http.StatusNotFound {
		t.Errorf("another organisation: %d", rec.Code)
	}
	if rec := f.doBearer(t, http.MethodGet, base+"/checkouts", "", key); rec.Code != http.StatusUnauthorized {
		t.Errorf("the merchant key on the dashboard: %d", rec.Code)
	}
}
