package commerceapp

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/domain"
)

var (
	ErrAgentUnknown          = errors.New("the agent is not one this merchant registered")
	ErrAgentDisabled         = errors.New("the agent is disabled")
	ErrFundingMethodUnknown  = errors.New("the funding method is not one this merchant registered for this customer")
	ErrFundingMethodDisabled = errors.New("the funding method is disabled")

	// ErrTermsMismatch means an approval named terms other than the
	// request's: consent binds only to exactly what was shown.
	ErrTermsMismatch = errors.New("these are not the terms of this request; reload the page and review them again")
)

// MandateRequestCommand asks a customer for a mandate.
type MandateRequestCommand struct {
	ExternalID             string
	CustomerID             commerce.CustomerID
	AgentID                commerce.AgentID
	FundingMethodID        commerce.FundingMethodID
	ApprovalMode           string
	Currency               string
	MaxPerTransactionMinor int64
	MaxTotalMinor          int64
	MaxTransactions        int
	AllowedCategories      []string
	ExpiresAt              time.Time
}

// MandateRow is a stored mandate with the statuses of what it names.
type MandateRow struct {
	commerce.Mandate
	Deps commerce.Dependencies
}

// MandateView is a mandate with its effective status.
type MandateView struct {
	commerce.Mandate
	Status commerce.MandateStatus
}

// MandateFilter narrows a list of mandates. Zero fields don't filter.
type MandateFilter struct {
	CustomerID commerce.CustomerID
	Status     commerce.MandateStatus
	Limit      int
	Cursor     string
}

type MandatePage struct {
	Mandates   []MandateView
	NextCursor string
}

// ConsentDecisionCommand is everything one decision changes, applied
// atomically: the link token is spent, the request decided, the consent
// record written and, on approval, the mandate created.
type ConsentDecisionCommand struct {
	TokenHash string
	Record    commerce.ConsentRecord
	Mandate   *commerce.Mandate // nil for a rejection
	At        time.Time
}

// MandateStore persists mandate requests, mandates and consent records.
// Requests and mandates are scoped like the merchant's other records.
type MandateStore interface {
	// CreateRequest stores r unless its merchant already has a request
	// with r's external ID; either way it returns the stored one.
	CreateRequest(ctx context.Context, r commerce.MandateRequest) (stored commerce.MandateRequest, created bool, err error)
	GetRequest(ctx context.Context, s Scope, id commerce.MandateRequestID) (commerce.MandateRequest, error)
	// RequestForConsent reads a request without a merchant scope: for
	// the consent page, where the customer's link is the authority.
	RequestForConsent(ctx context.Context, id commerce.MandateRequestID) (commerce.MandateRequest, error)
	// Decide applies cmd atomically. app.ErrTokenInvalid if the token is
	// not an unspent, unexpired consent token for the request, or the
	// request is no longer pending at cmd.At.
	Decide(ctx context.Context, cmd ConsentDecisionCommand) error
	GetMandate(ctx context.Context, s Scope, id commerce.MandateID) (MandateRow, error)
	// ListMandates lists newest first; a status filter is the effective
	// status at now.
	ListMandates(ctx context.Context, s Scope, f MandateFilter, now time.Time) ([]MandateRow, string, error)
	// RequestRevocation marks the mandate revocation_pending from at
	// until expires, unless it is revoked or a revocation is already
	// pending at at; changed reports whether it did.
	RequestRevocation(ctx context.Context, s Scope, id commerce.MandateID, at, expires time.Time) (changed bool, err error)
	// MandateForRevocation reads a mandate without a merchant scope: for
	// the confirmation page, where the customer's link is the authority.
	MandateForRevocation(ctx context.Context, id commerce.MandateID) (MandateRow, error)
	// ConfirmRevocation spends the revocation link and revokes the
	// mandate, atomically. app.ErrTokenInvalid unless the token is an
	// unspent, unexpired revocation link for it and its revocation is
	// pending at at.
	ConfirmRevocation(ctx context.Context, tokenHash string, id commerce.MandateID, at time.Time) error
}

const (
	auditTargetMandateRequest = "commerce_mandate_request"
	auditTargetMandate        = "commerce_mandate"
)

// RequestMandate asks one of the merchant's customers to grant one of
// its agents spending authority. It freezes the terms and emails the
// customer a one-time consent link; the merchant never sees the link,
// and nothing it sends can approve.
type RequestMandate struct {
	Mandates  MandateStore
	Customers CustomerStore
	Agents    AgentStore
	Methods   FundingMethodStore
	Emails    *app.Emails
	IDs       app.IDGenerator
	Clock     app.Clock
	Audit     *app.Audit
}

