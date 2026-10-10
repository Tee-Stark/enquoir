package postgres

import (
	"context"
	"fmt"

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

// Append loads the chain recorded so far, extends it with the domain's
// own chaining logic, and persists only the new event. The event is
// written before Append returns, never after a caller acts on it.
func (l *EventLog) Append(ctx context.Context, intentKey domain.IntentKey, eventType, payload string) (domain.Event, error) {
	chain, err := l.Load(ctx, intentKey)
	if err != nil {
		return domain.Event{}, err
	}

	chain = chain.Append(intentKey, eventType, payload)
	newEvent := chain[len(chain)-1]

	_, err = l.pool.Exec(ctx, `
		INSERT INTO intent_events (intent_key, sequence_number, type, payload, prev_hash, hash)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		string(intentKey), newEvent.SequenceNumber, newEvent.Type, newEvent.Payload, newEvent.PrevHash, newEvent.Hash,
	)
	if err != nil {
		return domain.Event{}, fmt.Errorf("appending event for %s: %w", intentKey, err)
	}

	return newEvent, nil
}

func (l *EventLog) Load(ctx context.Context, intentKey domain.IntentKey) (domain.Chain, error) {
	rows, err := l.pool.Query(ctx, `
		SELECT sequence_number, type, payload, prev_hash, hash
		FROM intent_events WHERE intent_key = $1
		ORDER BY sequence_number ASC`, string(intentKey),
	)
	if err != nil {
		return nil, fmt.Errorf("loading chain for %s: %w", intentKey, err)
	}
	defer rows.Close()

	var chain domain.Chain
	for rows.Next() {
		e := domain.Event{IntentKey: intentKey}
		if err := rows.Scan(&e.SequenceNumber, &e.Type, &e.Payload, &e.PrevHash, &e.Hash); err != nil {
			return nil, fmt.Errorf("scanning event for %s: %w", intentKey, err)
		}
		chain = append(chain, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading chain for %s: %w", intentKey, err)
	}

	return chain, nil
}
