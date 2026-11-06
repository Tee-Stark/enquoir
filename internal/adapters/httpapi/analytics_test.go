package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

// fixtureAnalytics answers with fixed numbers and remembers the org it
// was asked about.
type fixtureAnalytics struct{ lastOrg domain.OrgID }

func (a *fixtureAnalytics) Summary(ctx context.Context, org domain.OrgID, p app.Period) (app.SummaryCounts, error) {
	a.lastOrg = org
	p50 := int64(2400)
	return app.SummaryCounts{
		ByState:              map[string]int{"APPLIED": 3, "FAILED": 1},
		Volume:               []app.CurrencyVolume{{Currency: "USD", AppliedMinorUnits: 3150, AppliedCount: 3}},
		SubmissionsByOutcome: map[string]int{"started": 4, "deduplicated": 2},
		RailCallsByOutcome:   map[string]int{"ok": 9},
		TimeToAppliedCount:   3, TimeToAppliedP50MS: &p50,
	}, nil
}

func (a *fixtureAnalytics) Series(ctx context.Context, org domain.OrgID, q app.SeriesQuery) (app.SeriesData, error) {
	a.lastOrg = org
	start := time.Date(2026, 11, 5, 0, 0, 0, 0, q.Period.Location)
	next := start.AddDate(0, 0, 1)
	return app.SeriesData{Buckets: []time.Time{start, next}, Rows: []app.SeriesRow{{Bucket: next, Group: "APPLIED", Value: 2}}}, nil
}

func TestAnalytics_Summary(t *testing.T) {
	f := newAuthFixture(t, true)
	_, cookie := f.newOrg(t)
	rec := f.do(t, http.MethodGet, "/v1/analytics/summary?tz=Africa/Lagos", "", cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	var s struct {
		Period   struct{ TZ string }
		Payments struct {
			Total       int
			SuccessRate *float64 `json:"success_rate"`
		}
		Volume []struct {
			AppliedAmount string `json:"applied_amount"`
		}
		Submissions struct {
			DuplicatesPrevented int `json:"duplicates_prevented"`
		}
	}
	json.Unmarshal(rec.Body.Bytes(), &s)
	if s.Period.TZ != "Africa/Lagos" || s.Payments.Total != 4 || *s.Payments.SuccessRate != 0.75 ||
		s.Volume[0].AppliedAmount != "31.50" || s.Submissions.DuplicatesPrevented != 2 {
		t.Errorf("summary = %s", rec.Body)
	}
}

func TestAnalytics_TimeSeriesInTheCallersZone(t *testing.T) {
	f := newAuthFixture(t, true)
	_, cookie := f.newOrg(t)
	rec := f.do(t, http.MethodGet, "/v1/analytics/timeseries?metric=payments&group_by=state&tz=Asia/Kolkata&from=2026-11-04T18:30:00Z&to=2026-11-06T18:30:00Z", "", cookie)
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
	if len(ts.Series) != 1 || ts.Series[0].Group != "APPLIED" || *ts.Series[0].Points[0].Value != 0 || *ts.Series[0].Points[1].Value != 2 {
		t.Errorf("series = %s", rec.Body)
	}
}

func TestAnalytics_ValidationAndAccess(t *testing.T) {
	f := newAuthFixture(t, true)
	_, cookie := f.newOrg(t)
	for _, path := range []string{
		"/v1/analytics/timeseries",
		"/v1/analytics/timeseries?metric=payments&tz=Nowhere/Land",
		"/v1/analytics/timeseries?metric=payments&interval=fortnight",
		"/v1/analytics/summary?from=2026-11-06T00:00:00Z&to=2026-11-01T00:00:00Z",
	} {
		rec := f.do(t, http.MethodGet, path, "", cookie)
		if p := decodeProblem(t, rec); rec.Code != 400 || p.Code != "validation_failed" {
			t.Errorf("%s: %d %q", path, rec.Code, p.Code)
		}
	}
	if rec := f.do(t, http.MethodGet, "/v1/analytics/summary", ""); rec.Code != 401 {
		t.Errorf("summary without a session: %d", rec.Code)
	}
}
