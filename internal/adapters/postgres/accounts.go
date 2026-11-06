package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"enquoir/internal/app"
	"enquoir/internal/domain"
)

var (
	_ app.AccountStore = (*AccountStore)(nil)
	_ app.SessionStore = (*SessionStore)(nil)
)

const uniqueViolation = "23505"

func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && (constraint == "" || pgErr.ConstraintName == constraint)
}

// AccountStore persists organisations and users.
type AccountStore struct {
	pool *pgxpool.Pool
}

func NewAccountStore(pool *pgxpool.Pool) *AccountStore { return &AccountStore{pool: pool} }

func (s *AccountStore) CreateOrganisationWithOwner(ctx context.Context, org app.Organisation, owner app.User) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO organisations (id, name, created_at) VALUES ($1, $2, $3)`,
			string(org.ID), org.Name, org.CreatedAt); err != nil {
			return fmt.Errorf("creating organisation: %w", err)
		}
		if err := insertUser(ctx, tx, owner); err != nil {
			return err
		}
		return insertMembership(ctx, tx, org.ID, owner.ID, domain.Owner, owner.CreatedAt)
	})
}

func insertUser(ctx context.Context, tx pgx.Tx, u app.User) error {
	_, err := tx.Exec(ctx, `INSERT INTO users (id, email, password_hash, created_at) VALUES ($1, $2, $3, $4)`,
		string(u.ID), string(u.Email), u.PasswordHash, u.CreatedAt)
	if isUniqueViolation(err, "users_email_key") {
		return app.ErrEmailTaken
	}
	if err != nil {
		return fmt.Errorf("creating user: %w", err)
	}
	return nil
}

func insertMembership(ctx context.Context, tx pgx.Tx, org domain.OrgID, user domain.UserID, role domain.Role, at time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role, created_at) VALUES ($1, $2, $3, $4)`,
		string(org), string(user), string(role), at)
	if err != nil {
		return fmt.Errorf("creating membership: %w", err)
	}
	return nil
}

const userColumns = `u.id, u.email, u.password_hash, u.email_verified_at IS NOT NULL, u.created_at`

func scanUser(row pgx.Row) (app.User, error) {
	var u app.User
	var id, email string
	if err := row.Scan(&id, &email, &u.PasswordHash, &u.EmailVerified, &u.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return app.User{}, app.ErrNotFound
		}
		return app.User{}, err
	}
	u.ID, u.Email = domain.UserID(id), domain.Email(email)
	return u, nil
}

func (s *AccountStore) UserByEmail(ctx context.Context, email domain.Email) (app.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users u WHERE u.email = $1`, string(email)))
}

func (s *AccountStore) UserByID(ctx context.Context, id domain.UserID) (app.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users u WHERE u.id = $1`, string(id)))
}

const membershipSelect = `
	SELECT o.id, o.name, o.created_at, m.user_id, m.role, m.created_at
	FROM memberships m JOIN organisations o ON o.id = m.org_id`

func scanMembership(row pgx.Row) (app.Membership, error) {
	var m app.Membership
	var org, user, role string
	if err := row.Scan(&org, &m.Organisation.Name, &m.Organisation.CreatedAt, &user, &role, &m.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return app.Membership{}, app.ErrNotFound
		}
		return app.Membership{}, err
	}
	m.Organisation.ID, m.UserID, m.Role = domain.OrgID(org), domain.UserID(user), domain.Role(role)
	return m, nil
}

func (s *AccountStore) Membership(ctx context.Context, org domain.OrgID, user domain.UserID) (app.Membership, error) {
	return scanMembership(s.pool.QueryRow(ctx, membershipSelect+` WHERE m.org_id = $1 AND m.user_id = $2`, string(org), string(user)))
}

func (s *AccountStore) MembershipsOf(ctx context.Context, user domain.UserID) ([]app.Membership, error) {
	rows, err := s.pool.Query(ctx, membershipSelect+` WHERE m.user_id = $1 ORDER BY m.created_at, o.id`, string(user))
	if err != nil {
		return nil, fmt.Errorf("listing memberships: %w", err)
	}
	defer rows.Close()
	var out []app.Membership
	for rows.Next() {
		m, err := scanMembership(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SessionStore persists dashboard sessions by token hash.
type SessionStore struct {
	pool *pgxpool.Pool
}

func NewSessionStore(pool *pgxpool.Pool) *SessionStore { return &SessionStore{pool: pool} }

func (s *SessionStore) Create(ctx context.Context, sess app.Session) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (id, token_hash, user_id, org_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		sess.ID, sess.TokenHash, string(sess.UserID), string(sess.OrgID), sess.CreatedAt, sess.ExpiresAt)
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	return nil
}

func (s *SessionStore) Get(ctx context.Context, tokenHash string) (app.Session, error) {
	var sess app.Session
	var user, org string
	err := s.pool.QueryRow(ctx, `
		SELECT id, token_hash, user_id, org_id, created_at, expires_at, revoked_at
		FROM sessions WHERE token_hash = $1`, tokenHash,
	).Scan(&sess.ID, &sess.TokenHash, &user, &org, &sess.CreatedAt, &sess.ExpiresAt, &sess.RevokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.Session{}, app.ErrNotFound
	}
	if err != nil {
		return app.Session{}, fmt.Errorf("loading session: %w", err)
	}
	sess.UserID, sess.OrgID = domain.UserID(user), domain.OrgID(org)
	return sess, nil
}

func (s *SessionStore) Revoke(ctx context.Context, tokenHash string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL`, tokenHash, at)
	if err != nil {
		return fmt.Errorf("revoking session: %w", err)
	}
	return nil
}

var _ app.UserStore = (*AccountStore)(nil)

func (s *AccountStore) MarkEmailVerified(ctx context.Context, user domain.UserID, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET email_verified_at = COALESCE(email_verified_at, $2) WHERE id = $1`, string(user), at)
	return err
}

func (s *AccountStore) SetPassword(ctx context.Context, user domain.UserID, hash string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE users SET password_hash = $2, password_changed_at = $3 WHERE id = $1`, string(user), hash, at)
	if err == nil && tag.RowsAffected() == 0 {
		return app.ErrNotFound
	}
	return err
}

func (s *SessionStore) RevokeAllForUser(ctx context.Context, user domain.UserID, except string, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET revoked_at = $3 WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL`,
		string(user), except, at)
	if err != nil {
		return fmt.Errorf("revoking sessions: %w", err)
	}
	return nil
}
