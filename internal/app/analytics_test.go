package app_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/domain"
)

type stubAnalytics struct {
	counts    app.SummaryCounts
	data      app.SeriesData
	gotPeriod app.Period
	gotQuery  app.SeriesQuery
}

func (s *stubAnalytics) Summary(ctx context.Context, org domain.OrgID, p app.Period) (app.SummaryCounts, error) {
	s.gotPeriod = p
	return s.counts, nil
}

func (s *stubAnalytics) Series(ctx context.Context, org domain.OrgID, q app.SeriesQuery) (app.SeriesData, error) {
	s.gotQuery = q
	return s.data, nil
}

func TestGetSummary_DerivesRatesAndTotals(t *testing.T) {
	stub := &stubAnalytics{counts: app.SummaryCounts{
		ByState:              map[string]int{"APPLIED": 6, "FAILED": 2, "BLOCKED": 1, "IN_FLIGHT": 1, app.StateReceived: 1},
		SubmissionsByOutcome: map[string]int{"started": 10, "deduplicated": 4, "attached": 1, "invalid": 2},
		RailCallsByOutcome:   map[string]int{"ok": 20, "ambiguous": 1},
	}}
	s, err := (&app.GetSummary{Analytics: stub, Clock: apptest.NewClock()}).Run(context.Background(), testOrg, time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Payments != 11 || s.InProgress != 2 || s.BlockedByPolicy != 1 {
		t.Errorf("payments %d, in progress %d, blocked %d; want 11, 2, 1", s.Payments, s.InProgress, s.BlockedByPolicy)
	}
	if s.SuccessRate == nil || *s.SuccessRate != 0.75 {
		t.Errorf("success rate = %v, want 0.75 (6 applied of 8 settled; blocked is policy, not failure)", s.SuccessRate)
	}
	if s.Submissions != 17 || s.DuplicatesPrevented != 5 || s.RailCalls != 21 {
		t.Errorf("submissions %d, duplicates %d, rail calls %d; want 17, 5, 21", s.Submissions, s.DuplicatesPrevented, s.RailCalls)
	}
	if _, ok := s.ByState["UNKNOWN"]; !ok || s.ByState["UNKNOWN"] != 0 {
		t.Error("states with no payments are missing; charts need every state")
	}
}

func TestGetSummary_NoSettledPaymentsMeansNoRate(t *testing.T) {
	s, _ := (&app.GetSummary{Analytics: &stubAnalytics{}, Clock: apptest.NewClock()}).Run(context.Background(), testOrg, time.Time{}, time.Time{}, "")
	if s.SuccessRate != nil {
		t.Errorf("success rate = %v with nothing settled, want none", *s.SuccessRate)
	}
}

func TestGetSummary_ResolvesThePeriod(t *testing.T) {
	clock := apptest.NewClock()
	stub := &stubAnalytics{}
	use := &app.GetSummary{Analytics: stub, Clock: clock}

	use.Run(context.Background(), testOrg, time.Time{}, time.Time{}, "")
	if !stub.gotPeriod.To.Equal(clock.Now()) || stub.gotPeriod.To.Sub(stub.gotPeriod.From) != 30*24*time.Hour || stub.gotPeriod.Location.String() != "UTC" {
		t.Errorf("default period = %+v, want the last 30 days in UTC", stub.gotPeriod)
	}
	use.Run(context.Background(), testOrg, time.Time{}, time.Time{}, "Africa/Lagos")
	if stub.gotPeriod.Location.String() != "Africa/Lagos" {
		t.Errorf("location = %v, want Africa/Lagos", stub.gotPeriod.Location)
	}

	for _, tz := range []string{"Mars/Olympus", "Local", "+01:00"} {
		_, err := use.Run(context.Background(), testOrg, time.Time{}, time.Time{}, tz)
		var verr *app.ValidationError
		if !errors.As(err, &verr) || verr.Field != "tz" {
			t.Errorf("tz %q: err = %v, want a ValidationError on tz", tz, err)
		}
	}
}

func TestGetTimeSeries_Validates(t *testing.T) {
	now := apptest.NewClock().Now()
	cases := map[string]struct {
		metric, interval, groupBy string
		from                      time.Time
		field                     string
	}{
		"unknown metric":    {"profit", "day", "", time.Time{}, "metric"},
		"unknown interval":  {"payments", "fortnight", "", time.Time{}, "interval"},
		"group not allowed": {"rail_calls", "day", "currency", time.Time{}, "group_by"},
		"volume by state":   {"applied_volume", "day", "state", time.Time{}, "group_by"},
		"too many buckets":  {"payments", "hour", "", now.Add(-60 * 24 * time.Hour), "interval"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := (&app.GetTimeSeries{Analytics: &stubAnalytics{}, Clock: apptest.NewClock()}).
				Run(context.Background(), testOrg, tc.metric, tc.interval, tc.groupBy, tc.from, time.Time{}, "")
			var verr *app.ValidationError
			if !errors.As(err, &verr) || verr.Field != tc.field {
				t.Errorf("err = %v, want a ValidationError on %q", err, tc.field)
			}
		})
	}
}

