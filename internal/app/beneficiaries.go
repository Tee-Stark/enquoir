package app

import (
	"context"
	"time"

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

// RegisterBeneficiary is the register-beneficiary use case. It records
// the registering organisation as the payee's owner: only it may pay
// the payee.
type RegisterBeneficiary struct {
	Registrar     BeneficiaryRegistrar
	Beneficiaries BeneficiaryStore
	Clock         Clock
}

func (r *RegisterBeneficiary) Run(ctx context.Context, b domain.Beneficiary) (RegisterBeneficiaryResult, error) {
	id, err := r.Registrar.Register(ctx, b)
	if err != nil {
		return RegisterBeneficiaryResult{}, err
	}
	if err := r.Beneficiaries.Save(ctx, RegisteredBeneficiary{
		OrgID:         b.OrgID,
		Key:           b.Key,
		RailID:        id,
		Name:          b.Name,
		AccountLast4:  b.AccountNumber[len(b.AccountNumber)-4:],
		RoutingNumber: b.RoutingNumber,
		Category:      b.Category,
		CreatedAt:     r.Clock.Now(),
	}); err != nil {
		return RegisterBeneficiaryResult{}, err
	}
	return RegisterBeneficiaryResult{ID: id, Key: b.Key}, nil
}

// RegisteredBeneficiary records that an organisation registered a
// payee, and the rail's ID for it. Only the account number's last four
// digits are kept: enough to recognise it, not enough to misuse.
type RegisteredBeneficiary struct {
	OrgID         domain.OrgID
	Key           domain.BeneficiaryKey
	RailID        string
	Name          string
	AccountLast4  string
	RoutingNumber string
	Category      domain.AccountCategory
	CreatedAt     time.Time
}

// BeneficiaryStore records which organisation owns which payee. Every
// organisation pays through one rail account, so the rail itself
// cannot say; this is what stops one organisation paying another's.
type BeneficiaryStore interface {
	// Save records b. Saving the same beneficiary again is a no-op.
	// A rail ID already owned by another organisation is an error.
	Save(ctx context.Context, b RegisteredBeneficiary) error
	// Owns reports whether org registered the rail beneficiary railID.
	Owns(ctx context.Context, org domain.OrgID, railID string) (bool, error)
	// List returns org's payees, newest first.
	List(ctx context.Context, org domain.OrgID) ([]RegisteredBeneficiary, error)
}
