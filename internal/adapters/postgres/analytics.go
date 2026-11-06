package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

var _ app.AnalyticsReader = (*AnalyticsReader)(nil)

// AnalyticsReader computes analytics in SQL. Calendar buckets are made
// by Postgres in the caller's zone, so days follow local midnight
// across daylight-saving changes.
type AnalyticsReader struct {
	pool *pgxpool.Pool
}

func NewAnalyticsReader(pool *pgxpool.Pool) *AnalyticsReader { return &AnalyticsReader{pool: pool} }

func (r *AnalyticsReader) countBy(ctx context.Context, sql string, args ...any) (map[string]int, error) {
	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}

func (r *AnalyticsReader) Summary(ctx context.Context, org domain.OrgID, p app.Period) (app.SummaryCounts, error) {
	var c app.SummaryCounts
	var err error
	args := []any{string(org), p.From, p.To}
	const inPeriod = `org_id = $1 AND created_at >= $2 AND created_at < $3`

	if c.ByState, err = r.countBy(ctx, `SELECT COALESCE(state, 'RECEIVED'), count(*) FROM intents WHERE `+inPeriod+` GROUP BY 1`, args...); err != nil {
		return c, fmt.Errorf("counting payments: %w", err)
	}
	if err = r.pool.QueryRow(ctx, `
		SELECT count(*) FROM intents i WHERE i.`+inPeriod+` AND i.state IN ('APPLIED', 'FAILED')
		  AND EXISTS (SELECT 1 FROM intent_events e WHERE e.org_id = i.org_id AND e.intent_key = i.key AND e.type = 'UNKNOWN')`,
		args...).Scan(&c.RecoveredFromUnknown); err != nil {
		return c, fmt.Errorf("counting recoveries: %w", err)
	}

	rows, err := r.pool.Query(ctx, `SELECT currency, sum(minor_units), count(*) FROM intents WHERE `+inPeriod+` AND state = 'APPLIED' GROUP BY currency ORDER BY currency`, args...)
	if err != nil {
		return c, fmt.Errorf("summing volume: %w", err)
	}
	for rows.Next() {
		var v app.CurrencyVolume
		var cur string
		if err := rows.Scan(&cur, &v.AppliedMinorUnits, &v.AppliedCount); err != nil {
			rows.Close()
			return c, err
		}
		v.Currency = domain.Currency(cur)
		c.Volume = append(c.Volume, v)
	}
	rows.Close()

	const attemptsInPeriod = `a.org_id = $1 AND a.started_at >= $2 AND a.started_at < $3`
	if c.SubmissionsByOutcome, err = r.countBy(ctx, `SELECT a.outcome, count(*) FROM attempts a WHERE `+attemptsInPeriod+` AND a.kind = 'agent_submission' GROUP BY 1`, args...); err != nil {
		return c, fmt.Errorf("counting submissions: %w", err)
	}
	if err = r.pool.QueryRow(ctx, `SELECT count(DISTINCT a.agent_id) FROM attempts a WHERE `+attemptsInPeriod+` AND a.kind = 'agent_submission'`, args...).Scan(&c.ActiveAgents); err != nil {
		return c, fmt.Errorf("counting agents: %w", err)
	}
	if c.RailCallsByOutcome, err = r.countBy(ctx, `SELECT `+effectiveOutcome+`, count(*) FROM attempts a WHERE `+attemptsInPeriod+` AND a.kind = 'rail_call' GROUP BY 1`, args...); err != nil {
		return c, fmt.Errorf("counting rail calls: %w", err)
	}

	var p50, p95 *float64
	if err = r.pool.QueryRow(ctx, `
		SELECT count(*), percentile_cont(0.5) WITHIN GROUP (ORDER BY ms), percentile_cont(0.95) WITHIN GROUP (ORDER BY ms)
		FROM (SELECT extract(epoch FROM state_changed_at - created_at) * 1000 AS ms
		      FROM intents WHERE `+inPeriod+` AND state = 'APPLIED') t`, args...).Scan(&c.TimeToAppliedCount, &p50, &p95); err != nil {
		return c, fmt.Errorf("measuring time to applied: %w", err)
	}
	c.TimeToAppliedP50MS, c.TimeToAppliedP95MS = roundMS(p50), roundMS(p95)
	return c, nil
}

