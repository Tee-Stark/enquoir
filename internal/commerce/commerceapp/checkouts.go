package commerceapp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/domain"
	"enquoir/internal/policy"
)

var (
	// ErrMandateUnknown means the mandate named is not the merchant's.
	ErrMandateUnknown = errors.New("the mandate is not one of this merchant's")

	// ErrCheckoutMismatch means an approval named a checkout other than
	// the one awaiting approval: approval binds to exactly what was shown.
	ErrCheckoutMismatch = errors.New("this is not the checkout awaiting your approval; reload the page and review it again")
)

// CheckoutOutcome is a decision on a checkout.
type CheckoutOutcome struct {
	Status            commerce.CheckoutStatus
	Denials           []commerce.Denial
	ApprovalExpiresAt *time.Time
	// Authorization is set when the checkout is authorized; the store
	// saves it with the decision.
	Authorization *commerce.Authorization
}

// CheckoutDecider decides a checkout given its mandate - with the
// statuses of what it names - and what the mandate has already
// authorized. Stores call it while holding the mandate's lock.
type CheckoutDecider func(m MandateRow, u commerce.MandateUsage) CheckoutOutcome

// CheckoutStore persists checkouts. Decisions that count against a
// mandate happen under a lock on it, so concurrent checkouts under one
// mandate are decided one at a time and can't overspend it.
type CheckoutStore interface {
	// SubmitCheckout returns the merchant's checkout with c's external
	// ID if there is one (created false). Otherwise it locks c's mandate
	// (app.ErrNotFound if it isn't in s), decides c with decide, and
	// stores it, in one transaction.
	SubmitCheckout(ctx context.Context, s Scope, c commerce.Checkout, decide CheckoutDecider) (stored commerce.Checkout, created bool, err error)
	GetCheckout(ctx context.Context, s Scope, id commerce.CheckoutID) (commerce.Checkout, error)
	// CheckoutForApproval reads a checkout without a merchant scope: for
	// the approval page, where the customer's link is the authority.
	CheckoutForApproval(ctx context.Context, id commerce.CheckoutID) (commerce.Checkout, error)
	// DecideCheckout spends the approval link, locks the mandate, decides
	// the checkout with decide and stores the decision, atomically.
	// app.ErrTokenInvalid unless the token is an unspent approval link
	// for it and it is awaiting approval at at.
	DecideCheckout(ctx context.Context, tokenHash string, id commerce.CheckoutID, at time.Time, decide CheckoutDecider) (commerce.Checkout, error)
	// CancelCheckout cancels an authorized checkout whose grant is
	// unredeemed and unexpired, revoking it, or an awaiting one; changed
	// is false if it was already cancelled; commerce.ErrCheckoutNotCancelable
	// otherwise.
	CancelCheckout(ctx context.Context, s Scope, id commerce.CheckoutID, at time.Time) (commerce.Checkout, bool, error)

	// IssueGrant sets the checkout's grant to grantHash, replacing any
	// earlier one, under the mandate's lock. app.ErrNotFound for another
	// merchant's checkout; commerce.ErrGrantRedeemed once redeemed;
	// ErrGrantNotIssuable unless its authorization is active at at.
	IssueGrant(ctx context.Context, s Scope, id commerce.CheckoutID, grantHash string, at time.Time) (commerce.Checkout, error)
	// RedeemGrant finds s's authorization whose current grant hashes to
	// grantHash (ErrGrantInvalid if none) and, under the mandate's lock,
	// returns its execution if it was redeemed (created false); otherwise
	// calls redeem and, unless it fails, stores the execution, marks the
	// authorization and checkout redeemed - atomically, so a grant makes
	// at most one execution.
	RedeemGrant(ctx context.Context, s Scope, grantHash string, at time.Time, redeem GrantRedeemer) (commerce.Execution, bool, error)
	GetExecution(ctx context.Context, s Scope, id commerce.ExecutionID) (commerce.Execution, error)

	ExecutionStore
	MandateUsageReader
}

