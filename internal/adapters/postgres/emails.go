package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

var (
	_ app.EmailOutbox = (*EmailOutbox)(nil)
	_ app.TokenStore  = (*TokenStore)(nil)
)

// EmailOutbox stores queued emails.
type EmailOutbox struct{ pool *pgxpool.Pool }

func NewEmailOutbox(pool *pgxpool.Pool) *EmailOutbox { return &EmailOutbox{pool: pool} }

func (o *EmailOutbox) Queue(ctx context.Context, e app.Email) error {
	params, err := json.Marshal(e.Params)
	if err != nil {
		return err
	}
	var purpose, subject *string
	var ttl *int
	if e.Link != nil {
		p, s, secs := string(e.Link.Purpose), e.Link.Subject, int(e.Link.TTL.Seconds())
		purpose, subject, ttl = &p, &s, &secs
	}
	_, err = o.pool.Exec(ctx, `
		INSERT INTO emails (id, org_id, user_id, to_email, template, params, link_purpose, link_subject, link_ttl_secs, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		e.ID, nullable(string(e.OrgID)), nullable(string(e.UserID)), string(e.To), string(e.Template), params,
		purpose, subject, ttl, string(e.Status), e.CreatedAt)
	if err != nil {
		return fmt.Errorf("queueing email: %w", err)
	}
	return nil
}

func (o *EmailOutbox) Get(ctx context.Context, id string) (app.Email, error) {
	var e app.Email
	var org, user, provider, failure, purpose, subject *string
	var ttl *int
	var to, template, status string
	var params []byte
	err := o.pool.QueryRow(ctx, `
		SELECT id, org_id, user_id, to_email, template, params, link_purpose, link_subject, link_ttl_secs,
		       status, provider_id, error, created_at, sent_at
		FROM emails WHERE id = $1`, id).
		Scan(&e.ID, &org, &user, &to, &template, &params, &purpose, &subject, &ttl, &status, &provider, &failure, &e.CreatedAt, &e.SentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Email{}, app.ErrNotFound
	}
	if err != nil {
		return app.Email{}, err
	}
	e.To, e.Template, e.Status = domain.Email(to), app.TemplateID(template), app.EmailStatus(status)
	if org != nil {
		e.OrgID = domain.OrgID(*org)
	}
	if user != nil {
		e.UserID = domain.UserID(*user)
	}
	if provider != nil {
		e.ProviderID = *provider
	}
	if failure != nil {
		e.Error = *failure
	}
	if purpose != nil {
		e.Link = &app.LinkSpec{Purpose: app.TokenPurpose(*purpose), Subject: *subject, TTL: time.Duration(*ttl) * time.Second}
	}
	if err := json.Unmarshal(params, &e.Params); err != nil {
		return app.Email{}, err
	}
	return e, nil
}

// MarkSent and MarkFailed only move a queued email: the first outcome
// recorded stands.
func (o *EmailOutbox) MarkSent(ctx context.Context, id, providerID string, at time.Time) error {
	_, err := o.pool.Exec(ctx, `UPDATE emails SET status = 'sent', provider_id = $2, sent_at = $3 WHERE id = $1 AND status = 'queued'`,
		id, nullable(providerID), at)
	return err
}

func (o *EmailOutbox) MarkFailed(ctx context.Context, id, reason string, at time.Time) error {
	if len(reason) > 1000 {
		reason = reason[:1000]
	}
	_, err := o.pool.Exec(ctx, `UPDATE emails SET status = 'failed', error = $2 WHERE id = $1 AND status = 'queued'`, id, reason)
	return err
}

// Queued lists emails still queued after olderThan, oldest first, for
// the worker's sweep.
func (o *EmailOutbox) Queued(ctx context.Context, olderThan time.Time, limit int) ([]string, error) {
	rows, err := o.pool.Query(ctx, `SELECT id FROM emails WHERE status = 'queued' AND created_at < $1 ORDER BY created_at LIMIT $2`, olderThan, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// TokenStore keeps one-time link tokens by hash.
type TokenStore struct{ pool *pgxpool.Pool }

func NewTokenStore(pool *pgxpool.Pool) *TokenStore { return &TokenStore{pool: pool} }

func (s *TokenStore) Create(ctx context.Context, t app.OneTimeToken) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO one_time_tokens (token_hash, purpose, subject, email_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		t.Hash, string(t.Purpose), t.Subject, nullable(t.EmailID), t.CreatedAt, t.ExpiresAt)
	if err != nil {
		return fmt.Errorf("storing token: %w", err)
	}
	return nil
}

func (s *TokenStore) Consume(ctx context.Context, hash string, purpose app.TokenPurpose, now time.Time) (app.OneTimeToken, error) {
	var t app.OneTimeToken
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var p string
		var emailID *string
		err := tx.QueryRow(ctx, `
			UPDATE one_time_tokens SET used_at = $3
			WHERE token_hash = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > $3
			RETURNING token_hash, purpose, subject, email_id, created_at, expires_at, used_at`,
			hash, string(purpose), now).Scan(&t.Hash, &p, &t.Subject, &emailID, &t.CreatedAt, &t.ExpiresAt, &t.UsedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return app.ErrTokenInvalid
		}
		if err != nil {
			return err
		}
		t.Purpose = app.TokenPurpose(p)
		if emailID != nil {
			t.EmailID = *emailID
		}
		_, err = tx.Exec(ctx, `UPDATE one_time_tokens SET used_at = $3 WHERE purpose = $1 AND subject = $2 AND used_at IS NULL`,
			p, t.Subject, now)
		return err
	})
	if err != nil {
		return app.OneTimeToken{}, err
	}
	return t, nil
}
