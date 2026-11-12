package commerceapp_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/commerce/commerceapp/commercetest"
)

type mandatesFixture struct {
	*fundingFixture
	mailer  *apptest.Mailer
	tokens  *apptest.Tokens
	emails  *app.Emails
	auditor *app.Audit
	ids     *apptest.IDs
	store   *commercetest.Mandates
	request *commerceapp.RequestMandate
	getReq  *commerceapp.GetMandateRequest
	view    *commerceapp.ViewConsent
	decide  *commerceapp.DecideConsent
	list    *commerceapp.ListMandates
	get     *commerceapp.GetMandate
	revoke  *commerceapp.RequestMandateRevocation
	viewRev *commerceapp.ViewRevocation
	confirm *commerceapp.ConfirmRevocation
	ada     commerce.Customer
	bot     commerce.Agent
	card    commerce.FundingMethod
}

func newMandatesFixture(t *testing.T) *mandatesFixture {
	t.Helper()
	f := newFundingFixture()
	ids, secrets := &apptest.IDs{}, &apptest.Secrets{}
	tokens, outbox, mailer := apptest.NewTokens(), apptest.NewOutbox(), &apptest.Mailer{}
	deliver := &app.DeliverEmail{Outbox: outbox, Tokens: tokens, Renderer: apptest.Renderer{}, Mailer: mailer,
		Secrets: secrets, Clock: f.clock, AppBaseURL: "https://enquoir.etimi.me"}
	emails := &app.Emails{Outbox: outbox, Dispatcher: &apptest.Dispatcher{Deliver: deliver.Run}, IDs: ids, Clock: f.clock}
	audit := &app.Audit{Log: f.audit, IDs: ids, Clock: f.clock}
	customers, agents, methods := f.getCustomer.Customers, f.getAgent.Agents, f.get.Methods
	store := commercetest.NewMandates(tokens, customers.(*commercetest.Customers), agents.(*commercetest.Agents), methods.(*commercetest.FundingMethods))
	m := &mandatesFixture{
		fundingFixture: f, mailer: mailer, store: store, tokens: tokens, emails: emails, auditor: audit, ids: ids,
		request: &commerceapp.RequestMandate{Mandates: store, Customers: customers, Agents: agents, Methods: methods, Emails: emails, IDs: ids, Clock: f.clock, Audit: audit},
		getReq:  &commerceapp.GetMandateRequest{Mandates: store, Clock: f.clock},
		view:    &commerceapp.ViewConsent{Tokens: tokens, Mandates: store, Clock: f.clock},
		decide:  &commerceapp.DecideConsent{Tokens: tokens, Mandates: store, IDs: ids, Clock: f.clock, Audit: audit},
		list:    &commerceapp.ListMandates{Mandates: store, Clock: f.clock},
		get:     &commerceapp.GetMandate{Mandates: store, Clock: f.clock},
		revoke:  &commerceapp.RequestMandateRevocation{Mandates: store, Customers: customers, Emails: emails, Clock: f.clock, Audit: audit},
		viewRev: &commerceapp.ViewRevocation{Tokens: tokens, Mandates: store, Clock: f.clock},
		confirm: &commerceapp.ConfirmRevocation{Tokens: tokens, Mandates: store, Clock: f.clock, Audit: audit},
	}
	m.ada = f.customer(t, shopA1, "cust_42", "ada@example.com")
	m.bot = f.agent(t, shopA1, "assistant", "Acme Shopping Assistant")
	m.card = f.bind(t, shopA1, visa(m.ada.ID, "mfm_1"))
	return m
}

func (f *mandatesFixture) cmd(ext string) commerceapp.MandateRequestCommand {
	return commerceapp.MandateRequestCommand{
		ExternalID: ext, CustomerID: f.ada.ID, AgentID: f.bot.ID, FundingMethodID: f.card.ID,
		ApprovalMode: "autonomous", Currency: "USD", MaxPerTransactionMinor: 150000, MaxTotalMinor: 300000,
		MaxTransactions: 3, AllowedCategories: []string{"electronics"}, ExpiresAt: f.clock.Now().Add(30 * 24 * time.Hour),
	}
}

