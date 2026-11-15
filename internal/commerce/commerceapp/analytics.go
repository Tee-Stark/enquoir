package commerceapp

import (
	"context"
	"fmt"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce"
	"enquoir/internal/domain"
)

// CommercePaidVolume is paid execution volume in one currency. Amounts
// in different currencies are never added together.
type CommercePaidVolume struct {
	Currency       domain.Currency
	PaidMinorUnits int64
	PaidCount      int
}

// CommerceSummary is an organisation's commerce activity over a
// period - org-wide, across every merchant integration it owns, the
// same scope org_activity.go already reads at. Mirrors app.Summary's
// shape, but commerce has no submissions/rail-calls/duplicates-
// prevented concept (a checkout is its own submission; the merchant's
// PSP calls aren't tracked by Enquoir), so those sections are simply
// absent rather than forced into a misleading mapping.
type CommerceSummary struct {
	Period app.Period

	Checkouts            int
	ByStatus             map[string]int // every status, zeros included
	InProgress           int            // authorized, awaiting_customer_approval, redeemed
	Denied               int            // the mandate/policy engine's own decision - same role BLOCKED plays for treasury
	SuccessRate          *float64       // paid / (paid + payment_failed); nil when neither
	RecoveredFromUnknown int            // its execution passed through unknown and still reached paid or payment_failed

	Volume []CommercePaidVolume

	ActiveAgents int

	TimeToPaidCount int
	TimeToPaidP50MS *int64
	TimeToPaidP95MS *int64
}

// CommerceSummaryCounts is what a CommerceAnalyticsReader computes for
// a summary.
type CommerceSummaryCounts struct {
	ByStatus             map[string]int
	RecoveredFromUnknown int
	Volume               []CommercePaidVolume
	ActiveAgents         int
	TimeToPaidCount      int
	TimeToPaidP50MS      *int64
	TimeToPaidP95MS      *int64
}

// Time-series metrics.
const (
	MetricCheckouts  = "checkouts"    // checkouts created
	MetricPaidVolume = "paid_volume"  // minor units of succeeded executions, per currency
	MetricTimeToPaid = "time_to_paid" // median milliseconds, execution created (= redemption instant) to finished, succeeded only
)

type commerceMetricSpec struct {
	unit     string
	groupBys map[string]bool // "" = ungrouped
	forced   string          // a group-by always applied
}

var commerceMetrics = map[string]commerceMetricSpec{
	MetricCheckouts:  {app.UnitCount, commerceSet("", "status", "agent", "currency"), ""},
	MetricPaidVolume: {app.UnitMinorUnits, commerceSet("currency"), "currency"},
	MetricTimeToPaid: {app.UnitMilliseconds, commerceSet(""), ""},
}

func commerceSet(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// CommerceAnalyticsReader computes commerce analytics, scoped by
// organisation. Kept separate from CommerceOrgReader (entity listing)
// the same way app.AnalyticsReader is kept separate from Treasury's
// history readers.
type CommerceAnalyticsReader interface {
	Summary(ctx context.Context, org domain.OrgID, p app.Period) (CommerceSummaryCounts, error)
	Series(ctx context.Context, org domain.OrgID, q app.SeriesQuery) (app.SeriesData, error)
}

// GetCommerceSummary is the commerce analytics-summary use case.
type GetCommerceSummary struct {
	Analytics CommerceAnalyticsReader
	Clock     app.Clock
}

func (g *GetCommerceSummary) Run(ctx context.Context, org domain.OrgID, from, to time.Time, tz string) (CommerceSummary, error) {
	p, err := app.ResolvePeriod(from, to, tz, g.Clock)
	if err != nil {
		return CommerceSummary{}, err
	}
	c, err := g.Analytics.Summary(ctx, org, p)
	if err != nil {
		return CommerceSummary{}, err
	}
	s := CommerceSummary{
		Period: p, ByStatus: map[string]int{}, Volume: c.Volume, RecoveredFromUnknown: c.RecoveredFromUnknown,
		ActiveAgents: c.ActiveAgents, TimeToPaidCount: c.TimeToPaidCount, TimeToPaidP50MS: c.TimeToPaidP50MS, TimeToPaidP95MS: c.TimeToPaidP95MS,
	}
	for st := range checkoutStatuses {
		s.ByStatus[string(st)] = c.ByStatus[string(st)]
		s.Checkouts += c.ByStatus[string(st)]
	}
	for _, st := range []commerce.CheckoutStatus{commerce.CheckoutAuthorized, commerce.CheckoutAwaitingApproval, commerce.CheckoutRedeemed} {
		s.InProgress += s.ByStatus[string(st)]
	}
	s.Denied = s.ByStatus[string(commerce.CheckoutDenied)]
	if settled := s.ByStatus[string(commerce.CheckoutPaid)] + s.ByStatus[string(commerce.CheckoutPaymentFailed)]; settled > 0 {
		rate := float64(s.ByStatus[string(commerce.CheckoutPaid)]) / float64(settled)
		s.SuccessRate = &rate
	}
	if s.Volume == nil {
		s.Volume = []CommercePaidVolume{}
	}
	return s, nil
}

// GetCommerceTimeSeries is the commerce analytics time-series use case.
type GetCommerceTimeSeries struct {
	Analytics CommerceAnalyticsReader
	Clock     app.Clock
}

func (g *GetCommerceTimeSeries) Run(ctx context.Context, org domain.OrgID, metric, interval, groupBy string, from, to time.Time, tz string) (app.TimeSeries, error) {
	spec, ok := commerceMetrics[metric]
	if !ok {
		return app.TimeSeries{}, &app.ValidationError{Field: "metric", Message: fmt.Sprintf("unknown metric %q", metric)}
	}
	if interval == "" {
		interval = "day"
	}
	step, ok := app.Intervals[interval]
	if !ok {
		return app.TimeSeries{}, &app.ValidationError{Field: "interval", Message: fmt.Sprintf("unknown interval %q: use hour, day, week or month", interval)}
	}
	if groupBy == "" {
		groupBy = spec.forced
	}
	if !spec.groupBys[groupBy] {
		return app.TimeSeries{}, &app.ValidationError{Field: "group_by", Message: fmt.Sprintf("%s cannot be grouped by %q", metric, groupBy)}
	}
	p, err := app.ResolvePeriod(from, to, tz, g.Clock)
	if err != nil {
		return app.TimeSeries{}, err
	}
	if p.To.Sub(p.From)/step > app.MaxBuckets {
		return app.TimeSeries{}, &app.ValidationError{Field: "interval", Message: fmt.Sprintf("too many buckets: at most %d per series; use a longer interval or a shorter period", app.MaxBuckets)}
	}

	q := app.SeriesQuery{Metric: metric, Interval: interval, GroupBy: groupBy, Period: p}
	data, err := g.Analytics.Series(ctx, org, q)
	if err != nil {
		return app.TimeSeries{}, err
	}
	return app.AssembleSeries(q, spec.unit, data), nil
}
