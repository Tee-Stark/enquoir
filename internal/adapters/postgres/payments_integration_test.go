//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

func TestPaymentReader_ListsAndFiltersAnOrganisationsPayments(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	accounts := NewAccountStore(pool)
	org, _ := newTestOrg(t, accounts)
	other, _ := newTestOrg(t, accounts)
	agent, _ := newAgentFor(t, pool, org.ID)
	repo, events, log, reader := NewIntentRepository(pool), NewEventLog(pool), NewAttemptLog(pool), NewPaymentReader(pool)

	payee := ids.NewID("ben")
	NewBeneficiaryStore(pool).Save(ctx, app.RegisteredBeneficiary{OrgID: org.ID, TreasuryID: treasuryOf(org.ID), Key: domain.BeneficiaryKey(ids.NewID("bk")), RailID: payee,
		Name: "Acme Ltd", AccountLast4: "1121", RoutingNumber: "021000021", Category: domain.Checking, CreatedAt: time.Now()})

	newIntent := func(o domain.OrgID, ref string, history ...string) domain.Intent {
		i, err := domain.NewIntent(o, treasuryOf(o), payee, 1050, "USD", ref, "")
		if err != nil {
			t.Fatal(err)
		}
		if o == org.ID {
			i.Agent = agent
		}
		if _, _, err := repo.Save(ctx, i); err != nil {
			t.Fatal(err)
		}
		cleanupIntent(t, pool, i.Key)
		for _, h := range history {
			payload := ""
			if h == "APPLIED" {
				payload = "tr_" + ref
			}
			if _, err := events.Append(ctx, o, i.Key, h, payload); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(2 * time.Millisecond) // distinct created_at
		return i
	}
	received := newIntent(org.ID, "R-1")
	applied := newIntent(org.ID, "R-2", "PROPOSED", "AUTHORIZED", "IN_FLIGHT", "APPLIED")
	blocked := newIntent(org.ID, "R-3", "PROPOSED", "BLOCKED")
	newIntent(other.ID, "R-4", "PROPOSED")

	log.Record(ctx, app.Attempt{ID: ids.NewID("att"), OrgID: org.ID, Kind: app.KindAgentSubmission, Operation: app.OpSubmitIntent, Outcome: app.OutcomeStarted, IntentKey: applied.Key, StartedAt: time.Now()})
	log.Record(ctx, app.Attempt{ID: ids.NewID("att"), OrgID: org.ID, Kind: app.KindAgentSubmission, Operation: app.OpSubmitIntent, Outcome: app.OutcomeDeduplicated, IntentKey: applied.Key, StartedAt: time.Now()})
	log.Record(ctx, app.Attempt{ID: ids.NewID("att"), OrgID: org.ID, Kind: app.KindRailCall, Operation: app.OpCreateTransfer, Outcome: app.OutcomeOK, IntentKey: applied.Key, StartedAt: time.Now()})

	page, err := reader.ListPayments(ctx, org.ID, app.PaymentFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Payments) != 3 {
		t.Fatalf("%d payments, want the organisation's 3", len(page.Payments))
	}
	if page.Payments[0].Key != blocked.Key || page.Payments[2].Key != received.Key {
		t.Error("not newest first")
	}
	got, err := reader.GetPayment(ctx, org.ID, applied.Key)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "APPLIED" || got.TransferID != "tr_R-2" || got.PayeeName != "Acme Ltd" || got.AgentName != "test agent" ||
		got.Submissions != 2 || got.RailCalls != 1 || got.Amount.MinorUnits() != 1050 || got.StateChangedAt == nil {
		t.Errorf("applied payment = %+v", got)
	}
	if r, _ := reader.GetPayment(ctx, org.ID, received.Key); r.State != app.StateReceived {
		t.Errorf("a payment with no events reads %q, want RECEIVED", r.State)
	}
	if b, _ := reader.GetPayment(ctx, org.ID, blocked.Key); b.FailureReason != "" || b.State != "BLOCKED" {
		t.Errorf("blocked = %+v", b)
	}
	if _, err := reader.GetPayment(ctx, other.ID, applied.Key); err != app.ErrNotFound {
		t.Errorf("another organisation's payment: err = %v, want ErrNotFound", err)
	}

	for name, tc := range map[string]struct {
		f    app.PaymentFilter
		want int
	}{
		"state":         {app.PaymentFilter{States: []string{"APPLIED", "BLOCKED"}}, 2},
		"received":      {app.PaymentFilter{States: []string{app.StateReceived}}, 1},
		"agent":         {app.PaymentFilter{AgentID: agent}, 3},
		"reference":     {app.PaymentFilter{Reference: "R-2"}, 1},
		"currency":      {app.PaymentFilter{Currency: "EUR"}, 0},
		"future period": {app.PaymentFilter{From: time.Now().Add(time.Hour)}, 0},
	} {
		tc.f.Limit = 10
		p, err := reader.ListPayments(ctx, org.ID, tc.f)
		if err != nil || len(p.Payments) != tc.want {
			t.Errorf("filter %s: %d payments (err %v), want %d", name, len(p.Payments), err, tc.want)
		}
	}

	first, _ := reader.ListPayments(ctx, org.ID, app.PaymentFilter{Limit: 2})
	second, _ := reader.ListPayments(ctx, org.ID, app.PaymentFilter{Limit: 2, Cursor: first.NextCursor})
	if first.NextCursor == "" || len(second.Payments) != 1 || second.Payments[0].Key != received.Key || second.NextCursor != "" {
		t.Errorf("paging: first %d (cursor %q), second %+v", len(first.Payments), first.NextCursor, second.Payments)
	}

	list, _ := NewBeneficiaryStore(pool).List(ctx, org.ID)
	if len(list) != 1 || list[0].Name != "Acme Ltd" || list[0].AccountLast4 != "1121" {
		t.Errorf("beneficiaries = %+v", list)
	}
}
