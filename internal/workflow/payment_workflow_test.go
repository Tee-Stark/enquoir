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
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
	"enquoir/internal/policy"
)

const testOrg domain.OrgID = "org_test"

func testIntent(t *testing.T, minorUnits int64) domain.Intent {
	t.Helper()
	intent, err := domain.NewIntent(testOrg, "trs_test", "beneficiary-1", minorUnits, "USD", fmt.Sprintf("ref-%s", t.Name()), "test")
	require.NoError(t, err)
	return intent
}

type testEnv struct {
	env        *testsuite.TestWorkflowEnvironment
	events     *apptest.EventLog
	attempts   *apptest.Attempts
	activities *Activities
}

func newEnv(t *testing.T, rail app.Rail) *testEnv {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	events, attempts := apptest.NewEventLog(), apptest.NewAttempts()
	a := &Activities{
		Intents:  newFakeIntentRepository(),
		Events:   events,
		Rail:     rail,
		Attempts: attempts,
		IDs:      &apptest.IDs{},
		Clock:    apptest.NewClock(),
	}

	env.RegisterWorkflow(PaymentWorkflow)
	env.RegisterActivity(a)

	return &testEnv{env: env, events: events, attempts: attempts, activities: a}
}

func (e *testEnv) eventTypes(t *testing.T, key domain.IntentKey) []string {
	t.Helper()
	chain, err := e.events.Load(context.Background(), testOrg, key)
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
	events := apptest.NewEventLog()

	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			types := []string{}
			chain, _ := events.Load(ctx, testOrg, intent.Key)
			for _, ev := range chain {
				types = append(types, ev.Type)
			}
			require.Contains(t, types, "IN_FLIGHT", "IN_FLIGHT must be recorded before Rail is called")
			return app.Transfer{ID: "tr_1", Status: app.TransferPaid}, nil
		},
	}

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	a := &Activities{Intents: newFakeIntentRepository(), Events: events, Rail: rail, Attempts: apptest.NewAttempts(), IDs: &apptest.IDs{}, Clock: apptest.NewClock()}
	env.RegisterWorkflow(PaymentWorkflow)
	env.RegisterActivity(a)

	env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
}

// A payment escalated at submission waits in AWAITING_APPROVAL for a
// signal, rechecks the policy on approval, and proceeds normally.
func TestPaymentWorkflow_Escalated_ApprovalRechecksPolicyAndProceeds(t *testing.T) {
	intent := testIntent(t, 10050)
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{ID: "tr_1", Status: app.TransferPaid}, nil
		},
	}
	te := newEnv(t, rail)
	policies := &apptest.FixedPolicy{Outcome: policy.Allow}
	te.activities.Policies = policies

	te.env.RegisterDelayedCallback(func() {
		te.env.SignalWorkflow(EscalationSignalName, EscalationSignal{Approved: true, DecidedBy: "usr_owner"})
	}, 0)
	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax, Escalate: true})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())
	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.APPLIED, result.State)
	require.Equal(t, []string{"PROPOSED", "AWAITING_APPROVAL", "AUTHORIZED", "IN_FLIGHT", "APPLIED"}, te.eventTypes(t, intent.Key))
	require.Len(t, policies.Calls, 1, "approval must recheck the policy exactly once")
}

// The common case: the recheck still escalates (nothing about the
// amount or the threshold changed between submission and approval).
// That must not re-block the payment - a human just approved it - so
// Escalate-again proceeds exactly like Allow does.
func TestPaymentWorkflow_Escalated_StillEscalatesOnRecheckButProceeds(t *testing.T) {
	intent := testIntent(t, 10050)
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{ID: "tr_1", Status: app.TransferPaid}, nil
		},
	}
	te := newEnv(t, rail)
	policies := &apptest.FixedPolicy{Outcome: policy.Escalate, Reasons: []policy.Reason{{Code: policy.ReasonOverApprovalThreshold}}}
	te.activities.Policies = policies

	te.env.RegisterDelayedCallback(func() {
		te.env.SignalWorkflow(EscalationSignalName, EscalationSignal{Approved: true, DecidedBy: "usr_owner"})
	}, 0)
	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax, Escalate: true})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())
	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.APPLIED, result.State)
	require.Len(t, policies.Calls, 1, "approval must recheck the policy exactly once")
}

