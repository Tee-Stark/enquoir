package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

var _ app.AgentStore = (*AgentStore)(nil)

// AgentStore persists agents and their API keys.
type AgentStore struct {
	pool *pgxpool.Pool
}

func NewAgentStore(pool *pgxpool.Pool) *AgentStore { return &AgentStore{pool: pool} }

func insertKey(ctx context.Context, tx pgx.Tx, k app.APIKey) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO api_keys (id, agent_id, org_id, prefix, secret_hash, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		k.ID, string(k.AgentID), string(k.OrgID), k.Prefix, k.SecretHash, k.CreatedAt)
	return err
}

func (s *AgentStore) CreateAgent(ctx context.Context, a app.Agent, key app.APIKey) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agents (id, org_id, name, created_at) VALUES ($1, $2, $3, $4)`,
			string(a.ID), string(a.OrgID), a.Name, a.CreatedAt); err != nil {
			return fmt.Errorf("creating agent: %w", err)
		}
		if err := insertKey(ctx, tx, key); err != nil {
			return fmt.Errorf("creating API key: %w", err)
		}
		return nil
	})
}

// agentSelect joins each agent to its active key, if any.
const agentSelect = `
	SELECT a.id, a.org_id, a.name, a.created_at, a.revoked_at,
	       k.id, k.prefix, k.secret_hash, k.created_at, k.last_used_at
	FROM agents a
	LEFT JOIN api_keys k ON k.agent_id = a.id AND k.revoked_at IS NULL`

func scanAgent(row pgx.Row) (app.Agent, error) {
	var a app.Agent
	var id, org string
	var keyID, prefix, hash *string
	var keyCreated, lastUsed *time.Time
	if err := row.Scan(&id, &org, &a.Name, &a.CreatedAt, &a.RevokedAt, &keyID, &prefix, &hash, &keyCreated, &lastUsed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return app.Agent{}, app.ErrNotFound
		}
		return app.Agent{}, err
	}
	a.ID, a.OrgID = domain.AgentID(id), domain.OrgID(org)
	if keyID != nil {
		a.Key = &app.APIKey{ID: *keyID, AgentID: a.ID, OrgID: a.OrgID, Prefix: *prefix, SecretHash: *hash, CreatedAt: *keyCreated, LastUsedAt: lastUsed}
	}
	return a, nil
}

func (s *AgentStore) ListAgents(ctx context.Context, org domain.OrgID) ([]app.Agent, error) {
	rows, err := s.pool.Query(ctx, agentSelect+` WHERE a.org_id = $1 ORDER BY a.created_at, a.id`, string(org))
	if err != nil {
		return nil, fmt.Errorf("listing agents: %w", err)
	}
	defer rows.Close()
	var out []app.Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *AgentStore) GetAgent(ctx context.Context, org domain.OrgID, id domain.AgentID) (app.Agent, error) {
	return scanAgent(s.pool.QueryRow(ctx, agentSelect+` WHERE a.org_id = $1 AND a.id = $2`, string(org), string(id)))
}

func (s *AgentStore) ReplaceKey(ctx context.Context, org domain.OrgID, id domain.AgentID, key app.APIKey, at time.Time) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var exists bool
		// Lock the agent so concurrent rotations serialise.
		err := tx.QueryRow(ctx, `SELECT true FROM agents WHERE org_id = $1 AND id = $2 AND revoked_at IS NULL FOR UPDATE`,
			string(org), string(id)).Scan(&exists)
		if errors.Is(err, pgx.ErrNoRows) {
			return app.ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at = $2 WHERE agent_id = $1 AND revoked_at IS NULL`, string(id), at); err != nil {
			return err
		}
		return insertKey(ctx, tx, key)
	})
}

func (s *AgentStore) RevokeAgent(ctx context.Context, org domain.OrgID, id domain.AgentID, at time.Time) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var exists bool
		err := tx.QueryRow(ctx, `SELECT true FROM agents WHERE org_id = $1 AND id = $2 FOR UPDATE`, string(org), string(id)).Scan(&exists)
		if errors.Is(err, pgx.ErrNoRows) {
			return app.ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agents SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, string(id), at); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE api_keys SET revoked_at = $2 WHERE agent_id = $1 AND revoked_at IS NULL`, string(id), at)
		return err
	})
}

func (s *AgentStore) ActiveKeyByHash(ctx context.Context, hash string) (app.APIKey, app.Agent, error) {
	a, err := scanAgent(s.pool.QueryRow(ctx, agentSelect+` WHERE k.secret_hash = $1 AND a.revoked_at IS NULL`, hash))
	if err != nil {
		return app.APIKey{}, app.Agent{}, err
	}
	return *a.Key, a, nil
}

// TouchKey records use at most once a minute per key, so a busy agent
// doesn't turn every request into a write.
func (s *AgentStore) TouchKey(ctx context.Context, keyID string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE api_keys SET last_used_at = $2
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < $2::timestamptz - interval '1 minute')`, keyID, at)
	return err
}
