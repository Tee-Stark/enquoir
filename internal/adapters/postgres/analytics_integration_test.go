//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

// seedAt stores an intent of org with the given state and times.
func seedAt(t *testing.T, org domain.OrgID, ref string, minor int64, state string, created time.Time, changed time.Duration, unknownFirst bool) domain.IntentKey {
	t.Helper()
	ctx := context.Background()
	pool := testPool(t)
	i, _ := domain.NewIntent(org, treasuryOf(org), "ben_x", minor, "USD", ref, "")
	if _, _, err := NewIntentRepository(pool).Save(ctx, i); err != nil {
		t.Fatal(err)
	}
	cleanupIntent(t, pool, i.Key)
	events := NewEventLog(pool)
	if unknownFirst {
		events.Append(ctx, org, i.Key, "UNKNOWN", "")
	}
	if state != "" {
		events.Append(ctx, org, i.Key, state, "")
	}
	pool.Exec(ctx, `UPDATE intents SET created_at = $2, state_changed_at = CASE WHEN state IS NULL THEN NULL ELSE $3::timestamptz END WHERE key = $1`,
		string(i.Key), created, created.Add(changed))
	return i.Key
}

func TestAnalyticsReader_Summary(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	org, _ := newTestOrg(t, NewAccountStore(pool))
	base := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	seedAt(t, org.ID, "A1", 1000, "APPLIED", base, 2*time.Second, false)
	seedAt(t, org.ID, "A2", 2500, "APPLIED", base.Add(time.Minute), 4*time.Second, true)
	seedAt(t, org.ID, "F1", 700, "FAILED", base.Add(2*time.Minute), time.Second, true)
	seedAt(t, org.ID, "B1", 900, "BLOCKED", base.Add(3*time.Minute), 0, false)
	seedAt(t, org.ID, "R1", 100, "", base.Add(4*time.Minute), 0, false)
	seedAt(t, org.ID, "OUT", 100, "APPLIED", base.Add(-48*time.Hour), time.Second, false)

	log := NewAttemptLog(pool)
	for _, o := range []app.AttemptOutcome{app.OutcomeStarted, app.OutcomeStarted, app.OutcomeDeduplicated, app.OutcomeInvalid} {
		log.Record(ctx, app.Attempt{ID: ids.NewID("att"), OrgID: org.ID, Kind: app.KindAgentSubmission, Operation: app.OpSubmitIntent, Outcome: o, StartedAt: base})
	}
	log.Record(ctx, app.Attempt{ID: ids.NewID("att"), OrgID: org.ID, Kind: app.KindRailCall, Operation: app.OpCreateTransfer, Outcome: app.OutcomeOK, StartedAt: base})
	pool.Exec(ctx, `UPDATE attempts SET started_at = $2 WHERE org_id = $1`, string(org.ID), base)

	c, err := NewAnalyticsReader(pool).Summary(ctx, org.ID, app.Period{From: base.Add(-time.Hour), To: base.Add(time.Hour), Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	if c.ByState["APPLIED"] != 2 || c.ByState["FAILED"] != 1 || c.ByState["BLOCKED"] != 1 || c.ByState["RECEIVED"] != 1 {
		t.Errorf("by state = %v (the payment outside the period must not count)", c.ByState)
	}
	if c.RecoveredFromUnknown != 2 {
		t.Errorf("recovered = %d, want 2", c.RecoveredFromUnknown)
	}
	if len(c.Volume) != 1 || c.Volume[0].AppliedMinorUnits != 3500 || c.Volume[0].AppliedCount != 2 {
		t.Errorf("volume = %+v, want USD 3500 over 2", c.Volume)
	}
	if c.SubmissionsByOutcome["started"] != 2 || c.SubmissionsByOutcome["deduplicated"] != 1 || c.RailCallsByOutcome["ok"] != 1 {
		t.Errorf("submissions %v, rail calls %v", c.SubmissionsByOutcome, c.RailCallsByOutcome)
	}
	if c.TimeToAppliedCount != 2 || *c.TimeToAppliedP50MS != 3000 {
		t.Errorf("time to applied: count %d, p50 %v; want 2 and 3000ms", c.TimeToAppliedCount, c.TimeToAppliedP50MS)
	}
}

// Daily buckets in New York across the end of daylight saving (1 Nov
// 2026): every bucket starts at local midnight, and a payment at
// 23:30 local on 1 Nov lands on 1 Nov, not the next UTC day.
func TestAnalyticsReader_SeriesFollowsTheCallersZone(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	org, _ := newTestOrg(t, NewAccountStore(pool))
	ny, _ := time.LoadLocation("America/New_York")

	seedAt(t, org.ID, "LATE", 100, "PROPOSED", time.Date(2026, 11, 2, 4, 30, 0, 0, time.UTC), 0, false)  // 1 Nov 23:30 EST
	seedAt(t, org.ID, "EARLY", 100, "PROPOSED", time.Date(2026, 11, 2, 5, 30, 0, 0, time.UTC), 0, false) // 2 Nov 00:30 EST
	seedAt(t, org.ID, "APP", 250, "APPLIED", time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC), time.Second, false)

	q := app.SeriesQuery{Metric: app.MetricPayments, Interval: "day", Period: app.Period{
		From: time.Date(2026, 10, 31, 0, 0, 0, 0, ny), To: time.Date(2026, 11, 3, 0, 0, 0, 0, ny), Location: ny,
	}}
	data, err := NewAnalyticsReader(pool).Series(ctx, org.ID, q)
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Time{time.Date(2026, 10, 31, 0, 0, 0, 0, ny), time.Date(2026, 11, 1, 0, 0, 0, 0, ny), time.Date(2026, 11, 2, 0, 0, 0, 0, ny)}
	if len(data.Buckets) != 3 {
		t.Fatalf("buckets = %v, want 3", data.Buckets)
	}
	for i := range want {
		if !data.Buckets[i].Equal(want[i]) {
			t.Errorf("bucket %d = %v, want local midnight %v", i, data.Buckets[i].In(ny), want[i])
		}
	}
	if d := data.Buckets[2].Sub(data.Buckets[1]); d != 25*time.Hour {
		t.Errorf("1 Nov lasts %v in New York, want 25h", d)
	}
	counts := map[time.Time]float64{}
	for _, r := range data.Rows {
		counts[r.Bucket.In(ny)] = r.Value
	}
	if counts[want[0]] != 1 || counts[want[1]] != 1 || counts[want[2]] != 1 {
		t.Errorf("counts = %v, want one per day", data.Rows)
	}

	q.Metric, q.GroupBy = app.MetricAppliedVolume, "currency"
	data, _ = NewAnalyticsReader(pool).Series(ctx, org.ID, q)
	if len(data.Rows) != 1 || data.Rows[0].Group != "USD" || data.Rows[0].Value != 250 {
		t.Errorf("volume rows = %+v", data.Rows)
	}

	kolkata, _ := time.LoadLocation("Asia/Kolkata")
	q = app.SeriesQuery{Metric: app.MetricPayments, Interval: "hour", Period: app.Period{
		From: time.Date(2026, 11, 2, 9, 0, 0, 0, kolkata), To: time.Date(2026, 11, 2, 12, 0, 0, 0, kolkata), Location: kolkata}}
	data, _ = NewAnalyticsReader(pool).Series(ctx, org.ID, q)
	if len(data.Buckets) != 3 || data.Buckets[0].In(kolkata).Minute() != 0 {
		t.Errorf("Kolkata hour buckets = %v, want 3 on the local hour (+05:30)", data.Buckets)
	}
}
