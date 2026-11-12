//go:build integration

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"enquoir/internal/adapters/credentials"
	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

type mandateWorld struct {
	u       *commerceUseCases
	fu      fundingUseCases
	store   *CommerceMandateStore
	mailer  *apptest.Mailer
	request *commerceapp.RequestMandate
	decide  *commerceapp.DecideConsent
	view    *commerceapp.ViewConsent
	list    *commerceapp.ListMandates
	get     *commerceapp.GetMandate
	revoke  *commerceapp.RequestMandateRevocation
	confirm *commerceapp.ConfirmRevocation

	merchant commerceapp.MerchantPrincipal
	customer commerce.Customer
	agent    commerce.Agent
	card     commerce.FundingMethod
}

func newMandateWorld(t *testing.T) *mandateWorld {
	t.Helper()
	ctx := context.Background()
	pool := testPool(t)
	u, _, _ := newCommerceUseCases(t)
	fu := newFundingUseCases(t)
	clock, ids, secrets := realClock{}, credentials.RandomIDs{}, credentials.RandomSecrets{}
	tokens, outbox, mailer := NewTokenStore(pool), NewEmailOutbox(pool), &apptest.Mailer{}
	deliver := &app.DeliverEmail{Outbox: outbox, Tokens: tokens, Renderer: apptest.Renderer{}, Mailer: mailer, Secrets: secrets, Clock: clock, AppBaseURL: "https://enquoir.etimi.me"}
	emails := &app.Emails{Outbox: outbox, Dispatcher: &apptest.Dispatcher{Deliver: deliver.Run}, IDs: ids, Clock: clock}
	audit := &app.Audit{Log: NewAuditLog(pool), IDs: ids, Clock: clock}
	store := NewCommerceMandateStore(pool)
	customers, agents, methods := NewCommerceCustomerStore(pool), NewCommerceAgentStore(pool), NewCommerceFundingMethodStore(pool)
	w := &mandateWorld{
		u: u, fu: fu, store: store, mailer: mailer,
		request: &commerceapp.RequestMandate{Mandates: store, Customers: customers, Agents: agents, Methods: methods, Emails: emails, IDs: ids, Clock: clock, Audit: audit},
		decide:  &commerceapp.DecideConsent{Tokens: tokens, Mandates: store, IDs: ids, Clock: clock, Audit: audit},
		view:    &commerceapp.ViewConsent{Tokens: tokens, Mandates: store, Clock: clock},
		list:    &commerceapp.ListMandates{Mandates: store, Clock: clock},
		get:     &commerceapp.GetMandate{Mandates: store, Clock: clock},
		revoke:  &commerceapp.RequestMandateRevocation{Mandates: store, Customers: customers, Emails: emails, Clock: clock, Audit: audit},
		confirm: &commerceapp.ConfirmRevocation{Tokens: tokens, Mandates: store, Clock: clock, Audit: audit},
	}
	w.merchant, _ = u.newMerchant(t, NewAccountStore(pool))
	addr := fmt.Sprintf("delivered+%s@resend.dev", ids.NewID("c"))
	w.customer, _, _ = u.registerCustomer.Run(ctx, w.merchant, "cust", addr)
	w.agent, _, _ = u.registerAgent.Run(ctx, w.merchant, "assistant", "Assistant")
	var err error
	if w.card, _, err = fu.register.Run(ctx, w.merchant, cardOf(w.customer.ID, "mfm_1")); err != nil {
		t.Fatal(err)
	}
	return w
}

func (w *mandateWorld) cmd(ext string) commerceapp.MandateRequestCommand {
	return commerceapp.MandateRequestCommand{ExternalID: ext, CustomerID: w.customer.ID, AgentID: w.agent.ID, FundingMethodID: w.card.ID,
		ApprovalMode: "autonomous", Currency: "EUR", MaxPerTransactionMinor: 5000, MaxTotalMinor: 20000, AllowedCategories: []string{"books"},
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour)}
}

func (w *mandateWorld) token() string { return w.mailer.TokenTo(w.customer.Email) }

