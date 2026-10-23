package workflow

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

func testBeneficiary(t *testing.T) domain.Beneficiary {
	t.Helper()
	b, err := domain.NewBeneficiary("Acme Ltd", "50001121", "021000021", domain.BeneficiaryAddress{
		Street: "412 5th Avenue", City: "Seattle", State: "WA", Postcode: "98104",
	})
	require.NoError(t, err)
	return b
}

func runBeneficiaryWorkflow(t *testing.T, registry *fakeBeneficiaryRegistry, b domain.Beneficiary) (string, error) {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(BeneficiaryWorkflow)
	env.RegisterActivity(&Activities{Beneficiaries: registry})

	env.ExecuteWorkflow(BeneficiaryWorkflow, b)
	require.True(t, env.IsWorkflowCompleted())
	if err := env.GetWorkflowError(); err != nil {
		return "", err
	}
	var id string
	require.NoError(t, env.GetWorkflowResult(&id))
	return id, nil
}

func notFound(ctx context.Context, key domain.BeneficiaryKey, callNum int) (string, bool, error) {
	return "", false, nil
}

func TestBeneficiaryWorkflow_AlreadyRegisteredReturnsItWithoutCreating(t *testing.T) {
	registry := &fakeBeneficiaryRegistry{
		findFunc: func(ctx context.Context, key domain.BeneficiaryKey, callNum int) (string, bool, error) {
			return "ben_existing", true, nil
		},
		createFunc: func(ctx context.Context, b domain.Beneficiary, callNum int) (string, error) {
			return "", fmt.Errorf("create must not be called")
		},
	}

	id, err := runBeneficiaryWorkflow(t, registry, testBeneficiary(t))
	require.NoError(t, err)
	require.Equal(t, "ben_existing", id)
	require.Equal(t, 0, registry.creates())
}

func TestBeneficiaryWorkflow_AbsentIsCreated(t *testing.T) {
	b := testBeneficiary(t)
	registry := &fakeBeneficiaryRegistry{
		findFunc: notFound,
		createFunc: func(ctx context.Context, got domain.Beneficiary, callNum int) (string, error) {
			require.Equal(t, b.Key, got.Key)
			return "ben_new", nil
		},
	}

	id, err := runBeneficiaryWorkflow(t, registry, b)
	require.NoError(t, err)
	require.Equal(t, "ben_new", id)
	require.Equal(t, 1, registry.creates())
}

func TestBeneficiaryWorkflow_RejectionFailsAsRejected(t *testing.T) {
	registry := &fakeBeneficiaryRegistry{
		findFunc: notFound,
		createFunc: func(ctx context.Context, b domain.Beneficiary, callNum int) (string, error) {
			return "", fmt.Errorf("validation_failed: %w", app.ErrRejected)
		},
	}

	_, err := runBeneficiaryWorkflow(t, registry, testBeneficiary(t))
	require.Error(t, err)
	var appErr *temporal.ApplicationError
	require.True(t, errors.As(err, &appErr), "want an ApplicationError, got %v", err)
	require.Equal(t, ErrTypeRejected, appErr.Type())
	require.Equal(t, 1, registry.creates())
}

func TestBeneficiaryWorkflow_UnclearCreateFoundByLookupIsNeverRetried(t *testing.T) {
	// A plain, unclassified error is what a worker dying mid-call looks
	// like. With no request_id on the rail, Temporal must not retry the
	// create on its own - the lookup decides.
	registry := &fakeBeneficiaryRegistry{
		findFunc: func(ctx context.Context, key domain.BeneficiaryKey, callNum int) (string, bool, error) {
			if callNum == 1 {
				return "", false, nil
			}
			return "ben_landed", true, nil
		},
		createFunc: func(ctx context.Context, b domain.Beneficiary, callNum int) (string, error) {
			return "", errors.New("connection reset")
		},
	}

	id, err := runBeneficiaryWorkflow(t, registry, testBeneficiary(t))
	require.NoError(t, err)
	require.Equal(t, "ben_landed", id)
	require.Equal(t, 1, registry.creates())
}

func TestBeneficiaryWorkflow_UnclearCreateConfirmedAbsentIsRetried(t *testing.T) {
	registry := &fakeBeneficiaryRegistry{
		findFunc: notFound,
		createFunc: func(ctx context.Context, b domain.Beneficiary, callNum int) (string, error) {
			if callNum == 1 {
				return "", fmt.Errorf("timeout: %w", app.ErrAmbiguous)
			}
			return "ben_second_try", nil
		},
	}

	id, err := runBeneficiaryWorkflow(t, registry, testBeneficiary(t))
	require.NoError(t, err)
	require.Equal(t, "ben_second_try", id)
	require.Equal(t, 2, registry.creates())
}