func roundMS(v *float64) *int64 {
	if v == nil {
		return nil
	}
	ms := int64(*v + 0.5)
	return &ms
}

// seriesSource is how one metric reads: the table, the time it is
// bucketed by, extra conditions, the value, and the group expressions.
type seriesSource struct {
	from, timeCol, cond, value string
	groups                     map[string]string
}

var seriesSources = map[string]seriesSource{
	app.MetricPayments: {
		from: "intents i", timeCol: "i.created_at", cond: "TRUE", value: "count(*)",
		groups: map[string]string{"state": "COALESCE(i.state, 'RECEIVED')", "agent": "COALESCE(i.agent_id, 'none')", "currency": "i.currency"},
	},
	app.MetricAppliedVolume: {
		from: "intents i", timeCol: "i.state_changed_at", cond: "i.state = 'APPLIED'", value: "sum(i.minor_units)",
		groups: map[string]string{"currency": "i.currency"},
	},
	app.MetricTimeToApplied: {
		from: "intents i", timeCol: "i.state_changed_at", cond: "i.state = 'APPLIED'",
		value: "percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM i.state_changed_at - i.created_at) * 1000)",
	},
	app.MetricSubmissions: {
		from: "attempts a", timeCol: "a.started_at", cond: "a.kind = 'agent_submission'", value: "count(*)",
		groups: map[string]string{"outcome": "a.outcome", "agent": "COALESCE(a.agent_id, 'none')"},
	},
	app.MetricDuplicatesPrevented: {
		from: "attempts a", timeCol: "a.started_at", cond: "a.kind = 'agent_submission' AND a.outcome IN ('attached', 'deduplicated')", value: "count(*)",
		groups: map[string]string{"agent": "COALESCE(a.agent_id, 'none')"},
	},
	app.MetricRailCalls: {
		from: "attempts a", timeCol: "a.started_at", cond: "a.kind = 'rail_call'", value: "count(*)",
		groups: map[string]string{"outcome": effectiveOutcome, "operation": "a.operation"},
	},
}

func (r *AnalyticsReader) Series(ctx context.Context, org domain.OrgID, q app.SeriesQuery) (app.SeriesData, error) {
	src, ok := seriesSources[q.Metric]
	if !ok {
		return app.SeriesData{}, fmt.Errorf("analytics: no source for metric %q", q.Metric)
	}
	group := "''"
	if q.GroupBy != "" {
		if group, ok = src.groups[q.GroupBy]; !ok {
			return app.SeriesData{}, fmt.Errorf("analytics: %s has no group %q", q.Metric, q.GroupBy)
		}
	}
	tz := q.Period.Location.String()
	args := []any{string(org), q.Interval, tz, q.Period.From, q.Period.To}

	var data app.SeriesData
	rows, err := r.pool.Query(ctx, `
		SELECT b AT TIME ZONE $2
		FROM generate_series(date_trunc($1, $3::timestamptz AT TIME ZONE $2),
		                     ($4::timestamptz AT TIME ZONE $2) - interval '1 microsecond',
		                     ('1 ' || $1)::interval) b`, q.Interval, tz, q.Period.From, q.Period.To)
	if err != nil {
		return data, fmt.Errorf("making buckets: %w", err)
	}
	for rows.Next() {
		var b time.Time
		if err := rows.Scan(&b); err != nil {
			rows.Close()
			return data, err
		}
		data.Buckets = append(data.Buckets, b)
	}
	rows.Close()

	orgCol := src.timeCol[:2] + "org_id"
	rows, err = r.pool.Query(ctx, `
		SELECT date_trunc($2, `+src.timeCol+` AT TIME ZONE $3) AT TIME ZONE $3, `+group+`, (`+src.value+`)::float8
		FROM `+src.from+`
		WHERE `+orgCol+` = $1 AND `+src.cond+` AND `+src.timeCol+` >= $4 AND `+src.timeCol+` < $5
		GROUP BY 1, 2`, args...)
	if err != nil {
		return data, fmt.Errorf("computing %s: %w", q.Metric, err)
	}
	defer rows.Close()
	for rows.Next() {
		var row app.SeriesRow
		if err := rows.Scan(&row.Bucket, &row.Group, &row.Value); err != nil {
			return data, err
		}
		data.Rows = append(data.Rows, row)
	}
	return data, rows.Err()
}