func (f *mandatesFixture) mustRequest(t *testing.T, ext string) commerce.MandateRequest {
	t.Helper()
	r, _, err := f.request.Run(context.Background(), shopA1, f.cmd(ext))
	if err != nil {
		t.Fatalf("requesting a mandate: %v", err)
	}
	return r
}

func (f *mandatesFixture) token() string { return f.mailer.TokenTo(f.ada.Email) }

// approve requests a mandate and approves it as the customer would.
func (f *mandatesFixture) approve(t *testing.T, ext string) commerce.Mandate {
	t.Helper()
	r := f.mustRequest(t, ext)
	d, err := f.decide.Run(context.Background(), f.token(), r.Terms.Hash(), commerce.ConsentApproved)
	if err != nil || d.Mandate == nil {
		t.Fatalf("approving: %+v, %v", d, err)
	}
	return *d.Mandate
}

func (f *mandatesFixture) actions(prefix string) []string {
	var out []string
	for _, e := range f.audit.All() {
		if strings.HasPrefix(e.Action, prefix) {
			out = append(out, e.Action)
		}
	}
	return out
}

func TestRequestMandate_FreezesTheTermsAndEmailsTheCustomerALink(t *testing.T) {
	f := newMandatesFixture(t)
	r, created, err := f.request.Run(context.Background(), shopA1, f.cmd("mreq_1"))
	if err != nil || !created {
		t.Fatalf("request: created %v, %v", created, err)
	}
	if r.ID == "" || r.Status != commerce.RequestPending || !r.ExpiresAt.Equal(f.clock.Now().Add(time.Hour)) || r.MandateID != "" {
		t.Errorf("request = %+v", r)
	}
	p := r.Terms.Parties
	if p.MerchantName != "A1" || p.AgentName != "Acme Shopping Assistant" || p.FundingDisplay.LastFour != "4242" ||
		p.CustomerID != f.ada.ID || r.Terms.Scope.MaxTotalMinor != 300000 || r.Terms.ApprovalMode != commerce.Autonomous {
		t.Errorf("terms = %+v", r.Terms)
	}

	sent := f.mailer.Sent()
	if len(sent) != 1 || sent[0].To != "ada@example.com" {
		t.Fatalf("emails = %+v, want one to the customer", sent)
	}
	link := f.mailer.LastLinkTo("ada@example.com")
	if !strings.HasPrefix(link, "https://enquoir.etimi.me/consent?token=") {
		t.Errorf("link = %q", link)
	}
	for _, want := range []string{"A1", "Acme Shopping Assistant", "visa ending 4242", "USD 1500.00", "USD 3000.00", "electronics"} {
		if !strings.Contains(sent[0].Text, want) {
			t.Errorf("the consent email does not show %q:\n%s", want, sent[0].Text)
		}
	}
	// The merchant gets the request, never the token.
	if strings.Contains(string(r.ExternalID)+string(r.ID), f.token()) {
		t.Error("the token leaked into the request")
	}
	if got := f.actions("commerce.mandate."); len(got) != 1 || got[0] != app.AuditMandateRequested {
		t.Errorf("audit = %v", got)
	}
}

func TestRequestMandate_RepeatResolvesWithoutAnotherEmail_AndDifferentTermsConflict(t *testing.T) {
	f := newMandatesFixture(t)
	first := f.mustRequest(t, "mreq_1")
	again, created, err := f.request.Run(context.Background(), shopA1, f.cmd("mreq_1"))
	if err != nil || created || again.ID != first.ID {
		t.Fatalf("repeat: %+v %v %v", again, created, err)
	}
	if n := len(f.mailer.Sent()); n != 1 {
		t.Errorf("%d emails, want 1: a repeat sends nothing", n)
	}
	changed := f.cmd("mreq_1")
	changed.MaxTotalMinor = 900000
	if _, _, err := f.request.Run(context.Background(), shopA1, changed); !errors.Is(err, commerceapp.ErrRegistrationConflict) {
		t.Errorf("different terms: %v, want a conflict", err)
	}
}