// Approval rechecks the limits: if they no longer allow it (spent
// elsewhere meanwhile), it is denied, not let through on a stale
// decision from submission time.
func TestPaymentWorkflow_Escalated_ApprovalDeniedOnRecheck(t *testing.T) {
	intent := testIntent(t, 10050)
	var createCalled atomic.Bool
	rail := &fakeRail{createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
		createCalled.Store(true)
		return app.Transfer{}, nil
	}}
	te := newEnv(t, rail)
	te.activities.Policies = &apptest.FixedPolicy{Outcome: policy.Deny, Reasons: []policy.Reason{{Code: policy.ReasonOver24hTotal}}}

	te.env.RegisterDelayedCallback(func() {
		te.env.SignalWorkflow(EscalationSignalName, EscalationSignal{Approved: true, DecidedBy: "usr_owner"})
	}, 0)
	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax, Escalate: true})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())
	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.BLOCKED, result.State)
	require.False(t, createCalled.Load())
}

// A rejection signal blocks the payment without ever calling the rail.
func TestPaymentWorkflow_Escalated_RejectionBlocksWithoutCallingRail(t *testing.T) {
	intent := testIntent(t, 10050)
	var createCalled atomic.Bool
	rail := &fakeRail{createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
		createCalled.Store(true)
		return app.Transfer{}, nil
	}}
	te := newEnv(t, rail)

	te.env.RegisterDelayedCallback(func() {
		te.env.SignalWorkflow(EscalationSignalName, EscalationSignal{Approved: false, DecidedBy: "usr_owner"})
	}, 0)
	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax, Escalate: true})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())
	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.BLOCKED, result.State)
	require.False(t, createCalled.Load())
	require.Equal(t, []string{"PROPOSED", "AWAITING_APPROVAL", "BLOCKED"}, te.eventTypes(t, intent.Key))
}

// flakyPolicy fails EvaluateAndReserve a fixed number of times before
// succeeding - standing in for a transient Postgres blip during the
// approval recheck.
type flakyPolicy struct {
	policy.Store
	failures int32
	outcome  policy.Outcome
}

func (f *flakyPolicy) EvaluateAndReserve(ctx context.Context, org domain.OrgID, in policy.EvaluationInput) (policy.Decision, error) {
	if atomic.AddInt32(&f.failures, -1) >= 0 {
		return policy.Decision{}, errors.New("transient failure")
	}
	return policy.Decision{Outcome: f.outcome}, nil
}

// A transient failure rechecking the policy on approval must not
// strand the payment at AWAITING_APPROVAL forever - RecheckPolicy
// retries past the modest 3-attempt budget every other activity in
// this workflow uses, since it is a pure read with nothing to
// duplicate.
func TestPaymentWorkflow_Escalated_RecheckSurvivesMoreFailuresThanTheDefaultBudget(t *testing.T) {
	intent := testIntent(t, 10050)
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{ID: "tr_1", Status: app.TransferPaid}, nil
		},
	}
	te := newEnv(t, rail)
	te.activities.Policies = &flakyPolicy{failures: 5, outcome: policy.Allow}

	te.env.RegisterDelayedCallback(func() {
		te.env.SignalWorkflow(EscalationSignalName, EscalationSignal{Approved: true, DecidedBy: "usr_owner"})
	}, 0)
	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax, Escalate: true})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())
	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.APPLIED, result.State)
}

