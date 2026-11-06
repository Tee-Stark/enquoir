package app_test

import (
	"context"
	"errors"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
)

func TestListPayments_ValidatesTheFilter(t *testing.T) {
	clock := apptest.NewClock()
	cases := map[string]struct {
		f     app.PaymentFilter
		field string
	}{
		"limit too big":    {app.PaymentFilter{Limit: 201}, "limit"},
		"negative limit":   {app.PaymentFilter{Limit: -1}, "limit"},
		"unknown state":    {app.PaymentFilter{States: []string{"PAID"}}, "state"},
		"bad currency":     {app.PaymentFilter{Currency: "JPY"}, "currency"},
		"empty period":     {app.PaymentFilter{From: clock.Now(), To: clock.Now()}, "from"},
		"backwards period": {app.PaymentFilter{From: clock.Now(), To: clock.Now().Add(-1)}, "from"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := (&app.ListPayments{Payments: apptest.NewPayments()}).Run(context.Background(), testOrg, tc.f)
			var verr *app.ValidationError
			if !errors.As(err, &verr) || verr.Field != tc.field {
				t.Errorf("err = %v, want a ValidationError on %q", err, tc.field)
			}
		})
	}
}

func TestListPayments_DefaultsThePageSizeAndAcceptsReceived(t *testing.T) {
	payments := apptest.NewPayments()
	_, err := (&app.ListPayments{Payments: payments}).Run(context.Background(), testOrg, app.PaymentFilter{States: []string{app.StateReceived, "APPLIED"}})
	if err != nil || payments.LastFilter.Limit != 50 {
		t.Errorf("err %v, limit %d; want nil and the default 50", err, payments.LastFilter.Limit)
	}
}

func TestGetPayment_AssemblesHistoryChainAndAttempts(t *testing.T) {
	ctx := context.Background()
	key := domain.IntentKey("k1")
	payments := apptest.NewPayments(app.Payment{Key: key, OrgID: testOrg, State: "APPLIED"})
	events, attempts := apptest.NewEventLog(), apptest.NewAttempts()
	for _, typ := range []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "APPLIED"} {
		events.Append(ctx, testOrg, key, typ, "")
	}
	attempts.Record(ctx, app.Attempt{ID: "att_1", OrgID: testOrg, IntentKey: key, Kind: app.KindAgentSubmission})
	attempts.Record(ctx, app.Attempt{ID: "att_2", OrgID: testOrg, IntentKey: key, Kind: app.KindRailCall})
	attempts.Record(ctx, app.Attempt{ID: "att_other", OrgID: testOrg, IntentKey: "k2", Kind: app.KindRailCall})
	use := &app.GetPayment{Payments: payments, Events: events, Attempts: attempts}

	d, err := use.Run(ctx, testOrg, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Events) != 4 || d.ChainError != nil {
		t.Errorf("events %d, chain error %v; want 4 and a verifying chain", len(d.Events), d.ChainError)
	}
	if len(d.Attempts) != 2 || d.Attempts[0].ID != "att_1" || d.Attempts[1].ID != "att_2" {
		t.Errorf("attempts = %+v, want this payment's two, oldest first", d.Attempts)
	}

	if _, err := use.Run(ctx, "org_other", key); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("another organisation's payment: err = %v, want ErrNotFound", err)
	}
}

func TestGetPayment_ReportsATamperedChain(t *testing.T) {
	ctx := context.Background()
	key := domain.IntentKey("k1")
	events := tamperedLog{chain: domain.Chain{}.Append(key, "PROPOSED", "").Append(key, "APPLIED", "tr_1")}
	events.chain[1].Payload = "tr_EVIL"
	use := &app.GetPayment{Payments: apptest.NewPayments(app.Payment{Key: key, OrgID: testOrg}), Events: events, Attempts: apptest.NewAttempts()}
	d, err := use.Run(ctx, testOrg, key)
	if err != nil || d.ChainError == nil {
		t.Errorf("chain error = %v (err %v), want the tampering reported", d.ChainError, err)
	}
}

type tamperedLog struct{ chain domain.Chain }

func (l tamperedLog) Append(context.Context, domain.OrgID, domain.IntentKey, string, string) (domain.Event, error) {
	return domain.Event{}, nil
}
func (l tamperedLog) Load(context.Context, domain.OrgID, domain.IntentKey) (domain.Chain, error) {
	return l.chain, nil
}

func TestListAttempts_ValidatesTheFilter(t *testing.T) {
	cases := map[string]struct {
		f     app.AttemptFilter
		field string
	}{
		"kind":      {app.AttemptFilter{Kind: "webhook"}, "kind"},
		"operation": {app.AttemptFilter{Operation: "delete_everything"}, "operation"},
		"outcome":   {app.AttemptFilter{Outcomes: []app.AttemptOutcome{"maybe"}}, "outcome"},
		"limit":     {app.AttemptFilter{Limit: 500}, "limit"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := (&app.ListAttempts{Attempts: apptest.NewAttempts()}).Run(context.Background(), testOrg, tc.f)
			var verr *app.ValidationError
			if !errors.As(err, &verr) || verr.Field != tc.field {
				t.Errorf("err = %v, want a ValidationError on %q", err, tc.field)
			}
		})
	}
	if _, err := (&app.ListAttempts{Attempts: apptest.NewAttempts()}).Run(context.Background(), testOrg,
		app.AttemptFilter{Kind: app.KindRailCall, Outcomes: []app.AttemptOutcome{app.OutcomeInterrupted}}); err != nil {
		t.Errorf("a valid filter: %v", err)
	}
}
