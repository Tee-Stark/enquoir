package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

var _ app.AuditLog = (*AuditLog)(nil)

// AuditLog stores account actions. Times come from the database clock.
type AuditLog struct{ pool *pgxpool.Pool }

func NewAuditLog(pool *pgxpool.Pool) *AuditLog { return &AuditLog{pool: pool} }

func (l *AuditLog) Record(ctx context.Context, e app.AuditEvent) error {
	details, err := json.Marshal(e.Details)
	if err != nil || e.Details == nil {
		details = []byte("{}")
	}
	ua := e.UserAgent
	if len(ua) > 300 {
		ua = ua[:300]
	}
	_, err = l.pool.Exec(ctx, `
		INSERT INTO audit_events (id, org_id, actor_user_id, actor_email, action, target_type, target_id, details, ip, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		e.ID, nullable(string(e.OrgID)), nullable(string(e.ActorUserID)), nullable(string(e.ActorEmail)), e.Action,
		nullable(e.TargetType), nullable(e.TargetID), details, nullable(e.IP), nullable(ua))
	if err != nil {
		return fmt.Errorf("recording audit event: %w", err)
	}
	return nil
}

func (l *AuditLog) List(ctx context.Context, org domain.OrgID, f app.AuditFilter) (app.AuditPage, error) {
	w := &where{}
	w.add("a.org_id = ?", string(org))
	if f.Action != "" {
		w.add("a.action = ?", f.Action)
	}
	if f.Actor != "" {
		w.add("a.actor_user_id = ?", string(f.Actor))
	}
	if !f.From.IsZero() {
		w.add("a.at >= ?", f.From)
	}
	if !f.To.IsZero() {
		w.add("a.at < ?", f.To)
	}
	if f.Cursor != "" {
		t, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return app.AuditPage{}, &app.ValidationError{Field: "cursor", Message: err.Error()}
		}
		w.add("(a.at, a.id) < (?, ?)", t, id)
	}
	limit := w.next(f.Limit + 1)
	rows, err := l.pool.Query(ctx, `
		SELECT a.id, COALESCE(a.org_id, ''), COALESCE(a.actor_user_id, ''), COALESCE(a.actor_email, ''), a.action,
		       COALESCE(a.target_type, ''), COALESCE(a.target_id, ''), a.details, COALESCE(a.ip, ''), COALESCE(a.user_agent, ''), a.at
		FROM audit_events a WHERE `+w.sql()+` ORDER BY a.at DESC, a.id DESC LIMIT `+limit, w.args...)
	if err != nil {
		return app.AuditPage{}, fmt.Errorf("listing audit events: %w", err)
	}
	defer rows.Close()
	var page app.AuditPage
	for rows.Next() {
		var e app.AuditEvent
		var orgID, actor, email string
		var details []byte
		if err := rows.Scan(&e.ID, &orgID, &actor, &email, &e.Action, &e.TargetType, &e.TargetID, &details, &e.IP, &e.UserAgent, &e.At); err != nil {
			return app.AuditPage{}, err
		}
		e.OrgID, e.ActorUserID, e.ActorEmail = domain.OrgID(orgID), domain.UserID(actor), domain.Email(email)
		json.Unmarshal(details, &e.Details)
		page.Events = append(page.Events, e)
	}
	if err := rows.Err(); err != nil {
		return app.AuditPage{}, err
	}
	if len(page.Events) > f.Limit {
		page.Events = page.Events[:f.Limit]
		last := page.Events[f.Limit-1]
		page.NextCursor = encodeCursor(last.At, last.ID)
	}
	return page, nil
}