func TestRequestMandate_ValidatesEveryPartyAndTerm(t *testing.T) {
	f := newMandatesFixture(t)
	ctx := context.Background()
	bob := f.customer(t, shopA1, "bob", "bob@example.com")
	bobsCard := f.bind(t, shopA1, visa(bob.ID, "mfm_bob"))
	theirCustomer := f.customer(t, shopA2, "x", "x@example.com")
	theirAgent := f.agent(t, shopA2, "x", "X")

	sentinels := map[string]struct {
		change func(*commerceapp.MandateRequestCommand)
		want   error
	}{
		"another merchant's customer": {func(c *commerceapp.MandateRequestCommand) { c.CustomerID = theirCustomer.ID }, commerceapp.ErrCustomerUnknown},
		"another merchant's agent":    {func(c *commerceapp.MandateRequestCommand) { c.AgentID = theirAgent.ID }, commerceapp.ErrAgentUnknown},
		"another customer's card":     {func(c *commerceapp.MandateRequestCommand) { c.FundingMethodID = bobsCard.ID }, commerceapp.ErrFundingMethodUnknown},
		"a made-up card":              {func(c *commerceapp.MandateRequestCommand) { c.FundingMethodID = "fmb_nope" }, commerceapp.ErrFundingMethodUnknown},
	}
	for name, tc := range sentinels {
		c := f.cmd("m_" + strings.ReplaceAll(name, " ", "_"))
		tc.change(&c)
		if _, _, err := f.request.Run(ctx, shopA1, c); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}

	fields := map[string]struct {
		change func(*commerceapp.MandateRequestCommand)
		field  string
	}{
		"no external ID":    {func(c *commerceapp.MandateRequestCommand) { c.ExternalID = "" }, "external_id"},
		"no customer":       {func(c *commerceapp.MandateRequestCommand) { c.CustomerID = "" }, "customer_id"},
		"no agent":          {func(c *commerceapp.MandateRequestCommand) { c.AgentID = "" }, "agent_id"},
		"no card":           {func(c *commerceapp.MandateRequestCommand) { c.FundingMethodID = "" }, "funding_method_id"},
		"bad mode":          {func(c *commerceapp.MandateRequestCommand) { c.ApprovalMode = "yolo" }, "approval_mode"},
		"NGN":               {func(c *commerceapp.MandateRequestCommand) { c.Currency = "NGN" }, "scope.currency"},
		"per tx over total": {func(c *commerceapp.MandateRequestCommand) { c.MaxPerTransactionMinor = 400000 }, "scope.max_cumulative_minor"},
		"too far ahead":     {func(c *commerceapp.MandateRequestCommand) { c.ExpiresAt = f.clock.Now().Add(91 * 24 * time.Hour) }, "expires_at"},
	}
	for name, tc := range fields {
		c := f.cmd("f_" + strings.ReplaceAll(name, " ", "_"))
		tc.change(&c)
		_, _, err := f.request.Run(ctx, shopA1, c)
		var verr *app.ValidationError
		if !errors.As(err, &verr) || verr.Field != tc.field {
			t.Errorf("%s: %v, want a validation error on %s", name, err, tc.field)
		}
	}

	disabled := map[string]struct {
		disable func()
		want    error
	}{
		"customer": {func() { f.customerStatus.Run(ctx, shopA1, f.ada.ID, commerce.Disabled) }, commerceapp.ErrCustomerDisabled},
		"agent":    {func() { f.agentStatus.Run(ctx, shopA1, f.bot.ID, commerce.Disabled) }, commerceapp.ErrAgentDisabled},
		"card":     {func() { f.status.Run(ctx, shopA1, f.card.ID, commerce.Disabled) }, commerceapp.ErrFundingMethodDisabled},
	}
	for _, name := range []string{"card", "agent", "customer"} { // innermost first
		disabled[name].disable()
		if _, _, err := f.request.Run(ctx, shopA1, f.cmd("d_"+name)); !errors.Is(err, disabled[name].want) {
			t.Errorf("disabled %s: %v, want %v", name, err, disabled[name].want)
		}
	}
	if n := len(f.mailer.Sent()); n != 0 {
		t.Errorf("refused requests sent %d emails", n)
	}
}

func TestViewConsent_ShowsTheTermsWithoutSpendingTheLink(t *testing.T) {
	f := newMandatesFixture(t)
	r := f.mustRequest(t, "mreq_1")
	for range 2 {
		v, err := f.view.Run(context.Background(), f.token())
		if err != nil || v.ID != r.ID || v.Terms.Hash() != r.Terms.Hash() {
			t.Fatalf("view: %+v, %v", v, err)
		}
	}
	for _, bad := range []string{"", "tok_made_up"} {
		if _, err := f.view.Run(context.Background(), bad); !errors.Is(err, app.ErrTokenInvalid) {
			t.Errorf("token %q: %v", bad, err)
		}
	}
}

func TestDecideConsent_ApprovalCreatesTheMandateAndAnImmutableRecord(t *testing.T) {
	f := newMandatesFixture(t)
	r := f.mustRequest(t, "mreq_1")
	ctx := app.WithActor(context.Background(), app.Actor{IP: "203.0.113.7", UserAgent: "Mozilla/5.0"})

	d, err := f.decide.Run(ctx, f.token(), r.Terms.Hash(), commerce.ConsentApproved)
	if err != nil || d.Mandate == nil {
		t.Fatalf("approve: %+v, %v", d, err)
	}
	m := *d.Mandate
	if m.RequestID != r.ID || m.Terms.Hash() != r.Terms.Hash() || m.Revoked || m.ConsentRecordID == "" {
		t.Errorf("mandate = %+v", m)
	}
	if d.Request.Status != commerce.RequestApproved || d.Request.MandateID != m.ID {
		t.Errorf("request after approval = %+v", d.Request)
	}
	rec, ok := f.store.Consent(m.ConsentRecordID)
	if !ok || rec.Decision != commerce.ConsentApproved || rec.TermsHash != r.Terms.Hash() || string(rec.Snapshot) != string(r.Terms.Canonical()) ||
		rec.Method != commerce.ConsentMethodEmailLink || rec.IP != "203.0.113.7" || rec.UserAgent != "Mozilla/5.0" || rec.CustomerID != f.ada.ID {
		t.Errorf("consent record = %+v", rec)
	}
	got, err := f.get.Run(context.Background(), shopA1, m.ID)
	if err != nil || got.Status != commerce.MandateActive {
		t.Errorf("mandate as the merchant sees it: %+v, %v", got, err)
	}
	if r2, _ := f.getReq.Run(context.Background(), shopA1, r.ID); r2.Status != commerce.RequestApproved {
		t.Errorf("request status = %s", r2.Status)
	}
	if a := f.actions("commerce.mandate."); strings.Join(a, ",") != app.AuditMandateRequested+","+app.AuditMandateApproved {
		t.Errorf("audit = %v", a)
	}

	// The link worked once.
	if _, err := f.decide.Run(ctx, f.token(), r.Terms.Hash(), commerce.ConsentApproved); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("a second approval with the same link: %v", err)
	}
	if _, err := f.view.Run(ctx, f.token()); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("viewing a spent link: %v", err)
	}
}

// Consent binds to exactly what the page showed.
func TestDecideConsent_RefusesApprovalOfOtherTerms(t *testing.T) {
	f := newMandatesFixture(t)
	f.mustRequest(t, "mreq_1")
	for _, hash := range []string{"", "sha256:0000"} {
		if _, err := f.decide.Run(context.Background(), f.token(), hash, commerce.ConsentApproved); !errors.Is(err, commerceapp.ErrTermsMismatch) {
			t.Errorf("hash %q: %v, want terms mismatch", hash, err)
		}
	}
	if page, _ := f.list.Run(context.Background(), shopA1, commerceapp.MandateFilter{}); len(page.Mandates) != 0 {
		t.Error("a mismatched approval created a mandate")
	}
	// The link was not spent by the refusal.
	if _, err := f.view.Run(context.Background(), f.token()); err != nil {
		t.Errorf("view after a refused approval: %v", err)
	}
}

func TestDecideConsent_RejectionRecordsTheDecisionAndCreatesNoMandate(t *testing.T) {
	f := newMandatesFixture(t)
	r := f.mustRequest(t, "mreq_1")
	d, err := f.decide.Run(context.Background(), f.token(), "", commerce.ConsentRejected)
	if err != nil || d.Mandate != nil || d.Request.Status != commerce.RequestRejected {
		t.Fatalf("reject: %+v, %v", d, err)
	}
	if recs := f.store.ConsentsFor(r.ID); len(recs) != 1 || recs[0].Decision != commerce.ConsentRejected || recs[0].TermsHash != r.Terms.Hash() {
		t.Errorf("consent records = %+v", recs)
	}
	if page, _ := f.list.Run(context.Background(), shopA1, commerceapp.MandateFilter{}); len(page.Mandates) != 0 {
		t.Error("a rejection created a mandate")
	}
	if a := f.actions("commerce.mandate."); a[len(a)-1] != app.AuditMandateRejected {
		t.Errorf("audit = %v", a)
	}
}

func TestDecideConsent_AnExpiredRequestCannotBeDecided(t *testing.T) {
	f := newMandatesFixture(t)
	r := f.mustRequest(t, "mreq_1")
	token := f.token()
	f.clock.Advance(time.Hour)
	if _, err := f.decide.Run(context.Background(), token, r.Terms.Hash(), commerce.ConsentApproved); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("approving after an hour: %v", err)
	}
	if got, _ := f.getReq.Run(context.Background(), shopA1, r.ID); got.Status != commerce.RequestExpired {
		t.Errorf("request status = %s, want expired", got.Status)
	}
}

// Two approvals racing with one link make one mandate.
func TestDecideConsent_ConcurrentApprovalsMakeOneMandate(t *testing.T) {
	f := newMandatesFixture(t)
	r := f.mustRequest(t, "mreq_1")
	token := f.token()
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.decide.Run(context.Background(), token, r.Terms.Hash(), commerce.ConsentApproved); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	page, _ := f.list.Run(context.Background(), shopA1, commerceapp.MandateFilter{})
	if ok != 1 || len(page.Mandates) != 1 {
		t.Errorf("%d approvals succeeded, %d mandates; want 1 and 1", ok, len(page.Mandates))
	}
}

func TestMandates_StatusFollowsWhatTheyName(t *testing.T) {
	f := newMandatesFixture(t)
	ctx := context.Background()
	m := f.approve(t, "mreq_1")
	status := func() commerce.MandateStatus {
		got, err := f.get.Run(ctx, shopA1, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got.Status
	}
	f.status.Run(ctx, shopA1, f.card.ID, commerce.Disabled)
	if s := status(); s != commerce.MandateSuspended {
		t.Errorf("card disabled: %s", s)
	}
	f.status.Run(ctx, shopA1, f.card.ID, commerce.Active)
	f.agentStatus.Run(ctx, shopA1, f.bot.ID, commerce.Disabled)
	if s := status(); s != commerce.MandateSuspended {
		t.Errorf("agent disabled: %s", s)
	}
	f.agentStatus.Run(ctx, shopA1, f.bot.ID, commerce.Active)
	if s := status(); s != commerce.MandateActive {
		t.Errorf("all restored: %s", s)
	}
	f.clock.Advance(31 * 24 * time.Hour)
	if s := status(); s != commerce.MandateExpired {
		t.Errorf("past expiry: %s", s)
	}
}

// revokeConfirmed has the merchant ask to revoke m, and the customer
// confirm from the emailed link.
func (f *mandatesFixture) revokeConfirmed(t *testing.T, id commerce.MandateID) {
	t.Helper()
	if _, err := f.revoke.Run(context.Background(), shopA1, id); err != nil {
		t.Fatal(err)
	}
	if _, err := f.confirm.Run(context.Background(), f.revocationToken()); err != nil {
		t.Fatal(err)
	}
}

func (f *mandatesFixture) revocationToken() string {
	link := f.mailer.LastLinkTo(f.ada.Email)
	if !strings.Contains(link, "/consent/revoke?token=") {
		return ""
	}
	return f.mailer.TokenTo(f.ada.Email)
}

// The merchant asks; the mandate grants nothing at once; only the
// customer, from the emailed link, makes the revocation final.
func TestRevokeMandate_WaitsForTheCustomersConfirmation(t *testing.T) {
	f := newMandatesFixture(t)
	ctx := context.Background()
	m := f.approve(t, "mreq_1")
	before := len(f.mailer.Sent())

	got, err := f.revoke.Run(ctx, shopA1, m.ID)
	if err != nil || got.Status != commerce.MandateRevocationPending || got.RevokedAt != nil {
		t.Fatalf("asking: %+v, %v; want revocation_pending, not revoked", got, err)
	}
	sent := f.mailer.Sent()
	if len(sent) != before+1 || sent[len(sent)-1].To != "ada@example.com" || f.revocationToken() == "" {
		t.Fatalf("confirmation email: %+v", sent[before:])
	}
	// Asking again while it's pending sends nothing more.
	if again, err := f.revoke.Run(ctx, shopA1, m.ID); err != nil || again.Status != commerce.MandateRevocationPending || len(f.mailer.Sent()) != before+1 {
		t.Errorf("asking again: %+v, %v, %d emails", again, err, len(f.mailer.Sent())-before)
	}

	token := f.revocationToken()
	if v, err := f.viewRev.Run(ctx, token); err != nil || v.ID != m.ID || v.Status != commerce.MandateRevocationPending {
		t.Errorf("the confirmation page: %+v, %v", v, err)
	}
	actx := app.WithActor(ctx, app.Actor{IP: "203.0.113.7", UserAgent: "Mozilla/5.0"})
	done, err := f.confirm.Run(actx, token)
	if err != nil || done.Status != commerce.MandateRevoked || done.RevokedAt == nil {
		t.Fatalf("confirming: %+v, %v", done, err)
	}
	if _, err := f.confirm.Run(actx, token); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("the link a second time: %v", err)
	}
	if got, _ := f.get.Run(ctx, shopA1, m.ID); got.Status != commerce.MandateRevoked {
		t.Errorf("as the merchant sees it: %s", got.Status)
	}
	var events []app.AuditEvent
	for _, e := range f.audit.All() {
		if e.Action == app.AuditMandateRevocationRequested || e.Action == app.AuditMandateRevoked {
			events = append(events, e)
		}
	}
	if len(events) != 2 || events[1].Action != app.AuditMandateRevoked || events[1].Details["decided_by"] != "customer" || events[1].IP != "203.0.113.7" {
		t.Errorf("audit = %+v", events)
	}
	// Revoked is final: asking again changes and sends nothing.
	if again, err := f.revoke.Run(ctx, shopA1, m.ID); err != nil || again.Status != commerce.MandateRevoked || len(f.mailer.Sent()) != before+1 {
		t.Errorf("asking after revocation: %+v, %v", again, err)
	}
}

// Unconfirmed, the request lapses after the hour and the mandate works
// again; the old link no longer revokes; the merchant may ask again.
func TestRevokeMandate_AnUnconfirmedRequestLapses(t *testing.T) {
	f := newMandatesFixture(t)
	ctx := context.Background()
	m := f.approve(t, "mreq_1")
	f.revoke.Run(ctx, shopA1, m.ID)
	stale := f.revocationToken()
	f.clock.Advance(time.Hour)

	if got, _ := f.get.Run(ctx, shopA1, m.ID); got.Status != commerce.MandateActive {
		t.Errorf("after the hour: %s, want active again", got.Status)
	}
	if _, err := f.confirm.Run(ctx, stale); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("an expired link: %v", err)
	}
	before := len(f.mailer.Sent())
	if got, err := f.revoke.Run(ctx, shopA1, m.ID); err != nil || got.Status != commerce.MandateRevocationPending || len(f.mailer.Sent()) != before+1 {
		t.Errorf("asking again: %+v, %v", got, err)
	}
	if _, err := f.confirm.Run(ctx, f.revocationToken()); err != nil {
		t.Errorf("confirming the new request: %v", err)
	}
}

// A consent link is not a revocation link, nor the other way round.
func TestRevokeMandate_OnlyTheRevocationLinkConfirms(t *testing.T) {
	f := newMandatesFixture(t)
	ctx := context.Background()
	r := f.mustRequest(t, "mreq_1")
	consentToken := f.token()
	d, err := f.decide.Run(ctx, consentToken, r.Terms.Hash(), commerce.ConsentApproved)
	if err != nil {
		t.Fatal(err)
	}
	f.mustRequest(t, "mreq_2")
	otherConsent := f.token()
	f.revoke.Run(ctx, shopA1, d.Mandate.ID)
	for _, tok := range []string{"", consentToken, otherConsent, "tok_made_up"} {
		if _, err := f.confirm.Run(ctx, tok); !errors.Is(err, app.ErrTokenInvalid) {
			t.Errorf("confirming with %q: %v", tok, err)
		}
	}
	if got, _ := f.get.Run(ctx, shopA1, d.Mandate.ID); got.Status != commerce.MandateRevocationPending {
		t.Errorf("after the refused confirmations: %s", got.Status)
	}
}