// GrantRedeemer decides a redemption, given the authorization, its
// mandate and the funding method, all read under the mandate's lock.
type GrantRedeemer func(a commerce.Authorization, m MandateRow, f commerce.FundingMethod) (commerce.Execution, error)

// CheckoutCommand is a checkout the merchant's backend submits.
type CheckoutCommand struct {
	ExternalID      string
	MandateID       commerce.MandateID
	CustomerID      commerce.CustomerID
	AgentID         commerce.AgentID
	FundingMethodID commerce.FundingMethodID
	Currency        string
	Items           []commerce.CheckoutItem
	ShippingMinor   int64
	TaxMinor        int64
	FeesMinor       int64
	TotalMinor      int64
}

// CheckoutView is a checkout with its status now and, only in the
// response that issued it, its grant's secret.
type CheckoutView struct {
	commerce.Checkout
	Grant string
	// AuthorizationStatus is the authorization's status now, if any.
	AuthorizationStatus commerce.AuthorizationStatus
}

func checkoutView(c commerce.Checkout, now time.Time) CheckoutView {
	v := CheckoutView{Checkout: c}
	v.Status = c.StatusAt(now)
	if c.Authorization != nil {
		v.AuthorizationStatus = c.Authorization.StatusAt(now)
	}
	return v
}

const grantPrefix = "enq_gr"

// mintGrant mints a grant secret for a; only its hash is kept.
func mintGrant(secrets app.SecretGenerator, a *commerce.Authorization, now time.Time) string {
	secret := secrets.NewSecret(grantPrefix)
	a.GrantHash, a.GrantIssuedAt = app.HashSecret(secret), &now
	return secret
}

const auditTargetCheckout = "commerce_checkout"

func checkoutDetails(c commerce.Checkout) map[string]string {
	s := c.Snapshot
	d := map[string]string{
		"merchant_id": string(s.MerchantID), "mandate_id": string(s.MandateID), "external_id": string(s.ExternalID),
		"total_minor": fmt.Sprint(s.TotalMinor), "currency": string(s.Currency), "checkout_hash": s.Hash(),
	}
	if len(c.Denials) > 0 {
		var codes []string
		for _, x := range c.Denials {
			codes = append(codes, string(x.Code))
		}
		d["reasons"] = strings.Join(codes, ",")
	}
	return d
}

// evaluate is the one decision rule, for a submission and a customer's
// approval alike: any reason denies; otherwise awaitApproval, or
// authorized - with its authorization, made by authorize.
func evaluate(c commerce.Checkout, now time.Time, awaitApproval bool, authorize func(commerce.Checkout) commerce.Authorization) CheckoutDecider {
	return func(m MandateRow, u commerce.MandateUsage) CheckoutOutcome {
		if d := commerce.EvaluateCheckout(m.Mandate, m.StatusAt(now, m.Deps), c.Snapshot, u); len(d) > 0 {
			return CheckoutOutcome{Status: commerce.CheckoutDenied, Denials: d}
		}
		if awaitApproval && m.Terms.ApprovalMode == commerce.PerTransaction {
			exp := now.Add(commerce.CheckoutApprovalTTL)
			return CheckoutOutcome{Status: commerce.CheckoutAwaitingApproval, ApprovalExpiresAt: &exp}
		}
		a := authorize(c)
		return CheckoutOutcome{Status: commerce.CheckoutAuthorized, Authorization: &a}
	}
}

// SubmitCheckout evaluates a checkout the merchant's backend submits
// under one of its mandates. Within an autonomous mandate it is
// authorized at once; within a per-transaction mandate the customer is
// emailed a link to approve it. Outside it, it is denied with every
// reason. Either way the decision is recorded.
type SubmitCheckout struct {
	Checkouts CheckoutStore
	Customers CustomerStore
	Emails    *app.Emails
	IDs       app.IDGenerator
	Secrets   app.SecretGenerator
	Clock     app.Clock
	Audit     *app.Audit
	// Policies evaluates a candidate checkout against the merchant's
	// organisation's spending policy, org-wide across both treasury
	// payments and commerce checkouts. Nil skips the check and always
	// allows.
	Policies policy.Store
}

