package app

import (
	"context"
	"errors"
	"fmt"
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

// RegisterBeneficiaryCommand is a payee to register for an
// organisation, as sent.
type RegisterBeneficiaryCommand struct {
	OrgID         domain.OrgID
	Name          string
	AccountNumber string
	RoutingNumber string
	Category      string
	Address       domain.BeneficiaryAddress
}

// RegisterBeneficiary is the register-beneficiary use case. It records
// the registering organisation and its treasury as the payee's owner:
// only that treasury may pay the payee.
type RegisterBeneficiary struct {
	Treasuries    TreasuryStore
	Registrar     BeneficiaryRegistrar
	Beneficiaries BeneficiaryStore
	Clock         Clock
	Audit         *Audit
}

func (r *RegisterBeneficiary) Run(ctx context.Context, cmd RegisterBeneficiaryCommand) (RegisterBeneficiaryResult, error) {
	treasury, err := r.Treasuries.TreasuryOf(ctx, cmd.OrgID)
	if err != nil {
		return RegisterBeneficiaryResult{}, fmt.Errorf("finding the organisation's treasury: %w", err)
	}
	b, err := domain.NewBeneficiary(cmd.OrgID, treasury.ID, cmd.Name, cmd.AccountNumber, cmd.RoutingNumber, domain.AccountCategory(cmd.Category), cmd.Address)
	if err != nil {
		return RegisterBeneficiaryResult{}, &ValidationError{Field: "beneficiary", Message: err.Error()}
	}
	// A payee registered before treasuries has its key, and rail
	// nickname, without one: registering it again finds it rather than
	// creating a second rail beneficiary under a new nickname.
	if have, err := r.Beneficiaries.ByKey(ctx, b.OrgID, b.TreasuryID, b.LegacyKey()); err == nil {
		return RegisterBeneficiaryResult{ID: have.RailID, Key: have.Key}, nil
	} else if !errors.Is(err, ErrNotFound) {
		return RegisterBeneficiaryResult{}, err
	}
	id, err := r.Registrar.Register(ctx, b)
	if err != nil {
		return RegisterBeneficiaryResult{}, err
	}
	if err := r.Beneficiaries.Save(ctx, RegisteredBeneficiary{
		OrgID:         b.OrgID,
		TreasuryID:    b.TreasuryID,
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
	r.Audit.record(ctx, b.OrgID, AuditPayeeRegistered, "payee", id, map[string]string{"name": b.Name, "account_last4": b.AccountNumber[len(b.AccountNumber)-4:]})
	return RegisterBeneficiaryResult{ID: id, Key: b.Key}, nil
}

// RegisteredBeneficiary records that an organisation registered a
// payee, and the rail's ID for it. Only the account number's last four
// digits are kept: enough to recognise it, not enough to misuse.
type RegisteredBeneficiary struct {
	OrgID         domain.OrgID
	TreasuryID    domain.TreasuryID
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
	// Owns reports whether org's treasury registered the rail
	// beneficiary railID.
	Owns(ctx context.Context, org domain.OrgID, treasury domain.TreasuryID, railID string) (bool, error)
	// ByKey is the payee org's treasury registered under key; ErrNotFound if none.
	ByKey(ctx context.Context, org domain.OrgID, treasury domain.TreasuryID, key domain.BeneficiaryKey) (RegisteredBeneficiary, error)
	// List returns org's payees, newest first.
	List(ctx context.Context, org domain.OrgID) ([]RegisteredBeneficiary, error)
}