func TestMandateStore_RequestApproveListRevoke(t *testing.T) {
	ctx := context.Background()
	w := newMandateWorld(t)
	r, created, err := w.request.Run(ctx, w.merchant, w.cmd("mreq_1"))
	if err != nil || !created {
		t.Fatalf("request: %v %v", created, err)
	}
	again, created, err := w.request.Run(ctx, w.merchant, w.cmd("mreq_1"))
	if err != nil || created || again.ID != r.ID || again.Terms.Hash() != r.Terms.Hash() {
		t.Fatalf("repeat: %+v %v %v", again, created, err)
	}
	if v, err := w.view.Run(ctx, w.token()); err != nil || v.Terms.Hash() != r.Terms.Hash() {
		t.Fatalf("view: %+v %v", v, err)
	}
	actx := app.WithActor(ctx, app.Actor{IP: "203.0.113.7", UserAgent: "Mozilla/5.0"})
	if _, err := w.decide.Run(actx, w.token(), "sha256:nope", commerce.ConsentApproved); !errors.Is(err, commerceapp.ErrTermsMismatch) {
		t.Fatalf("wrong hash: %v", err)
	}
	d, err := w.decide.Run(actx, w.token(), r.Terms.Hash(), commerce.ConsentApproved)
	if err != nil || d.Mandate == nil {
		t.Fatalf("approve: %v", err)
	}
	if _, err := w.decide.Run(actx, w.token(), r.Terms.Hash(), commerce.ConsentApproved); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("second approval: %v", err)
	}
	stored, err := w.store.GetRequest(ctx, w.merchant.Scope(), r.ID)
	if err != nil || stored.Status != commerce.RequestApproved || stored.MandateID != d.Mandate.ID || stored.DecidedAt == nil {
		t.Errorf("request after approval: %+v %v", stored, err)
	}

	m, err := w.get.Run(ctx, w.merchant, d.Mandate.ID)
	if err != nil || m.Status != commerce.MandateActive || m.Terms.Hash() != r.Terms.Hash() || m.Terms.Scope.Currency != "EUR" {
		t.Fatalf("mandate: %+v %v", m, err)
	}
	var snapshot, hash, ip, ua string
	if err := testPool(t).QueryRow(ctx, `SELECT snapshot, terms_hash, ip, user_agent FROM commerce_consent_records WHERE id = $1`,
		string(m.ConsentRecordID)).Scan(&snapshot, &hash, &ip, &ua); err != nil {
		t.Fatal(err)
	}
	if snapshot != string(r.Terms.Canonical()) || hash != r.Terms.Hash() || ip != "203.0.113.7" || ua != "Mozilla/5.0" {
		t.Errorf("consent record: %s %s %s %s", snapshot, hash, ip, ua)
	}

	// Disabling the card suspends the mandate; status filters agree.
	w.fu.status.Run(ctx, w.merchant, w.card.ID, commerce.Disabled)
	if m, _ := w.get.Run(ctx, w.merchant, m.ID); m.Status != commerce.MandateSuspended {
		t.Errorf("card disabled: %s", m.Status)
	}
	for status, n := range map[commerce.MandateStatus]int{commerce.MandateSuspended: 1, commerce.MandateActive: 0} {
		page, err := w.list.Run(ctx, w.merchant, commerceapp.MandateFilter{Status: status})
		if err != nil || len(page.Mandates) != n {
			t.Errorf("status %s: %d mandates, %v; want %d", status, len(page.Mandates), err, n)
		}
	}
	w.fu.status.Run(ctx, w.merchant, w.card.ID, commerce.Active)

	got, err := w.revoke.Run(ctx, w.merchant, m.ID)
	if err != nil || got.Status != commerce.MandateRevocationPending {
		t.Fatalf("asking to revoke: %+v %v", got, err)
	}
	if page, _ := w.list.Run(ctx, w.merchant, commerceapp.MandateFilter{Status: commerce.MandateRevocationPending}); len(page.Mandates) != 1 {
		t.Errorf("revocation_pending: %+v", page.Mandates)
	}
	revocationToken := w.token() // the last link emailed: the revocation one
	if !strings.Contains(w.mailer.LastLinkTo(w.customer.Email), "/consent/revoke?token=") {
		t.Fatalf("no revocation link was emailed")
	}
	if err := w.store.ConfirmRevocation(ctx, app.HashSecret("not-the-link"), m.ID, time.Now()); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("confirming with another token: %v", err)
	}
	if got, err := w.confirm.Run(ctx, revocationToken); err != nil || got.Status != commerce.MandateRevoked {
		t.Fatalf("confirm: %+v %v", got, err)
	}
	page, _ := w.list.Run(ctx, w.merchant, commerceapp.MandateFilter{Status: commerce.MandateRevoked, CustomerID: w.customer.ID})
	if len(page.Mandates) != 1 {
		t.Errorf("revoked: %+v", page.Mandates)
	}
}

