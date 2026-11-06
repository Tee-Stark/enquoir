package httpapi

import (
	"net/http"
	"time"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

type periodDTO struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
	TZ   string    `json:"tz"`
}

func newPeriodDTO(p app.Period) periodDTO {
	return periodDTO{From: p.From.In(p.Location), To: p.To.In(p.Location), TZ: p.Location.String()}
}

type volumeDTO struct {
	Currency          string `json:"currency"`
	AppliedMinorUnits int64  `json:"applied_minor_units"`
	AppliedAmount     string `json:"applied_amount"`
	AppliedCount      int    `json:"applied_count"`
}

type summaryDTO struct {
	Period   periodDTO `json:"period"`
	Payments struct {
		Total                int            `json:"total"`
		ByState              map[string]int `json:"by_state"`
		InProgress           int            `json:"in_progress"`
		BlockedByPolicy      int            `json:"blocked_by_policy"`
		SuccessRate          *float64       `json:"success_rate"`
		RecoveredFromUnknown int            `json:"recovered_from_unknown"`
	} `json:"payments"`
	Volume      []volumeDTO `json:"volume"`
	Submissions struct {
		Total               int            `json:"total"`
		ByOutcome           map[string]int `json:"by_outcome"`
		DuplicatesPrevented int            `json:"duplicates_prevented"`
		ActiveAgents        int            `json:"active_agents"`
	} `json:"submissions"`
	RailCalls struct {
		Total     int            `json:"total"`
		ByOutcome map[string]int `json:"by_outcome"`
	} `json:"rail_calls"`
	TimeToApplied struct {
		Count int    `json:"count"`
		P50MS *int64 `json:"p50_ms"`
		P95MS *int64 `json:"p95_ms"`
	} `json:"time_to_applied"`
}

func (h *Handler) analyticsSummary(w http.ResponseWriter, r *http.Request) {
	q := &query{v: r.URL.Query()}
	from, to, tz := q.time("from"), q.time("to"), q.str("tz")
	if q.err != nil {
		writeAppError(w, q.err, "")
		return
	}
	s, err := h.Summary.Run(r.Context(), userFrom(r.Context()).Organisation.ID, from, to, tz)
	if err != nil {
		writeAppError(w, err, "could not compute the summary")
		return
	}
	var out summaryDTO
	out.Period = newPeriodDTO(s.Period)
	out.Payments.Total, out.Payments.ByState, out.Payments.InProgress = s.Payments, s.ByState, s.InProgress
	out.Payments.BlockedByPolicy, out.Payments.SuccessRate, out.Payments.RecoveredFromUnknown = s.BlockedByPolicy, s.SuccessRate, s.RecoveredFromUnknown
	out.Volume = make([]volumeDTO, len(s.Volume))
	for i, v := range s.Volume {
		amount := ""
		if m, err := domain.NewMoney(v.AppliedMinorUnits, v.Currency); err == nil {
			amount = m.MajorUnitsString()
		}
		out.Volume[i] = volumeDTO{Currency: string(v.Currency), AppliedMinorUnits: v.AppliedMinorUnits, AppliedAmount: amount, AppliedCount: v.AppliedCount}
	}
	out.Submissions.Total, out.Submissions.ByOutcome = s.Submissions, s.SubmissionsByOutcome
	out.Submissions.DuplicatesPrevented, out.Submissions.ActiveAgents = s.DuplicatesPrevented, s.ActiveAgents
	out.RailCalls.Total, out.RailCalls.ByOutcome = s.RailCalls, s.RailCallsByOutcome
	out.TimeToApplied.Count, out.TimeToApplied.P50MS, out.TimeToApplied.P95MS = s.TimeToAppliedCount, s.TimeToAppliedP50MS, s.TimeToAppliedP95MS
	writeJSON(w, http.StatusOK, out)
}

type pointDTO struct {
	Start time.Time `json:"start"`
	Value *float64  `json:"value"`
}

type seriesDTO struct {
	Group  string     `json:"group"`
	Points []pointDTO `json:"points"`
}

type timeSeriesDTO struct {
	Metric   string      `json:"metric"`
	Interval string      `json:"interval"`
	GroupBy  *string     `json:"group_by"`
	Unit     string      `json:"unit"`
	Period   periodDTO   `json:"period"`
	Buckets  []time.Time `json:"buckets"`
	Series   []seriesDTO `json:"series"`
}

func (h *Handler) analyticsTimeSeries(w http.ResponseWriter, r *http.Request) {
	q := &query{v: r.URL.Query()}
	metric, interval, groupBy := q.str("metric"), q.str("interval"), q.str("group_by")
	from, to, tz := q.time("from"), q.time("to"), q.str("tz")
	if metric == "" {
		q.fail("metric", "metric is required")
	}
	if q.err != nil {
		writeAppError(w, q.err, "")
		return
	}
	ts, err := h.TimeSeries.Run(r.Context(), userFrom(r.Context()).Organisation.ID, metric, interval, groupBy, from, to, tz)
	if err != nil {
		writeAppError(w, err, "could not compute the time series")
		return
	}
	out := timeSeriesDTO{Metric: ts.Query.Metric, Interval: ts.Query.Interval, GroupBy: optional(ts.Query.GroupBy), Unit: ts.Unit,
		Period: newPeriodDTO(ts.Query.Period), Buckets: ts.Buckets, Series: make([]seriesDTO, len(ts.Series))}
	if out.Buckets == nil {
		out.Buckets = []time.Time{}
	}
	for i, s := range ts.Series {
		d := seriesDTO{Group: s.Group, Points: make([]pointDTO, len(s.Points))}
		for j, p := range s.Points {
			d.Points[j] = pointDTO{Start: p.Start, Value: p.Value}
		}
		out.Series[i] = d
	}
	writeJSON(w, http.StatusOK, out)
}