// Every group gets a point in every bucket - zero for counts, none for
// time to APPLIED - so a chart never has to guess at gaps.
func TestGetTimeSeries_ZeroFillsEveryBucket(t *testing.T) {
	lagos, _ := time.LoadLocation("Africa/Lagos")
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, lagos) }
	stub := &stubAnalytics{data: app.SeriesData{
		Buckets: []time.Time{day(4), day(5), day(6)},
		Rows: []app.SeriesRow{
			{Bucket: day(4), Group: "APPLIED", Value: 3},
			{Bucket: day(6), Group: "APPLIED", Value: 1},
			{Bucket: day(5), Group: "FAILED", Value: 2},
		},
	}}
	ts, err := (&app.GetTimeSeries{Analytics: stub, Clock: apptest.NewClock()}).
		Run(context.Background(), testOrg, "payments", "day", "state", day(4), day(7), "Africa/Lagos")
	if err != nil {
		t.Fatal(err)
	}
	if ts.Unit != "count" || len(ts.Buckets) != 3 || len(ts.Series) != 2 || ts.Series[0].Group != "APPLIED" {
		t.Fatalf("series = %+v", ts)
	}
	applied := ts.Series[0].Points
	if *applied[0].Value != 3 || *applied[1].Value != 0 || *applied[2].Value != 1 {
		t.Errorf("APPLIED points = %v, %v, %v; want 3, 0, 1", *applied[0].Value, *applied[1].Value, *applied[2].Value)
	}
	if applied[1].Start.Location().String() != "Africa/Lagos" {
		t.Errorf("bucket starts are in %v, want the caller's zone", applied[1].Start.Location())
	}
	if stub.gotQuery.Period.Location.String() != "Africa/Lagos" || stub.gotQuery.GroupBy != "state" {
		t.Errorf("query = %+v", stub.gotQuery)
	}

	stub.data.Rows = []app.SeriesRow{{Bucket: day(5), Value: 1200}}
	ts, _ = (&app.GetTimeSeries{Analytics: stub, Clock: apptest.NewClock()}).
		Run(context.Background(), testOrg, "time_to_applied", "day", "", day(4), day(7), "Africa/Lagos")
	if p := ts.Series[0].Points; p[0].Value != nil || *p[1].Value != 1200 || ts.Series[0].Group != "all" {
		t.Errorf("time to applied: %+v; want no value where there is no data", p)
	}
}

func TestGetTimeSeries_VolumeIsAlwaysPerCurrency(t *testing.T) {
	stub := &stubAnalytics{}
	ts, err := (&app.GetTimeSeries{Analytics: stub, Clock: apptest.NewClock()}).
		Run(context.Background(), testOrg, "applied_volume", "", "", time.Time{}, time.Time{}, "")
	if err != nil || stub.gotQuery.GroupBy != "currency" || ts.Unit != "minor_units" || stub.gotQuery.Interval != "day" {
		t.Errorf("err %v, query %+v, unit %q", err, stub.gotQuery, ts.Unit)
	}
}