func required(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return &app.ValidationError{Field: field, Message: field + " is required"}
	}
	return nil
}

func (r *RequestMandate) Run(ctx context.Context, p MerchantPrincipal, cmd MandateRequestCommand) (commerce.MandateRequest, bool, error) {
	now := r.Clock.Now()
	ext, err := commerce.NewExternalID(cmd.ExternalID)
	if err != nil {
		return commerce.MandateRequest{}, false, invalid("external_id", err)
	}
	for field, v := range map[string]string{"customer_id": string(cmd.CustomerID), "agent_id": string(cmd.AgentID), "funding_method_id": string(cmd.FundingMethodID)} {
		if err := required(field, v); err != nil {
			return commerce.MandateRequest{}, false, err
		}
	}
	mode, err := commerce.ParseApprovalMode(cmd.ApprovalMode)
	if err != nil {
		return commerce.MandateRequest{}, false, invalid("approval_mode", err)
	}
	scope, err := commerce.NewMandateScope(domain.Currency(cmd.Currency), cmd.MaxPerTransactionMinor, cmd.MaxTotalMinor, cmd.MaxTransactions, cmd.AllowedCategories)
	if err != nil {
		return commerce.MandateRequest{}, false, fieldErr(err)
	}
	if _, err := commerce.NewMandateTerms(commerce.MandateParties{}, mode, scope, cmd.ExpiresAt, now); err != nil {
		return commerce.MandateRequest{}, false, fieldErr(err)
	}

	customer, agent, method, err := r.parties(ctx, p, cmd)
	if err != nil {
		return commerce.MandateRequest{}, false, err
	}
	terms, _ := commerce.NewMandateTerms(commerce.MandateParties{
		OrgID: p.OrgID, MerchantID: p.MerchantID, MerchantName: p.MerchantName,
		CustomerID: customer.ID, AgentID: agent.ID, AgentName: agent.Name,
		FundingMethodID: method.ID, FundingDisplay: method.Display,
	}, mode, scope, cmd.ExpiresAt, now)

	req := commerce.MandateRequest{
		ID: commerce.MandateRequestID(r.IDs.NewID("mreq")), ExternalID: ext, Terms: terms,
		Status: commerce.RequestPending, CreatedAt: now, ExpiresAt: now.Add(commerce.ConsentLinkTTL),
	}
	stored, created, err := r.Mandates.CreateRequest(ctx, req)
	if err != nil {
		return commerce.MandateRequest{}, false, fmt.Errorf("storing the mandate request: %w", err)
	}
	if !created {
		if stored.Terms.Hash() != terms.Hash() {
			return commerce.MandateRequest{}, false, ErrRegistrationConflict
		}
		stored.Status = stored.StatusAt(now)
		return stored, false, nil
	}
	if _, err := r.Emails.Send(ctx, app.Email{
		OrgID: p.OrgID, To: customer.Email, Template: app.TemplateMandateConsent, Params: describeTerms(terms),
		Link: &app.LinkSpec{Purpose: app.PurposeMandateConsent, Subject: string(stored.ID), TTL: commerce.ConsentLinkTTL},
	}); err != nil {
		return commerce.MandateRequest{}, false, fmt.Errorf("queueing the consent email: %w", err)
	}
	r.Audit.Record(ctx, p.OrgID, app.AuditMandateRequested, auditTargetMandateRequest, string(stored.ID), map[string]string{
		"merchant_id": string(p.MerchantID), "customer_id": string(customer.ID), "agent_id": string(agent.ID),
		"funding_method_id": string(method.ID), "terms_hash": terms.Hash(),
	})
	return stored, true, nil
}

// parties loads and checks the customer, agent and funding method:
// each the merchant's and active, and the method the customer's.
func (r *RequestMandate) parties(ctx context.Context, p MerchantPrincipal, cmd MandateRequestCommand) (commerce.Customer, commerce.Agent, commerce.FundingMethod, error) {
	none := func(err error) (commerce.Customer, commerce.Agent, commerce.FundingMethod, error) {
		return commerce.Customer{}, commerce.Agent{}, commerce.FundingMethod{}, err
	}
	c, err := r.Customers.GetCustomer(ctx, p.Scope(), cmd.CustomerID)
	if errors.Is(err, app.ErrNotFound) {
		return none(ErrCustomerUnknown)
	} else if err != nil {
		return none(err)
	}
	a, err := r.Agents.GetAgent(ctx, p.Scope(), cmd.AgentID)
	if errors.Is(err, app.ErrNotFound) {
		return none(ErrAgentUnknown)
	} else if err != nil {
		return none(err)
	}
	f, err := r.Methods.GetFundingMethod(ctx, p.Scope(), cmd.FundingMethodID)
	if errors.Is(err, app.ErrNotFound) || (err == nil && f.CustomerID != c.ID) {
		return none(ErrFundingMethodUnknown)
	} else if err != nil {
		return none(err)
	}
	switch {
	case !c.Status.IsActive():
		return none(ErrCustomerDisabled)
	case !a.Status.IsActive():
		return none(ErrAgentDisabled)
	case !f.Status.IsActive():
		return none(ErrFundingMethodDisabled)
	}
	return c, a, f, nil
}

