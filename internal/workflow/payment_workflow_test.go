package workflow

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

func testIntent(t *testing.T, minorUnits int64) domain.Intent {
	t.Helper()
	intent, err := domain.NewIntent("beneficiary-1", minorUnits, "USD", fmt.Sprintf("ref-%s", t.Name()), "test")
	require.NoError(t, err)
	return intent
}

type testEnv struct {
	env    *testsuite.TestWorkflowEnvironment
	events *fakeEventLog
}

func newEnv(t *testing.T, rail app.Rail) *testEnv {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	events := newFakeEventLog()
	a := &Activities{
		Intents: newFakeIntentRepository(),
		Events:  events,
		Rail:    rail,
	}

	env.RegisterWorkflow(PaymentWorkflow)
	env.RegisterActivity(a)

	return &testEnv{env: env, events: events}
}

func (e *testEnv) eventTypes(t *testing.T, key domain.IntentKey) []string {
	t.Helper()
	chain, err := e.events.Load(context.Background(), key)
	require.NoError(t, err)
	require.NoError(t, chain.Verify())

	types := make([]string, len(chain))
	for i, ev := range chain {
		types[i] = ev.Type
	}
	return types
}

const highMax = 1_000_000_00 // $1,000,000.00 in minor units

func TestPaymentWorkflow_HappyPathEndsApplied(t *testing.T) {
	intent := testIntent(t, 10050)
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{ID: "tr_1", Status: app.TransferSent}, nil
		},
		getFunc: func(ctx context.Context, transferID string) (app.Transfer, error) {
			return app.Transfer{ID: transferID, Status: app.TransferPaid}, nil
		},
	}
	te := newEnv(t, rail)

	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())

	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.APPLIED, result.State)
	require.Equal(t, "tr_1", result.TransferID)

	require.Equal(t, []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "APPLIED"}, te.eventTypes(t, intent.Key))
}

func TestPaymentWorkflow_OverMaxEndsBlockedWithoutCallingRail(t *testing.T) {
	intent := testIntent(t, 10050)
	var createCalled atomic.Bool
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			createCalled.Store(true)
			return app.Transfer{}, nil
		},
	}
	te := newEnv(t, rail)

	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: 100})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())

	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.BLOCKED, result.State)

	require.False(t, createCalled.Load(), "Rail.CreateTransfer must never be called when the amount exceeds the maximum")
	require.Equal(t, []string{"PROPOSED", "BLOCKED"}, te.eventTypes(t, intent.Key))
}

func TestPaymentWorkflow_RecordsInFlightBeforeCallingRail(t *testing.T) {
	intent := testIntent(t, 10050)
	events := newFakeEventLog()

	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			types := []string{}
			chain, _ := events.Load(ctx, intent.Key)
			for _, ev := range chain {
				types = append(types, ev.Type)
			}
			require.Contains(t, types, "IN_FLIGHT", "IN_FLIGHT must be recorded before Rail is called")
			return app.Transfer{ID: "tr_1", Status: app.TransferPaid}, nil
		},
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	a := &Activities{Intents: newFakeIntentRepository(), Events: events, Rail: rail}
	env.RegisterWorkflow(PaymentWorkflow)
	env.RegisterActivity(a)

	env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

func TestPaymentWorkflow_AmbiguousCreateFoundByLookupSendsNoSecondCreate(t *testing.T) {
	intent := testIntent(t, 10050)
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{}, app.ErrAmbiguous
		},
		findFunc: func(ctx context.Context, requestID string) (app.Transfer, bool, error) {
			return app.Transfer{ID: "tr_existing", Status: app.TransferPaid}, true, nil
		},
	}
	te := newEnv(t, rail)

	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())

	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.APPLIED, result.State)
	require.Equal(t, "tr_existing", result.TransferID)

	require.Equal(t, 1, rail.calls(), "a found transfer must never trigger a second create")
	require.Equal(t, []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "UNKNOWN", "IN_FLIGHT", "APPLIED"}, te.eventTypes(t, intent.Key))
}

func TestPaymentWorkflow_AmbiguousCreateConfirmedAbsentRetriesSameRequestID(t *testing.T) {
	intent := testIntent(t, 10050)
	var requestIDs []string
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			requestIDs = append(requestIDs, string(intent.Key))
			if callNum == 1 {
				return app.Transfer{}, app.ErrAmbiguous
			}
			return app.Transfer{ID: "tr_1", Status: app.TransferPaid}, nil
		},
		findFunc: func(ctx context.Context, requestID string) (app.Transfer, bool, error) {
			return app.Transfer{}, false, nil
		},
	}
	te := newEnv(t, rail)

	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())

	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.APPLIED, result.State)

	require.Equal(t, 2, rail.calls())
	require.Equal(t, requestIDs[0], requestIDs[1], "the retry must reuse the exact same request ID")
	require.Equal(t, []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "UNKNOWN", "IN_FLIGHT", "APPLIED"}, te.eventTypes(t, intent.Key))
}

// An unclassified failure - a plain error, not one carrying our
// Rejected/Ambiguous tag - is exactly what a bare worker crash looks
// like to the workflow: an outcome it genuinely cannot tell apart from
// an ambiguous one. Temporal's own retry policy gets first crack at it
// (safe, since Rail.CreateTransfer is idempotent); once that budget is
// exhausted, the workflow's own UNKNOWN-then-lookup recovery takes
// over instead of failing the run.
func TestPaymentWorkflow_UnclassifiedCreateFailureRecoversAsAmbiguous(t *testing.T) {
	intent := testIntent(t, 10050)
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{}, errors.New("worker died mid-call")
		},
		findFunc: func(ctx context.Context, requestID string) (app.Transfer, bool, error) {
			return app.Transfer{ID: "tr_existing", Status: app.TransferPaid}, true, nil
		},
	}
	te := newEnv(t, rail)

	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())

	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.APPLIED, result.State)
	require.Equal(t, 3, rail.calls(), "Temporal's own retry policy should exhaust its budget before the workflow's own recovery takes over")
	require.Equal(t, []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "UNKNOWN", "IN_FLIGHT", "APPLIED"}, te.eventTypes(t, intent.Key))
}

func TestPaymentWorkflow_RejectionEndsFailed(t *testing.T) {
	intent := testIntent(t, 10050)
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{}, app.ErrRejected
		},
	}
	te := newEnv(t, rail)

	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())

	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.FAILED, result.State)

	require.Equal(t, []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "FAILED"}, te.eventTypes(t, intent.Key))
}

func TestPaymentWorkflow_CancelledTransferEndsFailedWithReason(t *testing.T) {
	intent := testIntent(t, 10050)
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{ID: "tr_1", Status: app.TransferSent}, nil
		},
		getFunc: func(ctx context.Context, transferID string) (app.Transfer, error) {
			return app.Transfer{ID: transferID, Status: app.TransferCancelled, FailureType: "bank_return", FailureReason: "account closed"}, nil
		},
	}
	te := newEnv(t, rail)

	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())

	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.FAILED, result.State)
	require.Equal(t, "account closed", result.FailureReason)

	chain, err := te.events.Load(context.Background(), intent.Key)
	require.NoError(t, err)
	require.Equal(t, "account closed", chain[len(chain)-1].Payload)
}
