package sandbox

import (
	"context"
	"errors"
	"testing"

	"enquoir/internal/domain"
)

type fakeRegistry struct {
	existingID string
	createErr  error
	creates    int
}

func (r *fakeRegistry) CreateBeneficiary(ctx context.Context, b domain.Beneficiary) (string, error) {
	r.creates++
	if r.createErr != nil {
		return "", r.createErr
	}
	return "ben_created", nil
}

func (r *fakeRegistry) FindBeneficiary(ctx context.Context, key domain.BeneficiaryKey) (string, bool, error) {
	if key != TestPayee().Key {
		return "", false, errors.New("looked up the wrong key")
	}
	return r.existingID, r.existingID != "", nil
}

func TestTestPayee_IsAValidStableBeneficiary(t *testing.T) {
	if TestPayee().Key == "" || TestPayee().Key != TestPayee().Key {
		t.Fatalf("TestPayee key is empty or unstable: %q", TestPayee().Key)
	}
}

func TestEnsurePayee(t *testing.T) {
	createErr := errors.New("validation_failed")

	tests := []struct {
		name        string
		registry    *fakeRegistry
		wantID      string
		wantErr     error
		wantCreates int
	}{
		{"already registered is reused", &fakeRegistry{existingID: "ben_existing"}, "ben_existing", nil, 0},
		{"absent is created", &fakeRegistry{}, "ben_created", nil, 1},
		{"create failure is returned, not retried", &fakeRegistry{createErr: createErr}, "", createErr, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := EnsurePayee(context.Background(), tt.registry)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("EnsurePayee error = %v, want %v", err, tt.wantErr)
			}
			if id != tt.wantID {
				t.Errorf("id = %q, want %q", id, tt.wantID)
			}
			if tt.registry.creates != tt.wantCreates {
				t.Errorf("creates = %d, want %d", tt.registry.creates, tt.wantCreates)
			}
		})
	}
}