func TestMandateStore_ScopedToTheMerchant(t *testing.T) {
	ctx := context.Background()
	w := newMandateWorld(t)
	r, _, _ := w.request.Run(ctx, w.merchant, w.cmd("mreq_1"))
	d, err := w.decide.Run(ctx, w.token(), r.Terms.Hash(), commerce.ConsentApproved)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := w.u.newMerchant(t, NewAccountStore(testPool(t)))
	if _, err := w.get.Run(ctx, other, d.Mandate.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("get across merchants: %v", err)
	}
	if _, err := w.revoke.Run(ctx, other, d.Mandate.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("revoke across merchants: %v", err)
	}
	if _, err := w.store.GetRequest(ctx, other.Scope(), r.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("request across merchants: %v", err)
	}
	if m, _ := w.get.Run(ctx, w.merchant, d.Mandate.ID); m.Status != commerce.MandateActive {
		t.Errorf("after the other's attempt: %s", m.Status)
	}
}

// One link, eight simultaneous approvals: one mandate, one record.
func TestMandateStore_ConcurrentApprovalsMakeOneMandate(t *testing.T) {
	ctx := context.Background()
	w := newMandateWorld(t)
	r, _, _ := w.request.Run(ctx, w.merchant, w.cmd("mreq_race"))
	token := w.token()
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.decide.Run(ctx, token, r.Terms.Hash(), commerce.ConsentApproved); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var mandates, records int
	testPool(t).QueryRow(ctx, `SELECT (SELECT count(*) FROM commerce_mandates WHERE request_id = $1), (SELECT count(*) FROM commerce_consent_records WHERE request_id = $1)`,
		string(r.ID)).Scan(&mandates, &records)
	if ok != 1 || mandates != 1 || records != 1 {
		t.Errorf("%d approvals succeeded; %d mandates, %d records; want 1, 1, 1", ok, mandates, records)
	}
}

