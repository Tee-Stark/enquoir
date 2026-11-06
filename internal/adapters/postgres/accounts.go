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
		_, err := tx.Exec(ctx, `
			INSERT INTO users (id, org_id, email, password_hash, created_at)
			VALUES ($1, $2, $3, $4, $5)`,
			string(owner.ID), string(owner.OrgID), string(owner.Email), owner.PasswordHash, owner.CreatedAt)
		if isUniqueViolation(err, "users_email_key") {
			return app.ErrEmailTaken
		}
		if err != nil {
			return fmt.Errorf("creating user: %w", err)
		}
		return nil
	})
}

const userColumns = `u.id, u.org_id, u.email, u.password_hash, u.email_verified_at IS NOT NULL, u.created_at`

func scanUser(row pgx.Row, extra ...any) (app.User, error) {
	var u app.User
	var id, org, email string
	dest := append([]any{&id, &org, &email, &u.PasswordHash, &u.EmailVerified, &u.CreatedAt}, extra...)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return app.User{}, app.ErrNotFound
		}
		return app.User{}, err
	}
	u.ID, u.OrgID, u.Email = domain.UserID(id), domain.OrgID(org), domain.Email(email)
	return u, nil
}

func (s *AccountStore) UserByEmail(ctx context.Context, email domain.Email) (app.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `SELECT `+userColumns+` FROM users u WHERE u.email = $1`, string(email)))
}

func (s *AccountStore) UserWithOrganisation(ctx context.Context, id domain.UserID) (app.User, app.Organisation, error) {
	var org app.Organisation
	user, err := scanUser(s.pool.QueryRow(ctx, `
		SELECT `+userColumns+`, o.name, o.created_at
		FROM users u JOIN organisations o ON o.id = u.org_id
		WHERE u.id = $1`, string(id)), &org.Name, &org.CreatedAt)
	if err != nil {
		return app.User{}, app.Organisation{}, err
	}
	org.ID = user.OrgID
	return user, org, nil
}

// SessionStore persists dashboard sessions by token hash.
type SessionStore struct {
	pool *pgxpool.Pool
}

func NewSessionStore(pool *pgxpool.Pool) *SessionStore { return &SessionStore{pool: pool} }

func (s *SessionStore) Create(ctx context.Context, sess app.Session) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sessions (token_hash, user_id, org_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)`,
		sess.TokenHash, string(sess.UserID), string(sess.OrgID), sess.CreatedAt, sess.ExpiresAt)
	if err != nil {
		return fmt.Errorf("creating session: %w", err)
	}
	return nil
}

func (s *SessionStore) Get(ctx context.Context, tokenHash string) (app.Session, error) {
	var sess app.Session
	var user, org string
	err := s.pool.QueryRow(ctx, `
		SELECT token_hash, user_id, org_id, created_at, expires_at, revoked_at
		FROM sessions WHERE token_hash = $1`, tokenHash,
	).Scan(&sess.TokenHash, &user, &org, &sess.CreatedAt, &sess.ExpiresAt, &sess.RevokedAt)
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
