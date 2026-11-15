package commerceapp_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/app/apptest"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

const testOrg domain.OrgID = "org_test"

type stubCommerceAnalytics struct {
	counts    commerceapp.CommerceSummaryCounts
	data      app.SeriesData
	gotPeriod app.Period
	gotQuery  app.SeriesQuery
}

func (s *stubCommerceAnalytics) Summary(ctx context.Context, org domain.OrgID, p app.Period) (commerceapp.CommerceSummaryCounts, error) {
	s.gotPeriod = p
	return s.counts, nil
}

func (s *stubCommerceAnalytics) Series(ctx context.Context, org domain.OrgID, q app.SeriesQuery) (app.SeriesData, error) {
	s.gotQuery = q
	return s.data, nil
}

func TestGetCommerceSummary_DerivesRatesAndTotals(t *testing.T) {
	stub := &stubCommerceAnalytics{counts: commerceapp.CommerceSummaryCounts{
		ByStatus: map[string]int{"paid": 6, "payment_failed": 2, "denied": 1, "authorized": 1, "awaiting_customer_approval": 1},
	}}
	s, err := (&commerceapp.GetCommerceSummary{Analytics: stub, Clock: apptest.NewClock()}).Run(context.Background(), testOrg, time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Checkouts != 11 || s.InProgress != 2 || s.Denied != 1 {
		t.Errorf("checkouts %d, in progress %d, denied %d; want 11, 2, 1", s.Checkouts, s.InProgress, s.Denied)
	}
	if s.SuccessRate == nil || *s.SuccessRate != 0.75 {
		t.Errorf("success rate = %v, want 0.75 (6 paid of 8 settled; denied is a decision, not a payment failure)", s.SuccessRate)
	}
	if _, ok := s.ByStatus["revoked"]; !ok || s.ByStatus["revoked"] != 0 {
		t.Error("statuses with no checkouts are missing; charts need every status")
	}
}

// redeemed counts as in progress (its payment hasn't settled yet),
// matching awaiting_customer_approval also doing so.
func TestGetCommerceSummary_RedeemedCountsAsInProgress(t *testing.T) {
	stub := &stubCommerceAnalytics{counts: commerceapp.CommerceSummaryCounts{
		ByStatus: map[string]int{"paid": 1, "redeemed": 3},
	}}
	s, err := (&commerceapp.GetCommerceSummary{Analytics: stub, Clock: apptest.NewClock()}).Run(context.Background(), testOrg, time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.ByStatus["redeemed"] != 3 || s.InProgress != 3 || s.Checkouts != 4 {
		t.Errorf("redeemed %d, in progress %d, checkouts %d; want 3, 3, 4", s.ByStatus["redeemed"], s.InProgress, s.Checkouts)
	}
}

func TestGetCommerceSummary_NoSettledCheckoutsMeansNoRate(t *testing.T) {
	s, _ := (&commerceapp.GetCommerceSummary{Analytics: &stubCommerceAnalytics{}, Clock: apptest.NewClock()}).Run(context.Background(), testOrg, time.Time{}, time.Time{}, "")
	if s.SuccessRate != nil {
		t.Errorf("success rate = %v with nothing settled, want none", *s.SuccessRate)
	}
}

func TestGetCommerceSummary_VolumeIsNeverNilAndResolvesThePeriod(t *testing.T) {
	clock := apptest.NewClock()
	stub := &stubCommerceAnalytics{}
	use := &commerceapp.GetCommerceSummary{Analytics: stub, Clock: clock}

	s, err := use.Run(context.Background(), testOrg, time.Time{}, time.Time{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Volume == nil {
		t.Error("volume should be an empty slice, never nil - the frontend ranges over it unconditionally")
	}
	if !stub.gotPeriod.To.Equal(clock.Now()) || stub.gotPeriod.To.Sub(stub.gotPeriod.From) != 30*24*time.Hour || stub.gotPeriod.Location.String() != "UTC" {
		t.Errorf("default period = %+v, want the last 30 days in UTC", stub.gotPeriod)
	}

	_, err = use.Run(context.Background(), testOrg, time.Time{}, time.Time{}, "Mars/Olympus")
	var verr *app.ValidationError
	if !errors.As(err, &verr) || verr.Field != "tz" {
		t.Errorf("err = %v, want a ValidationError on tz", err)
	}
}

func TestGetCommerceTimeSeries_Validates(t *testing.T) {
	now := apptest.NewClock().Now()
	cases := map[string]struct {
		metric, interval, groupBy string
		from                      time.Time
		field                     string
	}{
		"unknown metric":    {"profit", "day", "", time.Time{}, "metric"},
		"unknown interval":  {"checkouts", "fortnight", "", time.Time{}, "interval"},
		"group not allowed": {"time_to_paid", "day", "currency", time.Time{}, "group_by"},
		"volume by status":  {"paid_volume", "day", "status", time.Time{}, "group_by"},
		"too many buckets":  {"checkouts", "hour", "", now.Add(-60 * 24 * time.Hour), "interval"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := (&commerceapp.GetCommerceTimeSeries{Analytics: &stubCommerceAnalytics{}, Clock: apptest.NewClock()}).
				Run(context.Background(), testOrg, tc.metric, tc.interval, tc.groupBy, tc.from, time.Time{}, "")
			var verr *app.ValidationError
			if !errors.As(err, &verr) || verr.Field != tc.field {
				t.Errorf("err = %v, want a ValidationError on %q", err, tc.field)
			}
		})
	}
}

func TestGetCommerceTimeSeries_ZeroFillsEveryBucket(t *testing.T) {
	lagos, _ := time.LoadLocation("Africa/Lagos")
	day := func(d int) time.Time { return time.Date(2026, 11, d, 0, 0, 0, 0, lagos) }
	stub := &stubCommerceAnalytics{data: app.SeriesData{
		Buckets: []time.Time{day(4), day(5), day(6)},
		Rows: []app.SeriesRow{
			{Bucket: day(4), Group: "paid", Value: 3},
			{Bucket: day(6), Group: "paid", Value: 1},
			{Bucket: day(5), Group: "denied", Value: 2},
		},
	}}
	ts, err := (&commerceapp.GetCommerceTimeSeries{Analytics: stub, Clock: apptest.NewClock()}).
		Run(context.Background(), testOrg, "checkouts", "day", "status", day(4), day(7), "Africa/Lagos")
	if err != nil {
		t.Fatal(err)
	}
	if ts.Unit != "count" || len(ts.Buckets) != 3 || len(ts.Series) != 2 || ts.Series[0].Group != "denied" {
		t.Fatalf("series = %+v", ts)
	}
	paid := ts.Series[1].Points
	if *paid[0].Value != 3 || *paid[1].Value != 0 || *paid[2].Value != 1 {
		t.Errorf("paid points = %v, %v, %v; want 3, 0, 1", *paid[0].Value, *paid[1].Value, *paid[2].Value)
	}
	if stub.gotQuery.Period.Location.String() != "Africa/Lagos" || stub.gotQuery.GroupBy != "status" {
		t.Errorf("query = %+v", stub.gotQuery)
	}

	stub.data.Rows = []app.SeriesRow{{Bucket: day(5), Value: 1200}}
	ts, _ = (&commerceapp.GetCommerceTimeSeries{Analytics: stub, Clock: apptest.NewClock()}).
		Run(context.Background(), testOrg, "time_to_paid", "day", "", day(4), day(7), "Africa/Lagos")
	if p := ts.Series[0].Points; p[0].Value != nil || *p[1].Value != 1200 || ts.Series[0].Group != "all" {
		t.Errorf("time to paid: %+v; want no value where there is no data", p)
	}
}

func TestGetCommerceTimeSeries_PaidVolumeIsAlwaysPerCurrency(t *testing.T) {
	stub := &stubCommerceAnalytics{}
	ts, err := (&commerceapp.GetCommerceTimeSeries{Analytics: stub, Clock: apptest.NewClock()}).
		Run(context.Background(), testOrg, "paid_volume", "", "", time.Time{}, time.Time{}, "")
	if err != nil || stub.gotQuery.GroupBy != "currency" || ts.Unit != "minor_units" || stub.gotQuery.Interval != "day" {
		t.Errorf("err %v, query %+v, unit %q", err, stub.gotQuery, ts.Unit)
	}
}
