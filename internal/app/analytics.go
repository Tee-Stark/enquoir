package app

import (
	"context"
	"fmt"
	"sort"
	"time"

	"enquoir/internal/domain"
)

// Period is a span of time, [From, To), read in Location: the zone
// buckets and calendar boundaries follow.
type Period struct {
	From, To time.Time
	Location *time.Location
}

// DefaultPeriodLength applies when a caller names no period.
const DefaultPeriodLength = 30 * 24 * time.Hour

// maxBuckets bounds a time series, so one request can't ask for a
// year of hourly buckets.
const maxBuckets = 1000

func resolvePeriod(from, to time.Time, tz string, clock Clock) (Period, error) {
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "Local" {
		return Period{}, &ValidationError{Field: "tz", Message: fmt.Sprintf("unknown time zone %q: use an IANA name such as Africa/Lagos", tz)}
	}
	if to.IsZero() {
		to = clock.Now()
	}
	if from.IsZero() {
		from = to.Add(-DefaultPeriodLength)
	}
	if !from.Before(to) {
		return Period{}, &ValidationError{Field: "from", Message: "from must be before to"}
	}
	return Period{From: from, To: to, Location: loc}, nil
}

// CurrencyVolume is applied payment volume in one currency. Amounts in
// different currencies are never added together.
type CurrencyVolume struct {
	Currency          domain.Currency
	AppliedMinorUnits int64
	AppliedCount      int
}

// Summary is an organisation's activity over a period.
type Summary struct {
	Period Period

	Payments             int            // created in the period
	ByState              map[string]int // every state, zeros included
	InProgress           int            // RECEIVED through UNKNOWN
	SuccessRate          *float64       // APPLIED / (APPLIED + FAILED); nil when neither
	BlockedByPolicy      int
	RecoveredFromUnknown int // passed through UNKNOWN and still reached APPLIED or FAILED
	Volume               []CurrencyVolume

	Submissions          int
	SubmissionsByOutcome map[string]int
	DuplicatesPrevented  int // submissions attached or deduplicated
	ActiveAgents         int

	RailCalls          int
	RailCallsByOutcome map[string]int

	TimeToAppliedCount int
	TimeToAppliedP50MS *int64
	TimeToAppliedP95MS *int64
}

// SummaryCounts is what an AnalyticsReader computes for a summary.
type SummaryCounts struct {
	ByState              map[string]int
	RecoveredFromUnknown int
	Volume               []CurrencyVolume
	SubmissionsByOutcome map[string]int
	ActiveAgents         int
	RailCallsByOutcome   map[string]int
	TimeToAppliedCount   int
	TimeToAppliedP50MS   *int64
	TimeToAppliedP95MS   *int64
}

// Time-series metrics.
const (
	MetricPayments            = "payments"             // payments created
	MetricAppliedVolume       = "applied_volume"       // minor units of APPLIED payments, per currency
	MetricSubmissions         = "submissions"          // agent submissions
	MetricDuplicatesPrevented = "duplicates_prevented" // submissions attached or deduplicated
	MetricRailCalls           = "rail_calls"           // Airwallex calls
	MetricTimeToApplied       = "time_to_applied"      // median milliseconds to APPLIED
)

// Units a series' values are in.
const (
	UnitCount        = "count"
	UnitMinorUnits   = "minor_units"
	UnitMilliseconds = "milliseconds"
)

type metricSpec struct {
	unit     string
	groupBys map[string]bool // "" = ungrouped
	forced   string          // a group-by always applied
}

var metrics = map[string]metricSpec{
	MetricPayments:            {UnitCount, set("", "state", "agent", "currency"), ""},
	MetricAppliedVolume:       {UnitMinorUnits, set("currency"), "currency"},
	MetricSubmissions:         {UnitCount, set("", "outcome", "agent"), ""},
	MetricDuplicatesPrevented: {UnitCount, set("", "agent"), ""},
	MetricRailCalls:           {UnitCount, set("", "outcome", "operation"), ""},
	MetricTimeToApplied:       {UnitMilliseconds, set(""), ""},
}

var intervals = map[string]time.Duration{"hour": time.Hour, "day": 24 * time.Hour, "week": 7 * 24 * time.Hour, "month": 31 * 24 * time.Hour}

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// SeriesQuery asks for one metric over a period, bucketed by interval.
type SeriesQuery struct {
	Metric   string
	Interval string // hour, day, week (ISO, from Monday) or month
	GroupBy  string
	Period   Period
}

// SeriesRow is one non-empty (bucket, group) value.
type SeriesRow struct {
	Bucket time.Time
	Group  string
	Value  float64
}

// SeriesData is what an AnalyticsReader returns for a series: every
// bucket start in the period (in the period's zone), and the rows that
// have data.
type SeriesData struct {
	Buckets []time.Time
	Rows    []SeriesRow
}

// AnalyticsReader computes analytics, scoped by organisation.
type AnalyticsReader interface {
	Summary(ctx context.Context, org domain.OrgID, p Period) (SummaryCounts, error)
	Series(ctx context.Context, org domain.OrgID, q SeriesQuery) (SeriesData, error)
}

// GetSummary is the analytics-summary use case.
type GetSummary struct {
	Analytics AnalyticsReader
	Clock     Clock
}

