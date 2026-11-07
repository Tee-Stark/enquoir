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

var (
	_ app.MemberStore     = (*AccountStore)(nil)
	_ app.MemberSessions  = (*SessionStore)(nil)
	_ app.InvitationStore = (*InvitationStore)(nil)
)

func (s *AccountStore) ListMembers(ctx context.Context, org domain.OrgID) ([]app.Member, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+userColumns+`, m.role, m.created_at
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.org_id = $1 ORDER BY m.created_at, u.id`, string(org))
	if err != nil {
		return nil, fmt.Errorf("listing members: %w", err)
	}
	defer rows.Close()
	var out []app.Member
	for rows.Next() {
		var m app.Member
		var id, email, role string
		if err := rows.Scan(&id, &email, &m.User.PasswordHash, &m.User.EmailVerified, &m.User.CreatedAt, &role, &m.JoinedAt); err != nil {
			return nil, err
		}
		m.User.ID, m.User.Email, m.Role = domain.UserID(id), domain.Email(email), domain.Role(role)
		m.User.PasswordHash = ""
		out = append(out, m)
	}
	return out, rows.Err()
}

// lockedMembership locks every membership of org (so owner counts
// cannot race) and returns user's role and the owner count.
func lockedMembership(ctx context.Context, tx pgx.Tx, org domain.OrgID, user domain.UserID) (domain.Role, int, error) {
	rows, err := tx.Query(ctx, `SELECT user_id, role FROM memberships WHERE org_id = $1 FOR UPDATE`, string(org))
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	var from domain.Role
	owners := 0
	for rows.Next() {
		var u, r string
		if err := rows.Scan(&u, &r); err != nil {
			return "", 0, err
		}
		if r == string(domain.Owner) {
			owners++
		}
		if u == string(user) {
			from = domain.Role(r)
		}
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	if from == "" {
		return "", 0, app.ErrNotFound
	}
	return from, owners, nil
}

func (s *AccountStore) ChangeRole(ctx context.Context, org domain.OrgID, user domain.UserID, to domain.Role, check app.MembershipCheck) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		from, owners, err := lockedMembership(ctx, tx, org, user)
		if err != nil {
			return err
		}
		if err := check(from, owners); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE memberships SET role = $3 WHERE org_id = $1 AND user_id = $2`, string(org), string(user), string(to))
		return err
	})
}

func (s *AccountStore) RemoveMember(ctx context.Context, org domain.OrgID, user domain.UserID, check app.MembershipCheck) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		from, owners, err := lockedMembership(ctx, tx, org, user)
		if err != nil {
			return err
		}
		if err := check(from, owners); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM memberships WHERE org_id = $1 AND user_id = $2`, string(org), string(user))
		return err
	})
}

func (s *SessionStore) RevokeForMember(ctx context.Context, org domain.OrgID, user domain.UserID, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE sessions SET revoked_at = $3 WHERE org_id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		string(org), string(user), at)
	return err
}

func (s *TokenStore) Peek(ctx context.Context, hash string, purpose app.TokenPurpose, now time.Time) (app.OneTimeToken, error) {
	t := app.OneTimeToken{Purpose: purpose}
	var emailID *string
	err := s.pool.QueryRow(ctx, `
		SELECT token_hash, subject, email_id, created_at, expires_at FROM one_time_tokens
		WHERE token_hash = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > $3`, hash, string(purpose), now).
		Scan(&t.Hash, &t.Subject, &emailID, &t.CreatedAt, &t.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return app.OneTimeToken{}, app.ErrTokenInvalid
	}
	if emailID != nil {
		t.EmailID = *emailID
	}
	return t, err
}

// InvitationStore keeps invitations.
type InvitationStore struct{ pool *pgxpool.Pool }

func NewInvitationStore(pool *pgxpool.Pool) *InvitationStore { return &InvitationStore{pool: pool} }

const invitationSelect = `
	SELECT i.id, i.org_id, o.name, i.email, i.role, i.invited_by, i.created_at, i.expires_at, i.accepted_at, i.revoked_at
	FROM invitations i JOIN organisations o ON o.id = i.org_id`