// The database refuses what the rules forbid, whatever the code does:
// a consent record can't be changed or deleted, a stored hash must be
// its snapshot's, and a request's card must be its customer's.
func TestMandateStore_TheSchemaGuardsTheEvidence(t *testing.T) {
	ctx := context.Background()
	w := newMandateWorld(t)
	pool := testPool(t)
	r, _, _ := w.request.Run(ctx, w.merchant, w.cmd("mreq_1"))
	d, _ := w.decide.Run(ctx, w.token(), r.Terms.Hash(), commerce.ConsentApproved)

	if _, err := pool.Exec(ctx, `UPDATE commerce_consent_records SET ip = '0.0.0.0' WHERE id = $1`, string(d.Mandate.ConsentRecordID)); err == nil {
		t.Error("a consent record was changed")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM commerce_consent_records WHERE id = $1`, string(d.Mandate.ConsentRecordID)); err == nil {
		t.Error("a consent record was deleted")
	}
	if _, err := pool.Exec(ctx, `UPDATE commerce_mandates SET terms = replace(terms, '20000', '99999') WHERE id = $1`, string(d.Mandate.ID)); err == nil {
		t.Error("a mandate's terms were changed without their hash")
	}

	bob, _, _ := w.u.registerCustomer.Run(ctx, w.merchant, "bob", "delivered+bob@resend.dev")
	bad := w.cmd("mreq_bad")
	bad.CustomerID = bob.ID // the card is the first customer's
	terms, _ := commerce.NewMandateTerms(commerce.MandateParties{OrgID: w.merchant.OrgID, MerchantID: w.merchant.MerchantID,
		CustomerID: bob.ID, AgentID: w.agent.ID, FundingMethodID: w.card.ID}, commerce.Autonomous, r.Terms.Scope, time.Now().Add(time.Hour), time.Now())
	req := commerce.MandateRequest{ID: commerce.MandateRequestID(credentials.RandomIDs{}.NewID("mreq")), ExternalID: "mreq_bad", Terms: terms,
		Status: commerce.RequestPending, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if _, _, err := w.store.CreateRequest(ctx, req); err == nil {
		t.Error("stored a request for one customer with another customer's card")
	}
}

// Decide spends the link itself: a pending request is not decided by a
// command carrying a token that isn't its unspent link.
func TestMandateStore_DecideRequiresTheRequestsOwnLink(t *testing.T) {
	ctx := context.Background()
	w := newMandateWorld(t)
	r, _, _ := w.request.Run(ctx, w.merchant, w.cmd("mreq_1"))
	now := time.Now().UTC()
	rec := commerce.ConsentRecord{ID: commerce.ConsentRecordID(credentials.RandomIDs{}.NewID("cns")), RequestID: r.ID,
		OrgID: w.merchant.OrgID, MerchantID: w.merchant.MerchantID, CustomerID: w.customer.ID, Decision: commerce.ConsentRejected,
		Snapshot: r.Terms.Canonical(), TermsHash: r.Terms.Hash(), Method: commerce.ConsentMethodEmailLink, At: now}
	err := w.store.Decide(ctx, commerceapp.ConsentDecisionCommand{TokenHash: app.HashSecret("not-the-link"), Record: rec, At: now})
	if !errors.Is(err, app.ErrTokenInvalid) {
		t.Fatalf("deciding with another token: %v", err)
	}
	if got, _ := w.store.GetRequest(ctx, w.merchant.Scope(), r.ID); got.Status != commerce.RequestPending {
		t.Errorf("request after the refused decision: %s, want still pending", got.Status)
	}
}

// A sibling storefront of the same organisation is another merchant too.
func TestMandateStore_ASiblingStorefrontCannotRevoke(t *testing.T) {
	ctx := context.Background()
	w := newMandateWorld(t)
	r, _, _ := w.request.Run(ctx, w.merchant, w.cmd("mreq_1"))
	d, err := w.decide.Run(ctx, w.token(), r.Terms.Hash(), commerce.ConsentApproved)
	if err != nil {
		t.Fatal(err)
	}
	sibling, _ := w.u.newMerchantIn(t, w.merchant.OrgID)
	if _, err := w.revoke.Run(ctx, sibling, d.Mandate.ID); !errors.Is(err, app.ErrNotFound) {
		t.Errorf("sibling revoking: %v", err)
	}
	if m, _ := w.get.Run(ctx, w.merchant, d.Mandate.ID); m.Status != commerce.MandateActive {
		t.Errorf("after the sibling's attempt: %s, want still active", m.Status)
	}
}

// The store revokes only while a revocation the merchant asked for is
// awaiting confirmation: a valid-looking link for a mandate nobody asked
// to revoke does nothing.
func TestMandateStore_ConfirmOnlyAPendingRevocation(t *testing.T) {
	ctx := context.Background()
	w := newMandateWorld(t)
	r, _, _ := w.request.Run(ctx, w.merchant, w.cmd("mreq_1"))
	d, err := w.decide.Run(ctx, w.token(), r.Terms.Hash(), commerce.ConsentApproved)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	link := app.HashSecret(credentials.RandomIDs{}.NewID("tok"))
	NewTokenStore(testPool(t)).Create(ctx, app.OneTimeToken{Hash: link, Purpose: app.PurposeMandateRevocation, Subject: string(d.Mandate.ID), CreatedAt: now, ExpiresAt: now.Add(time.Hour)})
	if err := w.store.ConfirmRevocation(ctx, link, d.Mandate.ID, now); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("confirming with no revocation pending: %v", err)
	}
	if m, _ := w.get.Run(ctx, w.merchant, d.Mandate.ID); m.Status != commerce.MandateActive {
		t.Errorf("after: %s, want still active", m.Status)
	}
}