// policyDenials turns a policy decision's reasons into Denials, each
// code prefixed "policy_" so it can never collide with a mandate-level
// DenialCode of the same underlying name (e.g. both have an
// "over_transaction_limit" reason, for two different authorities) -
// not that the two ever appear in the same response, since a
// policy-denied checkout never reaches mandate evaluation, but a
// reader of one stored denial should never have to guess which
// authority it came from.
func policyDenials(d policy.Decision) []commerce.Denial {
	out := make([]commerce.Denial, 0, len(d.Reasons))
	for _, r := range d.Reasons {
		out = append(out, commerce.Denial{Code: commerce.DenialCode("policy_" + string(r.Code)), Detail: r.Detail})
	}
	return out
}

// checkoutCategories is every distinct item category in snap, in
// first-seen order, for the organisation's category-block policy rule.
func checkoutCategories(items []commerce.CheckoutItem) []string {
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		if !seen[it.Category] {
			seen[it.Category] = true
			out = append(out, it.Category)
		}
	}
	return out
}

func (s *SubmitCheckout) Run(ctx context.Context, p MerchantPrincipal, cmd CheckoutCommand) (CheckoutView, bool, error) {
	now := s.Clock.Now()
	if strings.TrimSpace(string(cmd.MandateID)) == "" {
		return CheckoutView{}, false, &app.ValidationError{Field: "mandate_id", Message: "mandate_id is required"}
	}
	for field, v := range map[string]string{"customer_id": string(cmd.CustomerID), "agent_id": string(cmd.AgentID), "funding_method_id": string(cmd.FundingMethodID)} {
		if err := required(field, v); err != nil {
			return CheckoutView{}, false, err
		}
	}
	snap, err := commerce.NewCheckoutSnapshot(commerce.CheckoutSnapshot{
		OrgID: p.OrgID, MerchantID: p.MerchantID, MandateID: cmd.MandateID, ExternalID: commerce.ExternalID(cmd.ExternalID),
		CustomerID: cmd.CustomerID, AgentID: cmd.AgentID, FundingMethodID: cmd.FundingMethodID,
		Currency: domain.Currency(cmd.Currency), Items: cmd.Items,
		ShippingMinor: cmd.ShippingMinor, TaxMinor: cmd.TaxMinor, FeesMinor: cmd.FeesMinor, TotalMinor: cmd.TotalMinor,
	})
	if err != nil {
		return CheckoutView{}, false, fieldErr(err)
	}

	// The organisation's policy is a prior, separate gate: decided
	// before the mandate is ever evaluated or locked, so its reasons -
	// which a customer must never see - never mix with the mandate's
	// own, which sometimes are shown to the customer on the approval
	// page. Escalate is shimmed to behave exactly like Deny: commerce
	// has no awaiting_review state or escalation workflow of its own
	// (an accepted scope cut for Phase 6, see docs/progress.md) - this
	// is the standing behaviour, not a placeholder for a later phase.
	var policyDenied []commerce.Denial
	if s.Policies != nil {
		decision, err := s.Policies.EvaluateAndReserve(ctx, p.OrgID, policy.EvaluationInput{
			Context: policy.ContextCommerce, Currency: snap.Currency, AmountMinor: snap.TotalMinor,
			Categories: checkoutCategories(snap.Items),
		})
		if err != nil {
			return CheckoutView{}, false, fmt.Errorf("evaluating the organisation's spending policy: %w", err)
		}
		if decision.Outcome != policy.Allow {
			policyDenied = policyDenials(decision)
		}
	}

	c := commerce.Checkout{ID: commerce.CheckoutID(s.IDs.NewID("chk")), Snapshot: snap, CreatedAt: now}
	var terms commerce.MandateTerms
	grant := ""
	rule := evaluate(c, now, true, func(c commerce.Checkout) commerce.Authorization {
		a := commerce.NewAuthorization(commerce.AuthorizationID(s.IDs.NewID("auth")), c, now)
		grant = mintGrant(s.Secrets, &a, now)
		return a
	})
	if policyDenied != nil {
		rule = func(m MandateRow, u commerce.MandateUsage) CheckoutOutcome {
			return CheckoutOutcome{Status: commerce.CheckoutDenied, Denials: policyDenied}
		}
	}
	stored, created, err := s.Checkouts.SubmitCheckout(ctx, p.Scope(), c, func(m MandateRow, u commerce.MandateUsage) CheckoutOutcome {
		terms = m.Terms
		return rule(m, u)
	})
	if errors.Is(err, app.ErrNotFound) {
		return CheckoutView{}, false, ErrMandateUnknown
	}
	if err != nil {
		return CheckoutView{}, false, fmt.Errorf("submitting the checkout: %w", err)
	}
	if !created {
		if stored.Snapshot.Hash() != snap.Hash() {
			return CheckoutView{}, false, ErrRegistrationConflict
		}
		return checkoutView(stored, now), false, nil
	}

	action := map[commerce.CheckoutStatus]string{
		commerce.CheckoutAuthorized: app.AuditCheckoutAuthorized, commerce.CheckoutDenied: app.AuditCheckoutDenied,
		commerce.CheckoutAwaitingApproval: app.AuditCheckoutAwaitingApproval,
	}[stored.Status]
	if stored.Status == commerce.CheckoutAwaitingApproval {
		cust, err := s.Customers.GetCustomer(ctx, p.Scope(), snap.CustomerID)
		if err != nil {
			return CheckoutView{}, false, err
		}
		if _, err := s.Emails.Send(ctx, app.Email{
			OrgID: p.OrgID, To: cust.Email, Template: app.TemplateCheckoutApproval, Params: describeCheckout(snap, terms),
			Link: &app.LinkSpec{Purpose: app.PurposeCheckoutApproval, Subject: string(stored.ID), TTL: commerce.CheckoutApprovalTTL},
		}); err != nil {
			return CheckoutView{}, false, fmt.Errorf("queueing the approval email: %w", err)
		}
	}
	s.Audit.Record(ctx, p.OrgID, action, auditTargetCheckout, string(stored.ID), checkoutDetails(stored))
	v := checkoutView(stored, now)
	if stored.Status == commerce.CheckoutAuthorized {
		v.Grant = grant
	}
	return v, true, nil
}