func fieldErr(err error) error {
	var ferr *commerce.FieldError
	if errors.As(err, &ferr) {
		return &app.ValidationError{Field: ferr.Field, Message: ferr.Err.Error()}
	}
	return err
}

func money(minor int64, c domain.Currency) string {
	m, err := domain.NewMoney(minor, c)
	if err != nil {
		return fmt.Sprintf("%s %d minor units", c, minor)
	}
	return string(c) + " " + m.MajorUnitsString()
}

func paymentMethod(d commerce.FundingDisplay) string {
	if d.LastFour == "" {
		return d.Brand
	}
	return d.Brand + " ending " + d.LastFour
}

// describeTerms is the terms as the customer reads them in an email.
func describeTerms(t commerce.MandateTerms) map[string]string {
	approval := "without asking you each time"
	if t.ApprovalMode == commerce.PerTransaction {
		approval = "asking you to approve each purchase"
	}
	count := "no limit"
	if t.Scope.MaxTransactions > 0 {
		count = strconv.Itoa(t.Scope.MaxTransactions)
	}
	categories := "any"
	if len(t.Scope.AllowedCategories) > 0 {
		categories = strings.Join(t.Scope.AllowedCategories, ", ")
	}
	return map[string]string{
		"merchant": t.Parties.MerchantName, "agent": t.Parties.AgentName, "payment_method": paymentMethod(t.Parties.FundingDisplay),
		"approval": approval, "max_per_transaction": money(t.Scope.MaxPerTransactionMinor, t.Scope.Currency),
		"max_total": money(t.Scope.MaxTotalMinor, t.Scope.Currency), "max_transactions": count,
		"categories": categories, "expires": t.ExpiresAt.UTC().Format("2 Jan 2006 15:04 UTC"),
	}
}

// GetMandateRequest fetches one of the merchant's requests, with its
// status now.
type GetMandateRequest struct {
	Mandates MandateStore
	Clock    app.Clock
}

func (g *GetMandateRequest) Run(ctx context.Context, p MerchantPrincipal, id commerce.MandateRequestID) (commerce.MandateRequest, error) {
	r, err := g.Mandates.GetRequest(ctx, p.Scope(), id)
	if err != nil {
		return commerce.MandateRequest{}, err
	}
	r.Status = r.StatusAt(g.Clock.Now())
	return r, nil
}

// consentRequest resolves a consent link to its pending request without
// spending the link. Any problem - unknown, spent, expired, decided - is
// app.ErrTokenInvalid: the page needs no more.
func consentRequest(ctx context.Context, tokens app.TokenStore, mandates MandateStore, token string, now time.Time) (commerce.MandateRequest, string, error) {
	if token == "" {
		return commerce.MandateRequest{}, "", app.ErrTokenInvalid
	}
	hash := app.HashSecret(token)
	t, err := tokens.Peek(ctx, hash, app.PurposeMandateConsent, now)
	if err != nil {
		return commerce.MandateRequest{}, "", app.ErrTokenInvalid
	}
	r, err := mandates.RequestForConsent(ctx, commerce.MandateRequestID(t.Subject))
	if err != nil || r.StatusAt(now) != commerce.RequestPending || !now.Before(r.Terms.ExpiresAt) {
		return commerce.MandateRequest{}, "", app.ErrTokenInvalid
	}
	return r, hash, nil
}

// ViewConsent is the consent page reading the terms by its link. It
// does not spend the link.
type ViewConsent struct {
	Tokens   app.TokenStore
	Mandates MandateStore
	Clock    app.Clock
}

func (v *ViewConsent) Run(ctx context.Context, token string) (commerce.MandateRequest, error) {
	r, _, err := consentRequest(ctx, v.Tokens, v.Mandates, token, v.Clock.Now())
	return r, err
}

// ConsentOutcome is a decided request and, if approved, its mandate.
type ConsentOutcome struct {
	Request commerce.MandateRequest
	Mandate *commerce.Mandate
}

