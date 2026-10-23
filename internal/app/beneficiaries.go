package app

import (
	"context"

	"enquoir/internal/domain"
)

// BeneficiaryRegistrar registers a beneficiary on the rail through the
// durable workflow, or finds the one already registered for its key,
// and returns the rail's ID for it. Returns an error wrapping
// ErrRejected when the rail refuses the details.
type BeneficiaryRegistrar interface {
	Register(ctx context.Context, b domain.Beneficiary) (id string, err error)
}

// RegisterBeneficiaryResult is a registered beneficiary: the rail's ID,
// which an Intent names as its payee, and Enquoir's key for it.
type RegisterBeneficiaryResult struct {
	ID  string
	Key domain.BeneficiaryKey
}

// RegisterBeneficiary is the register-beneficiary use case.
type RegisterBeneficiary struct {
	Registrar BeneficiaryRegistrar
}

func (r *RegisterBeneficiary) Run(ctx context.Context, b domain.Beneficiary) (RegisterBeneficiaryResult, error) {
	id, err := r.Registrar.Register(ctx, b)
	if err != nil {
		return RegisterBeneficiaryResult{}, err
	}
	return RegisterBeneficiaryResult{ID: id, Key: b.Key}, nil
}
