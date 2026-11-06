package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

var _ app.AttemptLog = (*AttemptLog)(nil)

// AttemptLog stores attempts in Postgres.
type AttemptLog struct {
	pool *pgxpool.Pool
}

func NewAttemptLog(pool *pgxpool.Pool) *AttemptLog { return &AttemptLog{pool: pool} }

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullableInt(n int) *int {
	if n == 0 {
		return nil
	}
	return &n
}

func (l *AttemptLog) insert(ctx context.Context, a app.Attempt) error {
	details, err := json.Marshal(a.Details)
	if err != nil {
		return err
	}
	if a.Details == nil {
		details = []byte("{}")
	}
	_, err = l.pool.Exec(ctx, `
		INSERT INTO attempts (id, org_id, kind, operation, outcome, intent_key, beneficiary_key, agent_id,
			request_id, transfer_id, transfer_status, http_status, error_code, error_message, activity_attempt,
			details, started_at, finished_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`,
		a.ID, string(a.OrgID), string(a.Kind), a.Operation, string(a.Outcome),
		nullable(string(a.IntentKey)), nullable(string(a.BeneficiaryKey)), nullable(string(a.AgentID)),
		nullable(a.RequestID), nullable(a.TransferID), nullable(a.TransferStatus), nullableInt(a.HTTPStatus),
		nullable(a.ErrorCode), nullable(a.ErrorMessage), nullableInt(a.ActivityAttempt),
		details, a.StartedAt, a.FinishedAt)
	if err != nil {
		return fmt.Errorf("recording attempt: %w", err)
	}
	return nil
}

func (l *AttemptLog) Record(ctx context.Context, a app.Attempt) error { return l.insert(ctx, a) }

func (l *AttemptLog) Start(ctx context.Context, a app.Attempt) error {
	a.Outcome = app.OutcomeInProgress
	a.FinishedAt = nil
	return l.insert(ctx, a)
}

func (l *AttemptLog) Finish(ctx context.Context, org domain.OrgID, id string, f app.AttemptFinish) error {
	tag, err := l.pool.Exec(ctx, `
		UPDATE attempts SET outcome = $3, transfer_id = COALESCE($4, transfer_id), transfer_status = $5,
			http_status = $6, error_code = $7, error_message = $8, finished_at = $9
		WHERE org_id = $1 AND id = $2`,
		string(org), id, string(f.Outcome), nullable(f.TransferID), nullable(f.TransferStatus),
		nullableInt(f.HTTPStatus), nullable(f.ErrorCode), nullable(f.ErrorMessage), f.FinishedAt)
	if err != nil {
		return fmt.Errorf("finishing attempt: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return app.ErrNotFound
	}
	return nil
}

// effectiveOutcome reads a long-unfinished rail call as interrupted.
const effectiveOutcome = `CASE WHEN a.outcome = 'in_progress' AND a.started_at < now() - interval '2 minutes'
	THEN 'interrupted' ELSE a.outcome END`

const attemptColumns = `a.id, a.org_id, a.kind, a.operation, ` + effectiveOutcome + `,
	COALESCE(a.intent_key, ''), COALESCE(a.beneficiary_key, ''), COALESCE(a.agent_id, ''), COALESCE(ag.name, ''),
	COALESCE(a.request_id, ''), COALESCE(a.transfer_id, ''), COALESCE(a.transfer_status, ''),
	COALESCE(a.http_status, 0), COALESCE(a.error_code, ''), COALESCE(a.error_message, ''),
	COALESCE(a.activity_attempt, 0), a.details, a.started_at, a.finished_at`

func scanAttempt(row pgx.Row) (app.Attempt, error) {
	var a app.Attempt
	var org, kind, outcome, intent, ben, agent string
	var details []byte
	err := row.Scan(&a.ID, &org, &kind, &a.Operation, &outcome, &intent, &ben, &agent, &a.AgentName,
		&a.RequestID, &a.TransferID, &a.TransferStatus, &a.HTTPStatus, &a.ErrorCode, &a.ErrorMessage,
		&a.ActivityAttempt, &details, &a.StartedAt, &a.FinishedAt)
	if err != nil {
		return app.Attempt{}, err
	}
	a.OrgID, a.Kind, a.Outcome = domain.OrgID(org), app.AttemptKind(kind), app.AttemptOutcome(outcome)
	a.IntentKey, a.BeneficiaryKey, a.AgentID = domain.IntentKey(intent), domain.BeneficiaryKey(ben), domain.AgentID(agent)
	if err := json.Unmarshal(details, &a.Details); err != nil {
		return app.Attempt{}, err
	}
	return a, nil
}

func (l *AttemptLog) List(ctx context.Context, org domain.OrgID, f app.AttemptFilter) (app.AttemptPage, error) {
	w := &where{}
	w.add("a.org_id = ?", string(org))
	if f.Kind != "" {
		w.add("a.kind = ?", string(f.Kind))
	}
	if f.Operation != "" {
		w.add("a.operation = ?", f.Operation)
	}
	if len(f.Outcomes) > 0 {
		outcomes := make([]string, len(f.Outcomes))
		for i, o := range f.Outcomes {
			outcomes[i] = string(o)
		}
		w.add("("+effectiveOutcome+") = ANY(?)", outcomes)
	}
	if f.AgentID != "" {
		w.add("a.agent_id = ?", string(f.AgentID))
	}
	if f.IntentKey != "" {
		w.add("a.intent_key = ?", string(f.IntentKey))
	}
	if !f.From.IsZero() {
		w.add("a.started_at >= ?", f.From)
	}
	if !f.To.IsZero() {
		w.add("a.started_at < ?", f.To)
	}
	if f.Cursor != "" {
		t, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return app.AttemptPage{}, &app.ValidationError{Field: "cursor", Message: err.Error()}
		}
		w.add("(a.started_at, a.id) < (?, ?)", t, id)
	}
	limit := w.next(f.Limit + 1)

	rows, err := l.pool.Query(ctx, `
		SELECT `+attemptColumns+`
		FROM attempts a LEFT JOIN agents ag ON ag.id = a.agent_id
		WHERE `+w.sql()+`
		ORDER BY a.started_at DESC, a.id DESC
		LIMIT `+limit, w.args...)
	if err != nil {
		return app.AttemptPage{}, fmt.Errorf("listing attempts: %w", err)
	}
	defer rows.Close()
	var page app.AttemptPage
	for rows.Next() {
		a, err := scanAttempt(rows)
		if err != nil {
			return app.AttemptPage{}, err
		}
		page.Attempts = append(page.Attempts, a)
	}
	if err := rows.Err(); err != nil {
		return app.AttemptPage{}, err
	}
	if len(page.Attempts) > f.Limit {
		page.Attempts = page.Attempts[:f.Limit]
		last := page.Attempts[f.Limit-1]
		page.NextCursor = encodeCursor(last.StartedAt, last.ID)
	}
	return page, nil
}

// pageTime keeps microsecond precision, which is all Postgres stores.
func pageTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }
