//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"enquoir/internal/commerce"
	"enquoir/internal/commerce/commerceapp"
)

type commerceAnalyticsWorld struct {
	*checkoutWorld
	summary *commerceapp.GetCommerceSummary
	series  *commerceapp.GetCommerceTimeSeries
}

func newCommerceAnalyticsWorld(t *testing.T) *commerceAnalyticsWorld {
	t.Helper()
	w := newCheckoutWorld(t)
	clock := realClock{}
	return &commerceAnalyticsWorld{
		checkoutWorld: w,
		summary:       &commerceapp.GetCommerceSummary{Analytics: w.store, Clock: clock},
		series:        &commerceapp.GetCommerceTimeSeries{Analytics: w.store, Clock: clock},
	}
}

func TestCommerceAnalytics_Summary_StatusesVolumeAndRates(t *testing.T) {
	ctx := context.Background()
	w := newCommerceAnalyticsWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)

	paid := w.redeemed(t, m, "paid1", 5000)
	w.reporter(0).Run(ctx, w.merchant, paid.ID, rep(commerce.ReportDirect, commerce.ExecutionSucceeded, "pi_1", paid))

	failed := w.redeemed(t, m, "failed1", 3000)
	w.reporter(0).Run(ctx, w.merchant, failed.ID, rep(commerce.ReportDirect, commerce.ExecutionFailed, "", failed))

	// Over the per-transaction limit (5000) - denied by the mandate's own
	// evaluation, never authorized.
	denied, _, err := w.submit.Run(ctx, w.merchant, w.book(m, "denied1", 6000))
	if err != nil || denied.Status != commerce.CheckoutDenied {
		t.Fatalf("denied fixture: %+v %v", denied, err)
	}

	// Authorized, left unredeemed - still in progress.
	w.authorized(t, m, "auth1", 5000)

	s, err := w.summary.Run(ctx, w.merchant.OrgID, time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Checkouts != 4 || s.ByStatus["paid"] != 1 || s.ByStatus["payment_failed"] != 1 || s.ByStatus["denied"] != 1 || s.ByStatus["authorized"] != 1 {
		t.Fatalf("summary = %+v", s)
	}
	if s.InProgress != 1 || s.Denied != 1 {
		t.Errorf("in progress %d, denied %d; want 1, 1", s.InProgress, s.Denied)
	}
	if s.SuccessRate == nil || *s.SuccessRate != 0.5 {
		t.Errorf("success rate = %v, want 0.5 (1 paid of 2 settled)", s.SuccessRate)
	}
	if len(s.Volume) != 1 || s.Volume[0].Currency != "EUR" || s.Volume[0].PaidMinorUnits != 5000 || s.Volume[0].PaidCount != 1 {
		t.Errorf("volume = %+v, want one EUR entry of 5000/1 - only the paid execution, not the failed one", s.Volume)
	}
	if s.ActiveAgents != 1 {
		t.Errorf("active agents = %d, want 1", s.ActiveAgents)
	}
	if s.TimeToPaidCount != 1 {
		t.Errorf("time to paid count = %d, want 1", s.TimeToPaidCount)
	}
}

func TestCommerceAnalytics_Summary_ScopedByOrgNotMerchant(t *testing.T) {
	ctx := context.Background()
	w := newCommerceAnalyticsWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	paid := w.redeemed(t, m, "paid1", 5000)
	w.reporter(0).Run(ctx, w.merchant, paid.ID, rep(commerce.ReportDirect, commerce.ExecutionSucceeded, "pi_1", paid))

	otherOrg, _ := w.u.newMerchant(t, NewAccountStore(testPool(t)))
	s, err := w.summary.Run(ctx, otherOrg.OrgID, time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Checkouts != 0 || len(s.Volume) != 0 {
		t.Errorf("a sibling organisation's summary saw this org's data: %+v", s)
	}
}

// Unknown is not failed: once a later reconciliation resolves it to
// paid, it counts as recovered.
func TestCommerceAnalytics_Summary_RecoveredFromUnknown(t *testing.T) {
	ctx := context.Background()
	w := newCommerceAnalyticsWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	e := w.redeemed(t, m, "held1", 5000)
	if _, err := w.reporter(0).Run(ctx, w.merchant, e.ID, rep(commerce.ReportDirect, commerce.ExecutionUnknown, "", e)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.reporter(0).Run(ctx, w.merchant, e.ID, rep(commerce.ReportReconciliation, commerce.ExecutionSucceeded, "pi_2", e)); err != nil {
		t.Fatal(err)
	}

	s, err := w.summary.Run(ctx, w.merchant.OrgID, time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.RecoveredFromUnknown != 1 {
		t.Errorf("recovered from unknown = %d, want 1", s.RecoveredFromUnknown)
	}
}

func TestCommerceAnalytics_Series_ChecksOutGroupedByStatus(t *testing.T) {
	ctx := context.Background()
	w := newCommerceAnalyticsWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	w.authorized(t, m, "auth1", 5000)

	now := time.Now().UTC()
	from, to := now.Add(-24*time.Hour), now.Add(24*time.Hour)
	ts, err := w.series.Run(ctx, w.merchant.OrgID, "checkouts", "day", "status", from, to, "UTC")
	if err != nil {
		t.Fatal(err)
	}
	var authorizedCount float64
	for _, series := range ts.Series {
		if series.Group != "authorized" {
			continue
		}
		for _, p := range series.Points {
			if p.Value != nil {
				authorizedCount += *p.Value
			}
		}
	}
	if authorizedCount != 1 {
		t.Errorf("authorized count across buckets = %v, want 1", authorizedCount)
	}
}

func TestCommerceAnalytics_Series_PaidVolumeByCurrency(t *testing.T) {
	ctx := context.Background()
	w := newCommerceAnalyticsWorld(t)
	m := w.mandate(t, "m1", commerce.Autonomous)
	paid := w.redeemed(t, m, "paid1", 5000)
	w.reporter(0).Run(ctx, w.merchant, paid.ID, rep(commerce.ReportDirect, commerce.ExecutionSucceeded, "pi_1", paid))

	now := time.Now().UTC()
	ts, err := w.series.Run(ctx, w.merchant.OrgID, "paid_volume", "day", "", now.Add(-24*time.Hour), now.Add(24*time.Hour), "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if len(ts.Series) != 1 || ts.Series[0].Group != "EUR" || ts.Unit != "minor_units" {
		t.Fatalf("series = %+v", ts)
	}
	var total float64
	for _, p := range ts.Series[0].Points {
		if p.Value != nil {
			total += *p.Value
		}
	}
	if total != 5000 {
		t.Errorf("paid volume total = %v, want 5000", total)
	}
}
