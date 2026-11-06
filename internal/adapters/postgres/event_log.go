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
// row - so concurrent appends to one intent queue instead of computing
// the same next sequence number - reads only the chain's last event,
// all the next one needs, then inserts the event and moves the state in
// one statement. The event is durably
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

		// A separate statement, after the lock is held: one statement
		// that waited for the lock would still read the last event from
		// the snapshot it took before waiting - stale, if another
		// append committed meanwhile.
		var lastSeq *int
		var lastHash *string
		err = tx.QueryRow(ctx, `
			SELECT sequence_number, hash FROM intent_events
			WHERE org_id = $1 AND intent_key = $2
			ORDER BY sequence_number DESC LIMIT 1`, string(org), string(intentKey)).Scan(&lastSeq, &lastHash)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var last *domain.Event
		if lastSeq != nil {
			last = &domain.Event{SequenceNumber: *lastSeq, Hash: *lastHash}
		}
		newEvent = domain.NextEvent(intentKey, last, eventType, payload)

		err = tx.QueryRow(ctx, `
			WITH inserted AS (
				INSERT INTO intent_events (intent_key, org_id, sequence_number, type, payload, prev_hash, hash)
				VALUES ($2, $1, $3, $4, $5, $6, $7)
				RETURNING created_at
			)
			UPDATE intents SET
				state = $4,
				state_changed_at = (SELECT created_at FROM inserted),
				transfer_id = CASE WHEN $4 = 'APPLIED' THEN $5 ELSE transfer_id END,
				failure_reason = CASE WHEN $4 IN ('FAILED', 'BLOCKED') THEN $5 ELSE failure_reason END
			WHERE org_id = $1 AND key = $2
			RETURNING state_changed_at`,
			string(org), string(intentKey), newEvent.SequenceNumber, newEvent.Type, newEvent.Payload, newEvent.PrevHash, newEvent.Hash,
		).Scan(&newEvent.RecordedAt)
		if err != nil {
			return fmt.Errorf("appending event for %s: %w", intentKey, err)
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
