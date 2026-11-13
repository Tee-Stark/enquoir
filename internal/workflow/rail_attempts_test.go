package workflow

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
)

// railCalls returns the rail_call attempts recorded, oldest first, as
// "operation:outcome".
func railCalls(attempts *apptest.Attempts) []string {
	var out []string
	for _, a := range attempts.All() {
		if a.Kind == app.KindRailCall {
			out = append(out, a.Operation+":"+string(a.Outcome))
		}
	}
	return out
}

func TestRailCalls_HappyPathRecordsEachCall(t *testing.T) {
	intent := testIntent(t, 10050)
	polls := 0
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{ID: "tr_1", Status: "PROCESSING"}, nil
		},
		getFunc: func(ctx context.Context, transferID string) (app.Transfer, error) {
			polls++
			if polls == 1 {
				return app.Transfer{ID: transferID, Status: app.TransferSent}, nil
			}
			return app.Transfer{ID: transferID, Status: app.TransferPaid}, nil
		},
	}
	te := newEnv(t, rail)
	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax})
	require.NoError(t, te.env.GetWorkflowError())

	require.Equal(t, []string{"create_transfer:ok", "get_transfer:ok", "get_transfer:ok"}, railCalls(te.attempts))
	all := te.attempts.All()
	create := all[0]
	require.Equal(t, testOrg, create.OrgID)
	require.Equal(t, intent.Key, create.IntentKey)
	require.Equal(t, string(intent.Key), create.RequestID, "the request ID sent is the intent key")
	require.Equal(t, "tr_1", create.TransferID)
	require.Equal(t, "PROCESSING", create.TransferStatus)
	require.Equal(t, 1, create.ActivityAttempt)
	require.NotNil(t, create.FinishedAt)
	require.Equal(t, "PAID", all[2].TransferStatus)
}

// A rejection and an unclear outcome are recorded with the rail's HTTP
// status and error code; the lookup that follows an unclear create is
// recorded too.
func TestRailCalls_FailuresCarryTheRailsStatusAndCode(t *testing.T) {
	t.Run("rejected", func(t *testing.T) {
		rail := &fakeRail{createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{}, &app.RailError{Kind: app.ErrRejected, HTTPStatus: 400, Code: "fee_unknown", Message: "airwallex: create transfer: rejected"}
		}}
		te := newEnv(t, rail)
		te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: testIntent(t, 10050), MaxAmountMinorUnits: highMax})
		require.Equal(t, []string{"create_transfer:rejected"}, railCalls(te.attempts))
		a := te.attempts.All()[0]
		require.Equal(t, 400, a.HTTPStatus)
		require.Equal(t, "fee_unknown", a.ErrorCode)
		require.Contains(t, a.ErrorMessage, "rejected")
	})

	t.Run("ambiguous, then found by lookup", func(t *testing.T) {
		rail := &fakeRail{
			createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
				return app.Transfer{}, &app.RailError{Kind: app.ErrAmbiguous, HTTPStatus: 503, Message: "airwallex: create transfer: status 503"}
			},
			findFunc: func(ctx context.Context, requestID string) (app.Transfer, bool, error) {
				return app.Transfer{ID: "tr_1", Status: app.TransferPaid}, true, nil
			},
		}
		te := newEnv(t, rail)
		te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: testIntent(t, 10050), MaxAmountMinorUnits: highMax})
		require.NoError(t, te.env.GetWorkflowError())
		require.Equal(t, []string{"create_transfer:ambiguous", "find_transfer:ok"}, railCalls(te.attempts))
		require.Equal(t, 503, te.attempts.All()[0].HTTPStatus)
	})

	t.Run("ambiguous, confirmed absent, retried", func(t *testing.T) {
		rail := &fakeRail{
			createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
				if callNum == 1 {
					return app.Transfer{}, &app.RailError{Kind: app.ErrAmbiguous, HTTPStatus: 504, Message: "timeout"}
				}
				return app.Transfer{ID: "tr_1", Status: app.TransferPaid}, nil
			},
			findFunc: func(ctx context.Context, requestID string) (app.Transfer, bool, error) {
				return app.Transfer{}, false, nil
			},
		}
		te := newEnv(t, rail)
		te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: testIntent(t, 10050), MaxAmountMinorUnits: highMax})
		require.NoError(t, te.env.GetWorkflowError())
		require.Equal(t, []string{"create_transfer:ambiguous", "find_transfer:not_found", "create_transfer:ok"}, railCalls(te.attempts))
	})
}

// The attempt is stored before the call leaves, so a worker that dies
// mid-call leaves an in-progress record behind, not nothing.
func TestRailCalls_TheAttemptIsRecordedBeforeTheCall(t *testing.T) {
	var te *testEnv
	rail := &fakeRail{createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
		all := te.attempts.All()
		require.NotEmpty(t, all, "no attempt recorded before the rail was called")
		require.Equal(t, app.OutcomeInProgress, all[len(all)-1].Outcome)
		return app.Transfer{ID: "tr_1", Status: app.TransferPaid}, nil
	}}
	te = newEnv(t, rail)
	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: testIntent(t, 10050), MaxAmountMinorUnits: highMax})
	require.NoError(t, te.env.GetWorkflowError())
}

func TestRailCalls_BeneficiaryCallsAreRecorded(t *testing.T) {
	b, err := domain.NewBeneficiary(testOrg, "trs_test", "Acme Ltd", "50001121", "021000021", domain.Checking, domain.BeneficiaryAddress{
		Street: "412 5th Avenue", City: "Seattle", State: "WA", Postcode: "98104",
	})
	require.NoError(t, err)
	registry := &fakeBeneficiaryRegistry{
		findFunc: func(ctx context.Context, key domain.BeneficiaryKey, callNum int) (string, bool, error) {
			return "", false, nil
		},
		createFunc: func(ctx context.Context, b domain.Beneficiary, callNum int) (string, error) {
			return "ben_1", nil
		},
	}
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	attempts := apptest.NewAttempts()
	env.RegisterWorkflow(BeneficiaryWorkflow)
	env.RegisterActivity(&Activities{Beneficiaries: registry, Attempts: attempts, IDs: &apptest.IDs{}, Clock: apptest.NewClock()})
	env.ExecuteWorkflow(BeneficiaryWorkflow, b)
	require.NoError(t, env.GetWorkflowError())

	require.Equal(t, []string{"find_beneficiary:not_found", "create_beneficiary:ok"}, railCalls(attempts))
	for _, a := range attempts.All() {
		require.Equal(t, testOrg, a.OrgID)
		require.Equal(t, b.Key, a.BeneficiaryKey)
	}
}
