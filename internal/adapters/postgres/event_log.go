package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

var _ app.EventLog = (*EventLog)(nil)

// EventLog durably records an Intent's hash-chained execution history in
// Postgres.
type EventLog struct {
	pool *pgxpool.Pool
}

func NewEventLog(pool *pgxpool.Pool) *EventLog {
	return &EventLog{pool: pool}
}

// Append records the next event for an intent, and moves the intent's
// read-model state with it, in one transaction. It locks the intent's
// row first, so concurrent appends to one intent queue up instead of
// computing the same next sequence number. The event is durably
// written before Append returns, never after a caller acts on it.
// Tenants are isolated: org scopes every read and write.
func (l *EventLog) Append(ctx context.Context, org domain.OrgID, intentKey domain.IntentKey, eventType, payload string) (domain.Event, error) {
	var newEvent domain.Event
	err := pgx.BeginFunc(ctx, l.pool, func(tx pgx.Tx) error {
		var locked bool
		err := tx.QueryRow(ctx, `SELECT true FROM intents WHERE org_id = $1 AND key = $2 FOR UPDATE`,
			string(org), string(intentKey)).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("appending event for %s: no such intent in organisation %s", intentKey, org)
		}
		if err != nil {
			return err
		}

		chain, err := loadChain(ctx, tx, org, intentKey)
		if err != nil {
			return err
		}
		chain = chain.Append(intentKey, eventType, payload)
		newEvent = chain[len(chain)-1]

		if err := tx.QueryRow(ctx, `
			INSERT INTO intent_events (intent_key, org_id, sequence_number, type, payload, prev_hash, hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING created_at`,
			string(intentKey), string(org), newEvent.SequenceNumber, newEvent.Type, newEvent.Payload, newEvent.PrevHash, newEvent.Hash,
		).Scan(&newEvent.RecordedAt); err != nil {
			return fmt.Errorf("appending event for %s: %w", intentKey, err)
		}

		_, err = tx.Exec(ctx, `
			UPDATE intents SET
				state = $3,
				state_changed_at = $4,
				transfer_id = CASE WHEN $3 = 'APPLIED' THEN $5 ELSE transfer_id END,
				failure_reason = CASE WHEN $3 IN ('FAILED', 'BLOCKED') THEN $5 ELSE failure_reason END
			WHERE org_id = $1 AND key = $2`,
			string(org), string(intentKey), eventType, newEvent.RecordedAt, payload)
		if err != nil {
			return fmt.Errorf("updating state of %s: %w", intentKey, err)
		}
		return nil
	})
	if err != nil {
		return domain.Event{}, err
	}
	return newEvent, nil
}

func (l *EventLog) Load(ctx context.Context, org domain.OrgID, intentKey domain.IntentKey) (domain.Chain, error) {
	return loadChain(ctx, l.pool, org, intentKey)
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func loadChain(ctx context.Context, q querier, org domain.OrgID, intentKey domain.IntentKey) (domain.Chain, error) {
	rows, err := q.Query(ctx, `
		SELECT sequence_number, type, payload, prev_hash, hash, created_at
		FROM intent_events WHERE org_id = $1 AND intent_key = $2
		ORDER BY sequence_number ASC`, string(org), string(intentKey),
	)
	if err != nil {
		return nil, fmt.Errorf("loading chain for %s: %w", intentKey, err)
	}
	defer rows.Close()

	var chain domain.Chain
	for rows.Next() {
		e := domain.Event{IntentKey: intentKey}
		if err := rows.Scan(&e.SequenceNumber, &e.Type, &e.Payload, &e.PrevHash, &e.Hash, &e.RecordedAt); err != nil {
			return nil, fmt.Errorf("scanning event for %s: %w", intentKey, err)
		}
		chain = append(chain, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading chain for %s: %w", intentKey, err)
	}
	return chain, nil
}