func scanInvitation(row pgx.Row) (app.Invitation, error) {
	var inv app.Invitation
	var org, email, role, by string
	if err := row.Scan(&inv.ID, &org, &inv.OrgName, &email, &role, &by, &inv.CreatedAt, &inv.ExpiresAt, &inv.AcceptedAt, &inv.RevokedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return app.Invitation{}, app.ErrNotFound
		}
		return app.Invitation{}, err
	}
	inv.OrgID, inv.Email, inv.Role, inv.InvitedBy = domain.OrgID(org), domain.Email(email), domain.Role(role), domain.UserID(by)
	return inv, nil
}

func (s *InvitationStore) CreateInvitation(ctx context.Context, inv app.Invitation) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE invitations SET revoked_at = $3
			WHERE org_id = $1 AND email = $2 AND accepted_at IS NULL AND revoked_at IS NULL`,
			string(inv.OrgID), string(inv.Email), inv.CreatedAt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO invitations (id, org_id, email, role, invited_by, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			inv.ID, string(inv.OrgID), string(inv.Email), string(inv.Role), string(inv.InvitedBy), inv.CreatedAt, inv.ExpiresAt)
		return err
	})
}

func (s *InvitationStore) ListPendingInvitations(ctx context.Context, org domain.OrgID, now time.Time) ([]app.Invitation, error) {
	rows, err := s.pool.Query(ctx, invitationSelect+`
		WHERE i.org_id = $1 AND i.accepted_at IS NULL AND i.revoked_at IS NULL AND i.expires_at > $2
		ORDER BY i.created_at DESC`, string(org), now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []app.Invitation
	for rows.Next() {
		inv, err := scanInvitation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *InvitationStore) GetInvitation(ctx context.Context, id string) (app.Invitation, error) {
	return scanInvitation(s.pool.QueryRow(ctx, invitationSelect+` WHERE i.id = $1`, id))
}

func (s *InvitationStore) RevokeInvitation(ctx context.Context, org domain.OrgID, id string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE invitations SET revoked_at = $3
		WHERE org_id = $1 AND id = $2 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > $3`, string(org), id, at)
	if err == nil && tag.RowsAffected() == 0 {
		return app.ErrNotFound
	}
	return err
}

func (s *InvitationStore) AcceptInvitation(ctx context.Context, cmd app.AcceptInvitationCommand) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var org, role string
		err := tx.QueryRow(ctx, `
			UPDATE invitations SET accepted_at = $2
			WHERE id = $1 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > $2
			RETURNING org_id, role`, cmd.InvitationID, cmd.At).Scan(&org, &role)
		if errors.Is(err, pgx.ErrNoRows) {
			return app.ErrTokenInvalid
		}
		if err != nil {
			return err
		}
		// Spend this invitation's link - and any other for it - in the
		// same transaction: the link works once, and only together with
		// everything else here.
		tag, err := tx.Exec(ctx, `
			UPDATE one_time_tokens SET used_at = $3
			WHERE purpose = 'invitation' AND subject = $1 AND used_at IS NULL
			  AND expires_at > $3 AND EXISTS (
				SELECT 1 FROM one_time_tokens t WHERE t.token_hash = $2 AND t.purpose = 'invitation'
				  AND t.subject = $1 AND t.used_at IS NULL AND t.expires_at > $3)`,
			cmd.InvitationID, cmd.TokenHash, cmd.At)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return app.ErrTokenInvalid
		}

		switch {
		case cmd.NewUser != nil:
			if err := insertUser(ctx, tx, *cmd.NewUser); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE users SET email_verified_at = $2 WHERE id = $1`, string(cmd.NewUser.ID), cmd.At); err != nil {
				return err
			}
		case cmd.ClaimPasswordHash != "":
			tag, err := tx.Exec(ctx, `
				UPDATE users SET password_hash = $2, password_changed_at = $3, email_verified_at = $3
				WHERE id = $1 AND email_verified_at IS NULL`, string(cmd.UserID), cmd.ClaimPasswordHash, cmd.At)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("the account was verified meanwhile; try again")
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO memberships (org_id, user_id, role, created_at) VALUES ($1, $2, $3, $4)`, org, string(cmd.UserID), role, cmd.At)
		if isUniqueViolation(err, "memberships_pkey") {
			return app.ErrAlreadyMember
		}
		return err
	})
}