// DecideConsent is the customer approving or rejecting, with the link
// Enquoir emailed them. Approval names the hash of the terms the page
// showed and must match; it creates the mandate and the consent record
// together, and spends the link.
type DecideConsent struct {
	Tokens   app.TokenStore
	Mandates MandateStore
	IDs      app.IDGenerator
	Clock    app.Clock
	Audit    *app.Audit
}

func (d *DecideConsent) Run(ctx context.Context, token, termsHash string, decision commerce.ConsentDecision) (ConsentOutcome, error) {
	if decision != commerce.ConsentApproved && decision != commerce.ConsentRejected {
		return ConsentOutcome{}, &app.ValidationError{Field: "decision", Message: "decision must be approved or rejected"}
	}
	now := d.Clock.Now()
	r, hash, err := consentRequest(ctx, d.Tokens, d.Mandates, token, now)
	if err != nil {
		return ConsentOutcome{}, err
	}
	if decision == commerce.ConsentApproved && termsHash != r.Terms.Hash() {
		return ConsentOutcome{}, ErrTermsMismatch
	}
	actor := app.ActorFrom(ctx)
	p := r.Terms.Parties
	rec := commerce.ConsentRecord{
		ID: commerce.ConsentRecordID(d.IDs.NewID("cns")), RequestID: r.ID, OrgID: p.OrgID, MerchantID: p.MerchantID,
		CustomerID: p.CustomerID, Decision: decision, Snapshot: r.Terms.Canonical(), TermsHash: r.Terms.Hash(),
		Method: commerce.ConsentMethodEmailLink, IP: actor.IP, UserAgent: actor.UserAgent, At: now,
	}
	cmd := ConsentDecisionCommand{TokenHash: hash, Record: rec, At: now}
	if decision == commerce.ConsentApproved {
		cmd.Mandate = &commerce.Mandate{
			ID: commerce.MandateID(d.IDs.NewID("mdt")), RequestID: r.ID, ConsentRecordID: rec.ID, Terms: r.Terms, ApprovedAt: now,
		}
	}
	if err := d.Mandates.Decide(ctx, cmd); err != nil {
		return ConsentOutcome{}, err
	}

	r.Status, r.DecidedAt = commerce.RequestRejected, &now
	action, details := app.AuditMandateRejected, map[string]string{
		"merchant_id": string(p.MerchantID), "customer_id": string(p.CustomerID), "consent_record_id": string(rec.ID),
		"terms_hash": rec.TermsHash, "decided_by": "customer", "method": rec.Method,
	}
	if cmd.Mandate != nil {
		r.Status, r.MandateID = commerce.RequestApproved, cmd.Mandate.ID
		action, details["mandate_id"] = app.AuditMandateApproved, string(cmd.Mandate.ID)
	}
	d.Audit.Record(ctx, p.OrgID, action, auditTargetMandateRequest, string(r.ID), details)
	return ConsentOutcome{Request: r, Mandate: cmd.Mandate}, nil
}

func view(row MandateRow, now time.Time) MandateView {
	return MandateView{Mandate: row.Mandate, Status: row.StatusAt(now, row.Deps)}
}

// ListMandates lists the merchant's mandates, newest first: what the
// merchant shows a customer who asks what they have allowed.
type ListMandates struct {
	Mandates MandateStore
	Clock    app.Clock
}

func (l *ListMandates) Run(ctx context.Context, p MerchantPrincipal, f MandateFilter) (MandatePage, error) {
	lf, err := ListFilter{Limit: f.Limit}.normalised()
	if err != nil {
		return MandatePage{}, err
	}
	f.Limit = lf.Limit
	if f.Status != "" {
		if f.Status, err = commerce.ParseMandateStatus(string(f.Status)); err != nil {
			return MandatePage{}, invalid("status", err)
		}
	}
	now := l.Clock.Now()
	rows, next, err := l.Mandates.ListMandates(ctx, p.Scope(), f, now)
	if err != nil {
		return MandatePage{}, err
	}
	page := MandatePage{NextCursor: next, Mandates: make([]MandateView, len(rows))}
	for i, row := range rows {
		page.Mandates[i] = view(row, now)
	}
	return page, nil
}

// GetMandate fetches one of the merchant's mandates.
type GetMandate struct {
	Mandates MandateStore
	Clock    app.Clock
}

func (g *GetMandate) Run(ctx context.Context, p MerchantPrincipal, id commerce.MandateID) (MandateView, error) {
	row, err := g.Mandates.GetMandate(ctx, p.Scope(), id)
	if err != nil {
		return MandateView{}, err
	}
	return view(row, g.Clock.Now()), nil
}

