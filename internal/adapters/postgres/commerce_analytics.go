package postgres

import (
	"context"
	"fmt"
	"strings"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

var _ commerceapp.CommerceAnalyticsReader = (*CommerceCheckoutStore)(nil)

func (s *CommerceCheckoutStore) Summary(ctx context.Context, org domain.OrgID, p app.Period, now time.Time) (commerceapp.CommerceSummaryCounts, error) {
	var c commerceapp.CommerceSummaryCounts
	var err error
	args := []any{string(org), p.From, p.To, now}
	const inPeriod = `c.org_id = $1 AND c.created_at >= $2 AND c.created_at < $3`

	rows, err := s.pool.Query(ctx, `
		SELECT `+effectiveCheckoutStatus("$4")+`, count(*)
		FROM commerce_checkouts c LEFT JOIN commerce_authorizations a ON a.checkout_id = c.id
		WHERE `+inPeriod+` GROUP BY 1`, args...)
	if err != nil {
		return c, fmt.Errorf("counting checkouts: %w", err)
	}
	c.ByStatus = map[string]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			rows.Close()
			return c, err
		}
		c.ByStatus[k] = n
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return c, err
	}

	if err = s.pool.QueryRow(ctx, `
		SELECT count(*) FROM commerce_checkouts c
		JOIN commerce_authorizations a ON a.checkout_id = c.id
		JOIN commerce_executions e ON e.id = a.execution_id
		WHERE `+inPeriod+` AND c.status IN ('paid', 'payment_failed')
		  AND EXISTS (SELECT 1 FROM commerce_execution_events ev WHERE ev.execution_id = e.id AND ev.status_after = 'unknown')`,
		args[0], args[1], args[2]).Scan(&c.RecoveredFromUnknown); err != nil {
		return c, fmt.Errorf("counting recoveries: %w", err)
	}

	if err = s.pool.QueryRow(ctx, `SELECT count(DISTINCT agent_id) FROM commerce_checkouts c WHERE `+inPeriod,
		args[0], args[1], args[2]).Scan(&c.ActiveAgents); err != nil {
		return c, fmt.Errorf("counting agents: %w", err)
	}

	// Paid volume and time-to-paid are both about the execution's own
	// outcome, not the checkout's submission - filtered and bucketed by
	// when it finished (finished_at), mirroring how Treasury's
	// applied_volume/time_to_applied use state_changed_at, not created_at.
	volArgs := []any{string(org), p.From, p.To}
	rows, err = s.pool.Query(ctx, `
		SELECT currency, sum(amount_minor), count(*) FROM commerce_executions
		WHERE org_id = $1 AND finished_at >= $2 AND finished_at < $3 AND status = 'succeeded'
		GROUP BY currency ORDER BY currency`, volArgs...)
	if err != nil {
		return c, fmt.Errorf("summing paid volume: %w", err)
	}
	for rows.Next() {
		var v commerceapp.CommercePaidVolume
		var cur string
		if err := rows.Scan(&cur, &v.PaidMinorUnits, &v.PaidCount); err != nil {
			rows.Close()
			return c, err
		}
		v.Currency = domain.Currency(cur)
		c.Volume = append(c.Volume, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return c, err
	}

	var p50, p95 *float64
	if err = s.pool.QueryRow(ctx, `
		SELECT count(*), percentile_cont(0.5) WITHIN GROUP (ORDER BY ms), percentile_cont(0.95) WITHIN GROUP (ORDER BY ms)
		FROM (SELECT extract(epoch FROM finished_at - created_at) * 1000 AS ms
		      FROM commerce_executions WHERE org_id = $1 AND finished_at >= $2 AND finished_at < $3 AND status = 'succeeded') t`,
		volArgs...).Scan(&c.TimeToPaidCount, &p50, &p95); err != nil {
		return c, fmt.Errorf("measuring time to paid: %w", err)
	}
	c.TimeToPaidP50MS, c.TimeToPaidP95MS = roundMS(p50), roundMS(p95)
	return c, nil
}

// commerceSeriesSource mirrors seriesSource (analytics.go) for the
// commerce side.
type commerceSeriesSource struct {
	from, timeCol, cond, value string
	groups                     map[string]string
}

var commerceSeriesSources = map[string]commerceSeriesSource{
	commerceapp.MetricCheckouts: {
		from: "commerce_checkouts c LEFT JOIN commerce_authorizations a ON a.checkout_id = c.id", timeCol: "c.created_at", cond: "TRUE", value: "count(*)",
		// $6 is the real current time (see Series below) - the same
		// effective-status computation Summary uses, deliberately not
		// $5 (q.Period.To): a bucket inside the period must show each
		// checkout's real, current status, not a hypothetical status as
		// of the period's end.
		groups: map[string]string{"status": effectiveCheckoutStatus("$6"), "agent": "COALESCE(c.agent_id, 'none')", "currency": "c.currency"},
	},
	commerceapp.MetricPaidVolume: {
		from: "commerce_executions e", timeCol: "e.finished_at", cond: "e.status = 'succeeded'", value: "sum(e.amount_minor)",
		groups: map[string]string{"currency": "e.currency"},
	},
	commerceapp.MetricTimeToPaid: {
		from: "commerce_executions e", timeCol: "e.finished_at", cond: "e.status = 'succeeded'",
		value: "percentile_cont(0.5) WITHIN GROUP (ORDER BY extract(epoch FROM e.finished_at - e.created_at) * 1000)",
	},
}

// Series mirrors AnalyticsReader.Series exactly (same bucket-generation
// template, same orgCol-from-timeCol-prefix trick), reading from
// commerceSeriesSources instead.
func (s *CommerceCheckoutStore) Series(ctx context.Context, org domain.OrgID, q app.SeriesQuery, now time.Time) (app.SeriesData, error) {
	src, ok := commerceSeriesSources[q.Metric]
	if !ok {
		return app.SeriesData{}, fmt.Errorf("commerce analytics: no source for metric %q", q.Metric)
	}
	group := "''"
	if q.GroupBy != "" {
		if group, ok = src.groups[q.GroupBy]; !ok {
			return app.SeriesData{}, fmt.Errorf("commerce analytics: %s has no group %q", q.Metric, q.GroupBy)
		}
	}
	tz := q.Period.Location.String()
	args := []any{string(org), q.Interval, tz, q.Period.From, q.Period.To}
	// pgx errors if more parameters are passed than the SQL text
	// references - only the "status" group's effectiveCheckoutStatus
	// expression needs $6 (now), so only append it then.
	if strings.Contains(group, "$6") {
		args = append(args, now)
	}

	var data app.SeriesData
	rows, err := s.pool.Query(ctx, `
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
	rows, err = s.pool.Query(ctx, `
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