func (g *GetSummary) Run(ctx context.Context, org domain.OrgID, from, to time.Time, tz string) (Summary, error) {
	p, err := resolvePeriod(from, to, tz, g.Clock)
	if err != nil {
		return Summary{}, err
	}
	c, err := g.Analytics.Summary(ctx, org, p)
	if err != nil {
		return Summary{}, err
	}
	s := Summary{
		Period: p, ByState: map[string]int{}, Volume: c.Volume, RecoveredFromUnknown: c.RecoveredFromUnknown,
		SubmissionsByOutcome: map[string]int{}, ActiveAgents: c.ActiveAgents, RailCallsByOutcome: map[string]int{},
		TimeToAppliedCount: c.TimeToAppliedCount, TimeToAppliedP50MS: c.TimeToAppliedP50MS, TimeToAppliedP95MS: c.TimeToAppliedP95MS,
	}
	for st := range knownStates {
		s.ByState[st] = c.ByState[st]
		s.Payments += c.ByState[st]
	}
	for _, st := range []string{StateReceived, "PROPOSED", string(domain.AWAITING_APPROVAL), "AUTHORIZED", "IN_FLIGHT", "UNKNOWN"} {
		s.InProgress += s.ByState[st]
	}
	s.BlockedByPolicy = s.ByState["BLOCKED"]
	if settled := s.ByState["APPLIED"] + s.ByState["FAILED"]; settled > 0 {
		rate := float64(s.ByState["APPLIED"]) / float64(settled)
		s.SuccessRate = &rate
	}
	for _, o := range []AttemptOutcome{OutcomeStarted, OutcomeAttached, OutcomeDeduplicated, OutcomeInvalid, OutcomePayeeUnknown, OutcomeError, OutcomePolicyDenied} {
		n := c.SubmissionsByOutcome[string(o)]
		s.SubmissionsByOutcome[string(o)] = n
		s.Submissions += n
	}
	s.DuplicatesPrevented = s.SubmissionsByOutcome[string(OutcomeAttached)] + s.SubmissionsByOutcome[string(OutcomeDeduplicated)]
	for _, o := range []AttemptOutcome{OutcomeOK, OutcomeNotFound, OutcomeRejected, OutcomeAmbiguous, OutcomeDuplicateTransfers, OutcomeError, OutcomeInProgress, OutcomeInterrupted} {
		n := c.RailCallsByOutcome[string(o)]
		s.RailCallsByOutcome[string(o)] = n
		s.RailCalls += n
	}
	if s.Volume == nil {
		s.Volume = []CurrencyVolume{}
	}
	return s, nil
}

// SeriesPoint is one bucket's value; nil means no data (for a metric
// where zero would be misleading, such as time to APPLIED).
type SeriesPoint struct {
	Start time.Time
	Value *float64
}

// Series is one line on a chart.
type Series struct {
	Group  string // "all" when ungrouped
	Points []SeriesPoint
}

// TimeSeries is a metric over a period, every bucket present.
type TimeSeries struct {
	Query   SeriesQuery
	Unit    string
	Buckets []time.Time
	Series  []Series
}

// GetTimeSeries is the analytics time-series use case.
type GetTimeSeries struct {
	Analytics AnalyticsReader
	Clock     Clock
}

func (g *GetTimeSeries) Run(ctx context.Context, org domain.OrgID, metric, interval, groupBy string, from, to time.Time, tz string) (TimeSeries, error) {
	spec, ok := metrics[metric]
	if !ok {
		return TimeSeries{}, &ValidationError{Field: "metric", Message: fmt.Sprintf("unknown metric %q", metric)}
	}
	if interval == "" {
		interval = "day"
	}
	step, ok := intervals[interval]
	if !ok {
		return TimeSeries{}, &ValidationError{Field: "interval", Message: fmt.Sprintf("unknown interval %q: use hour, day, week or month", interval)}
	}
	if groupBy == "" {
		groupBy = spec.forced
	}
	if !spec.groupBys[groupBy] {
		return TimeSeries{}, &ValidationError{Field: "group_by", Message: fmt.Sprintf("%s cannot be grouped by %q", metric, groupBy)}
	}
	p, err := resolvePeriod(from, to, tz, g.Clock)
	if err != nil {
		return TimeSeries{}, err
	}
	if p.To.Sub(p.From)/step > maxBuckets {
		return TimeSeries{}, &ValidationError{Field: "interval", Message: fmt.Sprintf("too many buckets: at most %d per series; use a longer interval or a shorter period", maxBuckets)}
	}

	q := SeriesQuery{Metric: metric, Interval: interval, GroupBy: groupBy, Period: p}
	data, err := g.Analytics.Series(ctx, org, q)
	if err != nil {
		return TimeSeries{}, err
	}
	return assembleSeries(q, spec.unit, data), nil
}

// assembleSeries gives every group a point in every bucket: zero for
// counts and sums, nil (no data) for time to APPLIED.
func assembleSeries(q SeriesQuery, unit string, data SeriesData) TimeSeries {
	ts := TimeSeries{Query: q, Unit: unit, Buckets: make([]time.Time, len(data.Buckets))}
	index := map[int64]int{}
	for i, b := range data.Buckets {
		ts.Buckets[i] = b.In(q.Period.Location)
		index[b.UnixNano()] = i
	}

	values := map[string][]*float64{}
	if q.GroupBy == "" {
		values["all"] = make([]*float64, len(data.Buckets))
	}
	for _, r := range data.Rows {
		g := r.Group
		if q.GroupBy == "" {
			g = "all"
		}
		if values[g] == nil {
			values[g] = make([]*float64, len(data.Buckets))
		}
		if i, ok := index[r.Bucket.UnixNano()]; ok {
			v := r.Value
			values[g][i] = &v
		}
	}

	groups := make([]string, 0, len(values))
	for g := range values {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	for _, g := range groups {
		s := Series{Group: g, Points: make([]SeriesPoint, len(ts.Buckets))}
		for i, b := range ts.Buckets {
			v := values[g][i]
			if v == nil && unit != UnitMilliseconds {
				zero := 0.0
				v = &zero
			}
			s.Points[i] = SeriesPoint{Start: b, Value: v}
		}
		ts.Series = append(ts.Series, s)
	}
	return ts
}
