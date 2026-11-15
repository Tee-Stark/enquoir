package httpapi

import (
	"net/http"
	"time"

	"enquoir/internal/commerce/commerceapp"
	"enquoir/internal/domain"
)

// CommerceAnalyticsUseCases are the dashboard's org-wide commerce
// analytics use cases, parallel to the Summary/TimeSeries fields on
// Handler for the treasury side.
type CommerceAnalyticsUseCases struct {
	Summary    *commerceapp.GetCommerceSummary
	TimeSeries *commerceapp.GetCommerceTimeSeries
}

type commercePaidVolumeDTO struct {
	Currency       string `json:"currency"`
	PaidMinorUnits int64  `json:"paid_minor_units"`
	PaidAmount     string `json:"paid_amount"`
	PaidCount      int    `json:"paid_count"`
}

type commerceSummaryDTO struct {
	Period    periodDTO `json:"period"`
	Checkouts struct {
		Total                int            `json:"total"`
		ByStatus             map[string]int `json:"by_status"`
		InProgress           int            `json:"in_progress"`
		Denied               int            `json:"denied"`
		SuccessRate          *float64       `json:"success_rate"`
		RecoveredFromUnknown int            `json:"recovered_from_unknown"`
	} `json:"checkouts"`
	Volume       []commercePaidVolumeDTO `json:"volume"`
	ActiveAgents int                     `json:"active_agents"`
	TimeToPaid   struct {
		Count int    `json:"count"`
		P50MS *int64 `json:"p50_ms"`
		P95MS *int64 `json:"p95_ms"`
	} `json:"time_to_paid"`
}

func (h *Handler) commerceAnalyticsSummary(w http.ResponseWriter, r *http.Request) {
	q := &query{v: r.URL.Query()}
	from, to, tz := q.time("from"), q.time("to"), q.str("tz")
	if q.err != nil {
		writeAppError(w, q.err, "")
		return
	}
	s, err := h.CommerceAnalytics.Summary.Run(r.Context(), userFrom(r.Context()).Organisation.ID, from, to, tz)
	if err != nil {
		writeAppError(w, err, "could not compute the commerce summary")
		return
	}
	var out commerceSummaryDTO
	out.Period = newPeriodDTO(s.Period)
	out.Checkouts.Total, out.Checkouts.ByStatus, out.Checkouts.InProgress = s.Checkouts, s.ByStatus, s.InProgress
	out.Checkouts.Denied, out.Checkouts.SuccessRate, out.Checkouts.RecoveredFromUnknown = s.Denied, s.SuccessRate, s.RecoveredFromUnknown
	out.Volume = make([]commercePaidVolumeDTO, len(s.Volume))
	for i, v := range s.Volume {
		amount := ""
		if m, err := domain.NewMoney(v.PaidMinorUnits, v.Currency); err == nil {
			amount = m.MajorUnitsString()
		}
		out.Volume[i] = commercePaidVolumeDTO{Currency: string(v.Currency), PaidMinorUnits: v.PaidMinorUnits, PaidAmount: amount, PaidCount: v.PaidCount}
	}
	out.ActiveAgents = s.ActiveAgents
	out.TimeToPaid.Count, out.TimeToPaid.P50MS, out.TimeToPaid.P95MS = s.TimeToPaidCount, s.TimeToPaidP50MS, s.TimeToPaidP95MS
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) commerceAnalyticsTimeSeries(w http.ResponseWriter, r *http.Request) {
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
	ts, err := h.CommerceAnalytics.TimeSeries.Run(r.Context(), userFrom(r.Context()).Organisation.ID, metric, interval, groupBy, from, to, tz)
	if err != nil {
		writeAppError(w, err, "could not compute the commerce time series")
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