// describeCheckout is a checkout as the customer reads it in an email.
func describeCheckout(s commerce.CheckoutSnapshot, t commerce.MandateTerms) map[string]string {
	var lines []string
	for _, it := range s.Items {
		lines = append(lines, fmt.Sprintf("%d x %s, %s each", it.Quantity, it.Name, money(it.UnitPriceMinor, s.Currency)))
	}
	if s.ShippingMinor > 0 {
		lines = append(lines, "Shipping, "+money(s.ShippingMinor, s.Currency))
	}
	if s.TaxMinor > 0 {
		lines = append(lines, "Tax, "+money(s.TaxMinor, s.Currency))
	}
	if s.FeesMinor > 0 {
		lines = append(lines, "Fees, "+money(s.FeesMinor, s.Currency))
	}
	return map[string]string{
		"merchant": t.Parties.MerchantName, "agent": t.Parties.AgentName, "payment_method": paymentMethod(t.Parties.FundingDisplay),
		"items": strings.Join(lines, "; "), "total": money(s.TotalMinor, s.Currency),
	}
}

// GetCheckout fetches one of the merchant's checkouts.
type GetCheckout struct {
	Checkouts CheckoutStore
	Clock     app.Clock
}

func (g *GetCheckout) Run(ctx context.Context, p MerchantPrincipal, id commerce.CheckoutID) (CheckoutView, error) {
	c, err := g.Checkouts.GetCheckout(ctx, p.Scope(), id)
	if err != nil {
		return CheckoutView{}, err
	}
	return checkoutView(c, g.Clock.Now()), nil
}

// CancelCheckout cancels an authorized checkout, releasing what it
// counted against its mandate, or one awaiting the customer.
type CancelCheckout struct {
	Checkouts CheckoutStore
	Clock     app.Clock
	Audit     *app.Audit
}

