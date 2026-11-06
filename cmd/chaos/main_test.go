package main

import (
	"context"
	"testing"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/chaos"
	"enquoir/internal/domain"
	enquoirworkflow "enquoir/internal/workflow"
)

type railWith struct {
	app.Rail
	found bool
}

func (r railWith) FindTransferByRequestID(ctx context.Context, requestID string) (app.Transfer, bool, error) {
	return app.Transfer{ID: "tr_1"}, r.found, nil
}

func TestRejectedPoint(t *testing.T) {
	intent, _ := domain.NewIntent("org_test", "payee", 1000, "USD", "ref", "")
	failed := enquoirworkflow.PaymentResult{State: domain.FAILED, FailureReason: "fee_unknown"}

	cases := []struct {
		name             string
		history          []string
		found            bool
		result           enquoirworkflow.PaymentResult
		wantInconclusive bool
	}{
		{"clean rejection: inconclusive", []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "FAILED"}, false, failed, true},
		{"a transfer exists after all: fail", []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "FAILED"}, true, failed, false},
		{"blocked, not rejected: fail", []string{"PROPOSED", "BLOCKED"}, false, enquoirworkflow.PaymentResult{State: domain.BLOCKED}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := apptest.NewEventLog()
			for _, typ := range tc.history {
				events.Append(context.Background(), intent.OrgID, intent.Key, typ, "")
			}
			r := rejectedPoint(context.Background(), railWith{found: tc.found}, events, intent, chaos.DuringCall, tc.result)
			if r.Inconclusive != tc.wantInconclusive || r.Passed {
				t.Errorf("got inconclusive=%v passed=%v (%s), want inconclusive=%v and never passed", r.Inconclusive, r.Passed, r.Detail, tc.wantInconclusive)
			}
		})
	}
}