func TestMandates_AreScopedToTheirMerchant(t *testing.T) {
	f := newMandatesFixture(t)
	ctx := context.Background()
	m := f.approve(t, "mreq_1")
	r := f.mustRequest(t, "mreq_2")
	for _, other := range []commerceapp.MerchantPrincipal{shopA2, shopB} {
		if _, err := f.get.Run(ctx, other, m.ID); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s reading the mandate: %v", other.MerchantID, err)
		}
		if _, err := f.revoke.Run(ctx, other, m.ID); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s revoking the mandate: %v", other.MerchantID, err)
		}
		if _, err := f.getReq.Run(ctx, other, r.ID); !errors.Is(err, app.ErrNotFound) {
			t.Errorf("%s reading the request: %v", other.MerchantID, err)
		}
		if page, _ := f.list.Run(ctx, other, commerceapp.MandateFilter{}); len(page.Mandates) != 0 {
			t.Errorf("%s lists %d mandates", other.MerchantID, len(page.Mandates))
		}
	}
	if got, _ := f.get.Run(ctx, shopA1, m.ID); got.Status != commerce.MandateActive {
		t.Errorf("after the others' attempts: %s", got.Status)
	}
}

func TestListMandates_FiltersByCustomerAndStatus(t *testing.T) {
	f := newMandatesFixture(t)
	ctx := context.Background()
	m1 := f.approve(t, "mreq_1")
	f.clock.Advance(time.Second)
	m2 := f.approve(t, "mreq_2")
	f.revokeConfirmed(t, m1.ID)

	all, err := f.list.Run(ctx, shopA1, commerceapp.MandateFilter{CustomerID: f.ada.ID})
	if err != nil || len(all.Mandates) != 2 || all.Mandates[0].ID != m2.ID {
		t.Fatalf("all: %+v, %v", all.Mandates, err)
	}
	active, _ := f.list.Run(ctx, shopA1, commerceapp.MandateFilter{Status: commerce.MandateActive})
	revoked, _ := f.list.Run(ctx, shopA1, commerceapp.MandateFilter{Status: commerce.MandateRevoked})
	if len(active.Mandates) != 1 || active.Mandates[0].ID != m2.ID || len(revoked.Mandates) != 1 || revoked.Mandates[0].ID != m1.ID {
		t.Errorf("active %+v revoked %+v", active.Mandates, revoked.Mandates)
	}
	other, _ := f.list.Run(ctx, shopA1, commerceapp.MandateFilter{CustomerID: "cus_someone"})
	if len(other.Mandates) != 0 {
		t.Errorf("another customer's: %+v", other.Mandates)
	}
	page, _ := f.list.Run(ctx, shopA1, commerceapp.MandateFilter{Limit: 1})
	rest, _ := f.list.Run(ctx, shopA1, commerceapp.MandateFilter{Limit: 1, Cursor: page.NextCursor})
	if len(page.Mandates) != 1 || page.NextCursor == "" || len(rest.Mandates) != 1 || rest.Mandates[0].ID != m1.ID {
		t.Errorf("paging: %+v then %+v", page, rest)
	}
	var verr *app.ValidationError
	if _, err := f.list.Run(ctx, shopA1, commerceapp.MandateFilter{Status: "pending"}); !errors.As(err, &verr) {
		t.Errorf("unknown status: %v", err)
	}
}

// Terms may expire before the link does. Once they have, the page can't
// show them and nobody can approve them: that would make a mandate
// already expired.
func TestDecideConsent_TermsThatExpiredCannotBeApproved(t *testing.T) {
	f := newMandatesFixture(t)
	c := f.cmd("short")
	c.ExpiresAt = f.clock.Now().Add(10 * time.Minute)
	r, _, err := f.request.Run(context.Background(), shopA1, c)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(20 * time.Minute) // the link is still valid
	if _, err := f.view.Run(context.Background(), f.token()); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("viewing expired terms: %v", err)
	}
	if _, err := f.decide.Run(context.Background(), f.token(), r.Terms.Hash(), commerce.ConsentApproved); !errors.Is(err, app.ErrTokenInvalid) {
		t.Errorf("approving expired terms: %v", err)
	}
}