// No decision within 24 hours denies it too - the test environment
// auto-skips virtual time when nothing else is runnable, so this
// completes without a real 24-hour wait.
func TestPaymentWorkflow_Escalated_TimesOutAfter24Hours(t *testing.T) {
	intent := testIntent(t, 10050)
	var createCalled atomic.Bool
	rail := &fakeRail{createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
		createCalled.Store(true)
		return app.Transfer{}, nil
	}}
	te := newEnv(t, rail)

	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax, Escalate: true})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())
	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.BLOCKED, result.State)
	require.False(t, createCalled.Load())
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

	chain, err := te.events.Load(context.Background(), testOrg, intent.Key)
	require.NoError(t, err)
	require.Equal(t, "account closed", chain[len(chain)-1].Payload)
}

func TestPaymentWorkflow_UnclearLookupIsRetriedUntilItAnswers(t *testing.T) {
	// A rail outage during the lookup used to fail the run and strand
	// the intent in UNKNOWN with nothing left to resolve it.
	intent := testIntent(t, 10050)
	var findCalls atomic.Int32
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{}, fmt.Errorf("timeout: %w", app.ErrAmbiguous)
		},
		findFunc: func(ctx context.Context, requestID string) (app.Transfer, bool, error) {
			if findCalls.Add(1) <= 2 {
				return app.Transfer{}, false, fmt.Errorf("login 403: %w", app.ErrAmbiguous)
			}
			return app.Transfer{ID: "tr_landed", Status: app.TransferPaid}, true, nil
		},
	}
	te := newEnv(t, rail)

	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax})

	require.True(t, te.env.IsWorkflowCompleted())
	require.NoError(t, te.env.GetWorkflowError())
	var result PaymentResult
	require.NoError(t, te.env.GetWorkflowResult(&result))
	require.Equal(t, domain.APPLIED, result.State)
	require.Equal(t, 1, rail.calls(), "an unclear lookup must never lead to a second create")
	require.Equal(t, int32(3), findCalls.Load())
	require.Equal(t, []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "UNKNOWN", "IN_FLIGHT", "APPLIED"}, te.eventTypes(t, intent.Key))
}

func TestPaymentWorkflow_UnclearPollIsRetriedUntilItAnswers(t *testing.T) {
	intent := testIntent(t, 10050)
	var getCalls atomic.Int32
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{ID: "tr_1", Status: app.TransferSent}, nil
		},
		getFunc: func(ctx context.Context, transferID string) (app.Transfer, error) {
			if getCalls.Add(1) <= 2 {
				return app.Transfer{}, fmt.Errorf("status 502: %w", app.ErrAmbiguous)
			}
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
	require.Equal(t, []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "APPLIED"}, te.eventTypes(t, intent.Key))
}

func TestPaymentWorkflow_DuplicateTransfersStopsTheRunLoudly(t *testing.T) {
	// Two transfers for one request ID is a double payment already on
	// the rail. Retrying can't fix it and nothing may proceed: the run
	// fails, leaving the intent in UNKNOWN for a person to investigate.
	intent := testIntent(t, 10050)
	var findCalls atomic.Int32
	rail := &fakeRail{
		createFunc: func(ctx context.Context, intent domain.Intent, callNum int) (app.Transfer, error) {
			return app.Transfer{}, fmt.Errorf("timeout: %w", app.ErrAmbiguous)
		},
		findFunc: func(ctx context.Context, requestID string) (app.Transfer, bool, error) {
			findCalls.Add(1)
			return app.Transfer{}, false, fmt.Errorf("tr_1, tr_2: %w", app.ErrDuplicateTransfers)
		},
	}
	te := newEnv(t, rail)

	te.env.ExecuteWorkflow(PaymentWorkflow, PaymentInput{Intent: intent, MaxAmountMinorUnits: highMax})

	require.True(t, te.env.IsWorkflowCompleted())
	require.Error(t, te.env.GetWorkflowError())
	require.Equal(t, 1, rail.calls())
	require.Equal(t, int32(1), findCalls.Load(), "a duplicate is not an unclear outcome and must not be retried")
	require.Equal(t, []string{"PROPOSED", "AUTHORIZED", "IN_FLIGHT", "UNKNOWN"}, te.eventTypes(t, intent.Key))
}