func (c *CancelCheckout) Run(ctx context.Context, p MerchantPrincipal, id commerce.CheckoutID) (CheckoutView, error) {
	now := c.Clock.Now()
	ck, changed, err := c.Checkouts.CancelCheckout(ctx, p.Scope(), id, now)
	if err != nil {
		return CheckoutView{}, err
	}
	if changed {
		c.Audit.Record(ctx, p.OrgID, app.AuditCheckoutCanceled, auditTargetCheckout, string(id), checkoutDetails(ck))
	}
	return checkoutView(ck, now), nil
}

// approvalCheckout resolves an approval link to its awaiting checkout
// without spending it. Any problem is app.ErrTokenInvalid.
func approvalCheckout(ctx context.Context, tokens app.TokenStore, checkouts CheckoutStore, token string, now time.Time) (commerce.Checkout, string, error) {
	if token == "" {
		return commerce.Checkout{}, "", app.ErrTokenInvalid
	}
	hash := app.HashSecret(token)
	t, err := tokens.Peek(ctx, hash, app.PurposeCheckoutApproval, now)
	if err != nil {
		return commerce.Checkout{}, "", app.ErrTokenInvalid
	}
	c, err := checkouts.CheckoutForApproval(ctx, commerce.CheckoutID(t.Subject))
	if err != nil || c.StatusAt(now) != commerce.CheckoutAwaitingApproval {
		return commerce.Checkout{}, "", app.ErrTokenInvalid
	}
	return c, hash, nil
}

// ViewCheckoutApproval is the approval page reading the checkout by its
// link. It does not spend the link.
type ViewCheckoutApproval struct {
	Tokens    app.TokenStore
	Checkouts CheckoutStore
	Clock     app.Clock
}

func (v *ViewCheckoutApproval) Run(ctx context.Context, token string) (CheckoutView, error) {
	now := v.Clock.Now()
	c, _, err := approvalCheckout(ctx, v.Tokens, v.Checkouts, token, now)
	if err != nil {
		return CheckoutView{}, err
	}
	return checkoutView(c, now), nil
}

// DecideCheckoutApproval is the customer approving or rejecting a
// checkout from the link Enquoir emailed them. Approval names the hash
// of the checkout the page showed, and the mandate's limits are checked
// again: a checkout that no longer fits is denied.
type DecideCheckoutApproval struct {
	Tokens    app.TokenStore
	Checkouts CheckoutStore
	IDs       app.IDGenerator
	Clock     app.Clock
	Audit     *app.Audit
}

func (d *DecideCheckoutApproval) Run(ctx context.Context, token, checkoutHash string, approve bool) (CheckoutView, error) {
	now := d.Clock.Now()
	c, hash, err := approvalCheckout(ctx, d.Tokens, d.Checkouts, token, now)
	if err != nil {
		return CheckoutView{}, err
	}
	if approve && checkoutHash != c.Snapshot.Hash() {
		return CheckoutView{}, ErrCheckoutMismatch
	}
	// The authorization has no grant yet: the customer never sees one;
	// the merchant has one issued (IssueGrant).
	rule := evaluate(c, now, false, func(c commerce.Checkout) commerce.Authorization {
		return commerce.NewAuthorization(commerce.AuthorizationID(d.IDs.NewID("auth")), c, now)
	})
	decided, err := d.Checkouts.DecideCheckout(ctx, hash, c.ID, now, func(m MandateRow, u commerce.MandateUsage) CheckoutOutcome {
		if !approve {
			return CheckoutOutcome{Status: commerce.CheckoutRejected}
		}
		return rule(m, u)
	})
	if err != nil {
		return CheckoutView{}, err
	}
	action := map[commerce.CheckoutStatus]string{
		commerce.CheckoutAuthorized: app.AuditCheckoutApproved, commerce.CheckoutDenied: app.AuditCheckoutDenied,
		commerce.CheckoutRejected: app.AuditCheckoutRejected,
	}[decided.Status]
	details := checkoutDetails(decided)
	details["decided_by"] = "customer"
	d.Audit.Record(ctx, decided.Snapshot.OrgID, action, auditTargetCheckout, string(decided.ID), details)
	return checkoutView(decided, now), nil
}
