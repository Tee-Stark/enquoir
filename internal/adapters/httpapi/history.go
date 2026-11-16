package httpapi

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

type moneyDTO struct {
	MinorUnits int64  `json:"minor_units"`
	Currency   string `json:"currency"`
	Amount     string `json:"amount"`
}

func newMoneyDTO(m domain.Money) moneyDTO {
	return moneyDTO{MinorUnits: m.MinorUnits(), Currency: string(m.Currency()), Amount: m.MajorUnitsString()}
}

type refDTO struct {
	ID   string  `json:"id"`
	Name *string `json:"name"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optionalInt(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

type paymentDTO struct {
	Key            string     `json:"key"`
	State          string     `json:"state"`
	Terminal       bool       `json:"terminal"`
	Amount         moneyDTO   `json:"amount"`
	Payee          refDTO     `json:"payee"`
	Reference      string     `json:"reference"`
	Reason         string     `json:"reason"`
	Agent          *refDTO    `json:"agent"`
	TransferID     *string    `json:"transfer_id"`
	FailureReason  *string    `json:"failure_reason"`
	PolicyVersion  *int       `json:"policy_version"`
	CreatedAt      time.Time  `json:"created_at"`
	StateChangedAt *time.Time `json:"state_changed_at"`
	Submissions    int        `json:"submissions"`
	RailCalls      int        `json:"rail_calls"`
}

func newPaymentDTO(p app.Payment) paymentDTO {
	d := paymentDTO{
		Key: string(p.Key), State: p.State, Terminal: p.Terminal(), Amount: newMoneyDTO(p.Amount),
		Payee: refDTO{ID: p.Payee, Name: optional(p.PayeeName)}, Reference: p.Reference, Reason: p.Reason,
		TransferID: optional(p.TransferID), FailureReason: optional(p.FailureReason), PolicyVersion: p.PolicyVersion,
		CreatedAt: p.CreatedAt.UTC(), StateChangedAt: utcPtr(p.StateChangedAt),
		Submissions: p.Submissions, RailCalls: p.RailCalls,
	}
	if p.AgentID != "" {
		d.Agent = &refDTO{ID: string(p.AgentID), Name: optional(p.AgentName)}
	}
	return d
}

type attemptDTO struct {
	ID              string            `json:"id"`
	Kind            string            `json:"kind"`
	Operation       string            `json:"operation"`
	Outcome         string            `json:"outcome"`
	IntentKey       *string           `json:"intent_key"`
	BeneficiaryKey  *string           `json:"beneficiary_key"`
	Agent           *refDTO           `json:"agent"`
	RequestID       *string           `json:"request_id"`
	TransferID      *string           `json:"transfer_id"`
	TransferStatus  *string           `json:"transfer_status"`
	HTTPStatus      *int              `json:"http_status"`
	ErrorCode       *string           `json:"error_code"`
	ErrorMessage    *string           `json:"error_message"`
	ActivityAttempt *int              `json:"activity_attempt"`
	Details         map[string]string `json:"details"`
	StartedAt       time.Time         `json:"started_at"`
	FinishedAt      *time.Time        `json:"finished_at"`
	DurationMS      *int64            `json:"duration_ms"`
}

func newAttemptDTO(a app.Attempt) attemptDTO {
	d := attemptDTO{
		ID: a.ID, Kind: string(a.Kind), Operation: a.Operation, Outcome: string(a.Outcome),
		IntentKey: optional(string(a.IntentKey)), BeneficiaryKey: optional(string(a.BeneficiaryKey)),
		RequestID: optional(a.RequestID), TransferID: optional(a.TransferID), TransferStatus: optional(a.TransferStatus),
		HTTPStatus: optionalInt(a.HTTPStatus), ErrorCode: optional(a.ErrorCode), ErrorMessage: optional(a.ErrorMessage),
		ActivityAttempt: optionalInt(a.ActivityAttempt), Details: a.Details,
		StartedAt: a.StartedAt.UTC(), FinishedAt: utcPtr(a.FinishedAt),
	}
	if d.Details == nil {
		d.Details = map[string]string{}
	}
	if a.AgentID != "" {
		d.Agent = &refDTO{ID: string(a.AgentID), Name: optional(a.AgentName)}
	}
	if dur := a.Duration(); dur != nil {
		ms := dur.Milliseconds()
		d.DurationMS = &ms
	}
	return d
}

type eventDetailDTO struct {
	SequenceNumber int       `json:"sequence_number"`
	Type           string    `json:"type"`
	Payload        string    `json:"payload"`
	RecordedAt     time.Time `json:"recorded_at"`
	Hash           string    `json:"hash"`
	PrevHash       string    `json:"prev_hash"`
}

type chainDTO struct {
	Valid bool    `json:"valid"`
	Error *string `json:"error"`
}

func newChainDTO(err error) chainDTO {
	if err == nil {
		return chainDTO{Valid: true}
	}
	msg := err.Error()
	return chainDTO{Valid: false, Error: &msg}
}

type paymentDetailDTO struct {
	paymentDTO
	Events   []eventDetailDTO `json:"events"`
	Chain    chainDTO         `json:"chain"`
	Attempts []attemptDTO     `json:"attempts"`
}

// query reads list parameters, collecting the first problem.
type query struct {
	v   url.Values
	err *app.ValidationError
}

func (q *query) fail(field, msg string) {
	if q.err == nil {
		q.err = &app.ValidationError{Field: field, Message: msg}
	}
}

func (q *query) str(name string) string { return strings.TrimSpace(q.v.Get(name)) }

// list accepts repeated parameters and comma-separated values alike.
func (q *query) list(name string) []string {
	var out []string
	for _, v := range q.v[name] {
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func (q *query) time(name string) time.Time {
	s := q.str(name)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		q.fail(name, "must be an RFC 3339 timestamp, e.g. 2026-11-06T00:00:00Z")
	}
	return t
}

func (q *query) int(name string) int {
	s := q.str(name)
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		q.fail(name, "must be a whole number")
	}
	return n
}

func (h *Handler) listPayments(w http.ResponseWriter, r *http.Request) {
	q := &query{v: r.URL.Query()}
	f := app.PaymentFilter{
		States: q.list("state"), AgentID: domain.AgentID(q.str("agent_id")), Payee: q.str("payee"),
		Currency: domain.Currency(strings.ToUpper(q.str("currency"))), Reference: q.str("reference"),
		From: q.time("from"), To: q.time("to"), Limit: q.int("limit"), Cursor: q.str("cursor"),
	}
	if q.err != nil {
		writeAppError(w, q.err, "")
		return
	}
	page, err := h.ListPayments.Run(r.Context(), userFrom(r.Context()).Organisation.ID, f)
	if err != nil {
		writeAppError(w, err, "could not list payments")
		return
	}
	data := make([]paymentDTO, len(page.Payments))
	for i, p := range page.Payments {
		data[i] = newPaymentDTO(p)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "next_cursor": optional(page.NextCursor)})
}

func (h *Handler) getPayment(w http.ResponseWriter, r *http.Request) {
	d, err := h.GetPayment.Run(r.Context(), userFrom(r.Context()).Organisation.ID, domain.IntentKey(chi.URLParam(r, "key")))
	if err != nil {
		writeAppError(w, err, "could not load the payment")
		return
	}
	out := paymentDetailDTO{paymentDTO: newPaymentDTO(d.Payment), Chain: newChainDTO(d.ChainError),
		Events: make([]eventDetailDTO, len(d.Events)), Attempts: make([]attemptDTO, len(d.Attempts))}
	for i, e := range d.Events {
		out.Events[i] = eventDetailDTO{SequenceNumber: e.SequenceNumber, Type: e.Type, Payload: e.Payload, RecordedAt: e.RecordedAt.UTC(), Hash: e.Hash, PrevHash: e.PrevHash}
	}
	for i, a := range d.Attempts {
		out.Attempts[i] = newAttemptDTO(a)
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) listAttempts(w http.ResponseWriter, r *http.Request) {
	q := &query{v: r.URL.Query()}
	f := app.AttemptFilter{
		Kind: app.AttemptKind(q.str("kind")), Operation: q.str("operation"),
		AgentID: domain.AgentID(q.str("agent_id")), IntentKey: domain.IntentKey(q.str("intent_key")),
		From: q.time("from"), To: q.time("to"), Limit: q.int("limit"), Cursor: q.str("cursor"),
	}
	for _, o := range q.list("outcome") {
		f.Outcomes = append(f.Outcomes, app.AttemptOutcome(o))
	}
	if q.err != nil {
		writeAppError(w, q.err, "")
		return
	}
	page, err := h.ListAttempts.Run(r.Context(), userFrom(r.Context()).Organisation.ID, f)
	if err != nil {
		writeAppError(w, err, "could not list attempts")
		return
	}
	data := make([]attemptDTO, len(page.Attempts))
	for i, a := range page.Attempts {
		data[i] = newAttemptDTO(a)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "next_cursor": optional(page.NextCursor)})
}

type beneficiaryDTO struct {
	ID              string    `json:"id"`
	Key             string    `json:"key"`
	Name            string    `json:"name"`
	AccountLast4    string    `json:"account_last4"`
	RoutingNumber   string    `json:"routing_number"`
	AccountCategory string    `json:"account_category"`
	CreatedAt       time.Time `json:"created_at"`
}

func (h *Handler) listBeneficiaries(w http.ResponseWriter, r *http.Request) {
	list, err := h.ListBeneficiaries.Run(r.Context(), userFrom(r.Context()).Organisation.ID)
	if err != nil {
		writeAppError(w, err, "could not list beneficiaries")
		return
	}
	data := make([]beneficiaryDTO, len(list))
	for i, b := range list {
		data[i] = beneficiaryDTO{ID: b.RailID, Key: string(b.Key), Name: b.Name, AccountLast4: b.AccountLast4,
			RoutingNumber: b.RoutingNumber, AccountCategory: string(b.Category), CreatedAt: b.CreatedAt.UTC()}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data})
}
