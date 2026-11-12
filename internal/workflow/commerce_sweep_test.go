package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

// countingSweeper stands in for the execution store: it counts sweeps
// and reports one stale execution on the second.
type countingSweeper struct {
	commerceapp.CheckoutStore
	sweeps int
}

func (s *countingSweeper) SweepExecutions(ctx context.Context, at time.Time) ([]commerce.Execution, error) {
	s.sweeps++
	if s.sweeps == 2 {
		return []commerce.Execution{{ID: "exe_1", OrgID: "org_1", MerchantID: "mi_1"}}, nil
	}
	return nil, nil
}

func sweepEnv(t *testing.T) (*testsuite.TestWorkflowEnvironment, *countingSweeper, *apptest.AuditLog) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	store, log := &countingSweeper{}, apptest.NewAuditLog()
	clock := apptest.NewClock()
	env.RegisterWorkflow(CommerceSweepWorkflow)
	env.RegisterActivity(&Activities{ExecutionSweep: &commerceapp.SweepExecutions{Checkouts: store, Clock: clock,
		Audit: &app.Audit{Log: log, IDs: &apptest.IDs{}, Clock: clock}}})
	return env, store, log
}

func TestCommerceSweepWorkflow_SweepsEveryIntervalDurably(t *testing.T) {
	env, store, log := sweepEnv(t)
	env.ExecuteWorkflow(CommerceSweepWorkflow, SweepInput{Iterations: 3})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 3, store.sweeps)
	events := log.All()
	require.Len(t, events, 1)
	require.Equal(t, app.AuditExecutionUnknown, events[0].Action)
}

// Running for good, it continues as new so its history stays small.
func TestCommerceSweepWorkflow_ContinuesAsNew(t *testing.T) {
	env, store, _ := sweepEnv(t)
	env.ExecuteWorkflow(CommerceSweepWorkflow, SweepInput{Iterations: 2, Forever: true})
	require.True(t, workflow.IsContinueAsNewError(env.GetWorkflowError()))
	require.Equal(t, 2, store.sweeps)
}
