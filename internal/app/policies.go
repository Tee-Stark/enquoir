package app

import (
	"context"
	"fmt"

	"enquoir/internal/domain"
	"enquoir/internal/policy"
)

// GetCurrentPolicy is the get-current-policy use case: any member may
// read it (the permission check is the route's job).
type GetCurrentPolicy struct{ Policies policy.Store }

func (g *GetCurrentPolicy) Run(ctx context.Context, org domain.OrgID) (policy.PolicyVersion, error) {
	return g.Policies.CurrentPolicy(ctx, org)
}

// UpdatePolicyCommand is an edit: a rule per currency the caller wants
// to set (an omitted currency defaults to unlimited - the dense
// model, see policy.NewPolicyVersion), and the treasury payee
// allow/block lists and commerce category block list.
type UpdatePolicyCommand struct {
	CurrencyRules map[domain.Currency]policy.CurrencyRule
	PayeeAllow    []string
	PayeeBlock    []string
	CategoryBlock []string
}

// UpdatePolicy is the edit-the-policy use case: owners and admins
// only (the route's job to check). Every edit is a new version, never
// a mutation, and is audited.
type UpdatePolicy struct {
	Policies policy.Store
	Clock    Clock
	Audit    *Audit
}

func (u *UpdatePolicy) Run(ctx context.Context, org domain.OrgID, cmd UpdatePolicyCommand) (policy.PolicyVersion, error) {
	v, err := policy.NewPolicyVersion(org, 0, cmd.CurrencyRules, cmd.PayeeAllow, cmd.PayeeBlock, cmd.CategoryBlock)
	if err != nil {
		return policy.PolicyVersion{}, &ValidationError{Field: "currency_rules", Message: err.Error()}
	}
	v.CreatedAt = u.Clock.Now()
	v.CreatedBy = ActorFrom(ctx).UserID
	saved, err := u.Policies.SaveNewVersion(ctx, v)
	if err != nil {
		return policy.PolicyVersion{}, fmt.Errorf("saving the new policy version: %w", err)
	}
	u.Audit.record(ctx, org, AuditPolicyUpdated, "policy", fmt.Sprint(saved.Version), map[string]string{"version": fmt.Sprint(saved.Version)})
	return saved, nil
}

// DryRunPolicyCommand is a candidate payment to evaluate without
// persisting or reserving anything - sharing EvaluateAndReserve's
// input is safe for this because evaluation alone never reserves
// anything by itself (see policy.Store's doc comment); only the
// caller's own eventual persistence of a real payment does.
type DryRunPolicyCommand struct {
	Context     policy.Context
	Currency    domain.Currency
	AmountMinor int64
	Payee       *string
	Categories  []string
}

// DryRunPolicy evaluates a candidate payment against the
// organisation's current policy and usage, without moving or
// reserving anything. Owners and admins only (the route's job): it
// reveals how close the organisation is to its limits.
type DryRunPolicy struct{ Policies policy.Store }

func (d *DryRunPolicy) Run(ctx context.Context, org domain.OrgID, cmd DryRunPolicyCommand) (policy.Decision, error) {
	return d.Policies.EvaluateAndReserve(ctx, org, policy.EvaluationInput{
		Context: cmd.Context, Currency: cmd.Currency, AmountMinor: cmd.AmountMinor, Payee: cmd.Payee, Categories: cmd.Categories,
	})
}