// RequestMandateRevocation is the merchant asking, for its customer, to
// revoke a mandate. The mandate grants nothing from that moment, and
// Enquoir emails the customer a one-time link to confirm; only the
// customer's confirmation revokes it. Unconfirmed within the hour, the
// request lapses and the mandate is active again.
type RequestMandateRevocation struct {
	Mandates  MandateStore
	Customers CustomerStore
	Emails    *app.Emails
	Clock     app.Clock
	Audit     *app.Audit
}

func (r *RequestMandateRevocation) Run(ctx context.Context, p MerchantPrincipal, id commerce.MandateID) (MandateView, error) {
	now := r.Clock.Now()
	changed, err := r.Mandates.RequestRevocation(ctx, p.Scope(), id, now, now.Add(commerce.RevocationLinkTTL))
	if err != nil {
		return MandateView{}, err
	}
	row, err := r.Mandates.GetMandate(ctx, p.Scope(), id)
	if err != nil {
		return MandateView{}, err
	}
	if changed {
		terms := row.Terms
		c, err := r.Customers.GetCustomer(ctx, p.Scope(), terms.Parties.CustomerID)
		if err != nil {
			return MandateView{}, err
		}
		if _, err := r.Emails.Send(ctx, app.Email{OrgID: p.OrgID, To: c.Email, Template: app.TemplateMandateRevocation,
			Params: map[string]string{"merchant": terms.Parties.MerchantName, "agent": terms.Parties.AgentName, "payment_method": paymentMethod(terms.Parties.FundingDisplay)},
			Link:   &app.LinkSpec{Purpose: app.PurposeMandateRevocation, Subject: string(id), TTL: commerce.RevocationLinkTTL},
		}); err != nil {
			return MandateView{}, fmt.Errorf("queueing the revocation email: %w", err)
		}
		r.Audit.Record(ctx, p.OrgID, app.AuditMandateRevocationRequested, auditTargetMandate, string(id), map[string]string{
			"merchant_id": string(p.MerchantID), "customer_id": string(terms.Parties.CustomerID),
		})
	}
	return view(row, now), nil
}

// revocationMandate resolves a revocation link to its mandate, without
// spending the link. Any problem is app.ErrTokenInvalid.
func revocationMandate(ctx context.Context, tokens app.TokenStore, mandates MandateStore, token string, now time.Time) (MandateRow, string, error) {
	if token == "" {
		return MandateRow{}, "", app.ErrTokenInvalid
	}
	hash := app.HashSecret(token)
	t, err := tokens.Peek(ctx, hash, app.PurposeMandateRevocation, now)
	if err != nil {
		return MandateRow{}, "", app.ErrTokenInvalid
	}
	row, err := mandates.MandateForRevocation(ctx, commerce.MandateID(t.Subject))
	if err != nil || !row.RevocationPendingAt(now) {
		return MandateRow{}, "", app.ErrTokenInvalid
	}
	return row, hash, nil
}

// ViewRevocation is the confirmation page reading the mandate by its
// link. It does not spend the link.
type ViewRevocation struct {
	Tokens   app.TokenStore
	Mandates MandateStore
	Clock    app.Clock
}

func (v *ViewRevocation) Run(ctx context.Context, token string) (MandateView, error) {
	now := v.Clock.Now()
	row, _, err := revocationMandate(ctx, v.Tokens, v.Mandates, token, now)
	if err != nil {
		return MandateView{}, err
	}
	return view(row, now), nil
}

// ConfirmRevocation is the customer confirming, with the link Enquoir
// emailed them, the revocation the merchant asked for. It spends the
// link and revokes the mandate for good.
type ConfirmRevocation struct {
	Tokens   app.TokenStore
	Mandates MandateStore
	Clock    app.Clock
	Audit    *app.Audit
}

func (c *ConfirmRevocation) Run(ctx context.Context, token string) (MandateView, error) {
	now := c.Clock.Now()
	row, hash, err := revocationMandate(ctx, c.Tokens, c.Mandates, token, now)
	if err != nil {
		return MandateView{}, err
	}
	if err := c.Mandates.ConfirmRevocation(ctx, hash, row.ID, now); err != nil {
		return MandateView{}, err
	}
	row.Revoked, row.RevokedAt = true, &now
	p := row.Terms.Parties
	c.Audit.Record(ctx, p.OrgID, app.AuditMandateRevoked, auditTargetMandate, string(row.ID), map[string]string{
		"merchant_id": string(p.MerchantID), "customer_id": string(p.CustomerID), "decided_by": "customer", "method": commerce.ConsentMethodEmailLink,
	})
	return view(row, now), nil
}
