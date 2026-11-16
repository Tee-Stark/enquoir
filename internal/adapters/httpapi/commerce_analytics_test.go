package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

// fixtureCommerceAnalytics answers with fixed numbers and remembers
// the org it was asked about - mirrors fixtureAnalytics.
type fixtureCommerceAnalytics struct{ lastOrg domain.OrgID }

func (a *fixtureCommerceAnalytics) Summary(ctx context.Context, org domain.OrgID, p app.Period, now time.Time) (commerceapp.CommerceSummaryCounts, error) {
	a.lastOrg = org
	p50 := int64(1800)
	return commerceapp.CommerceSummaryCounts{
		ByStatus:        map[string]int{"paid": 3, "payment_failed": 1},
		Volume:          []commerceapp.CommercePaidVolume{{Currency: "USD", PaidMinorUnits: 3150, PaidCount: 3}},
		ActiveAgents:    2,
		TimeToPaidCount: 3, TimeToPaidP50MS: &p50,
	}, nil
}

func (a *fixtureCommerceAnalytics) Series(ctx context.Context, org domain.OrgID, q app.SeriesQuery, now time.Time) (app.SeriesData, error) {
	a.lastOrg = org
	start := time.Date(2026, 11, 5, 0, 0, 0, 0, q.Period.Location)
	next := start.AddDate(0, 0, 1)
	return app.SeriesData{Buckets: []time.Time{start, next}, Rows: []app.SeriesRow{{Bucket: next, Group: "paid", Value: 2}}}, nil
}

func TestCommerceAnalytics_Summary(t *testing.T) {
	f := newAuthFixture(t, true)
	_, cookie := f.newOrg(t)
	rec := f.do(t, http.MethodGet, "/v1/commerce-analytics/summary?tz=Africa/Lagos", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var s struct {
		Period    struct{ TZ string }
		Checkouts struct {
			Total       int
			SuccessRate *float64 `json:"success_rate"`
		}
		Volume []struct {
			PaidAmount string `json:"paid_amount"`
		}
		ActiveAgents int `json:"active_agents"`
	}
	json.Unmarshal(rec.Body.Bytes(), &s)
	if s.Period.TZ != "Africa/Lagos" || s.Checkouts.Total != 4 || *s.Checkouts.SuccessRate != 0.75 ||
		s.Volume[0].PaidAmount != "31.50" || s.ActiveAgents != 2 {
		t.Errorf("summary = %s", rec.Body)
	}
}

func TestCommerceAnalytics_TimeSeriesInTheCallersZone(t *testing.T) {
	f := newAuthFixture(t, true)
	_, cookie := f.newOrg(t)
	rec := f.do(t, http.MethodGet, "/v1/commerce-analytics/timeseries?metric=checkouts&group_by=status&tz=Asia/Kolkata&from=2026-11-04T18:30:00Z&to=2026-11-06T18:30:00Z", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var ts struct {
		Unit    string
		Buckets []string
		Series  []struct {
			Group  string
			Points []struct{ Value *float64 }
		}
	}
	json.Unmarshal(rec.Body.Bytes(), &ts)
	if ts.Unit != "count" || len(ts.Buckets) != 2 || ts.Buckets[0] != "2026-11-05T00:00:00+05:30" {
		t.Errorf("buckets = %v, want local midnights with their offset", ts.Buckets)
	}
	if len(ts.Series) != 1 || ts.Series[0].Group != "paid" || *ts.Series[0].Points[0].Value != 0 || *ts.Series[0].Points[1].Value != 2 {
		t.Errorf("series = %s", rec.Body)
	}
}

func TestCommerceAnalytics_ValidationAndAccess(t *testing.T) {
	f := newAuthFixture(t, true)
	_, cookie := f.newOrg(t)
	for _, path := range []string{
		"/v1/commerce-analytics/timeseries",
		"/v1/commerce-analytics/timeseries?metric=checkouts&tz=Nowhere/Land",
		"/v1/commerce-analytics/timeseries?metric=checkouts&interval=fortnight",
		"/v1/commerce-analytics/summary?from=2026-11-06T00:00:00Z&to=2026-11-01T00:00:00Z",
	} {
		rec := f.do(t, http.MethodGet, path, "", cookie)
		if p := decodeProblem(t, rec); rec.Code != 400 || p.Code != "validation_failed" {
			t.Errorf("%s: %d %q", path, rec.Code, p.Code)
		}
	}
	if rec := f.do(t, http.MethodGet, "/v1/commerce-analytics/summary", ""); rec.Code != 401 {
		t.Errorf("summary without a session: %d", rec.Code)
	}
}
