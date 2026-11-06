// Package sandbox holds one-time sandbox bootstrapping shared by the
// setup, demo and chaos commands. Nothing the running system does
// depends on it.
package sandbox

import (
	"context"
	"fmt"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

// TestPayee is Enquoir's single sandbox test beneficiary: a US company
// paid in USD over ACH. The details are fixed, so its key - and so the
// beneficiary it registers as - stays the same across every run.
func TestPayee() domain.Beneficiary {
	b, err := domain.NewBeneficiary("Enquoir Sandbox Test Payee", "000123456789", "021000021", domain.Checking, domain.BeneficiaryAddress{
		Street:   "412 5th Avenue",
		City:     "Seattle",
		State:    "WA",
		Postcode: "98104",
	})
	if err != nil {
		panic(fmt.Sprintf("sandbox: test payee is invalid: %v", err))
	}
	return b
}

// EnsurePayee returns the test payee's rail ID, registering it only
// when a lookup by its key confirms it is absent. Unlike
// BeneficiaryWorkflow this is not durable: a failed create is returned
// rather than retried, and re-running finds the payee if the create
// actually landed. Fine for setup tooling, never for the running
// system.
func EnsurePayee(ctx context.Context, registry app.BeneficiaryRegistry) (string, error) {
	payee := TestPayee()

	id, ok, err := registry.FindBeneficiary(ctx, payee.Key)
	if err != nil {
		return "", fmt.Errorf("looking up test payee: %w", err)
	}
	if ok {
		return id, nil
	}

	id, err = registry.CreateBeneficiary(ctx, payee)
	if err != nil {
		return "", fmt.Errorf("creating test payee: %w", err)
	}
	return id, nil
}
