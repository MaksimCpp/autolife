package accounts

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

const sessionTTL = 7 * 24 * time.Hour

func (s *Store) rate(ctx context.Context, scope string, key []byte, limit int) (bool, error) {
	var allowed bool
	err := s.db.QueryRow(ctx, `WITH attempt AS (
		INSERT INTO platform.auth_rate_limits (scope,key_hash,window_start,attempts)
		VALUES ($1,$2,date_bin(interval '15 minutes',now(),timestamptz '2000-01-01 UTC'),1)
		ON CONFLICT (scope,key_hash,window_start) DO UPDATE
		SET attempts=platform.auth_rate_limits.attempts+1
		WHERE platform.auth_rate_limits.attempts < $3
		RETURNING 1)
		SELECT EXISTS (SELECT FROM attempt)`, scope, key, limit).Scan(&allowed)
	return allowed, err
}

func (s *Store) getSession(ctx context.Context, token []byte) (session, error) {
	var current session
	err := s.db.QueryRow(ctx, `SELECT s.id::text,s.user_id::text,s.last_authenticated_at
		FROM platform.sessions s JOIN platform.users u ON u.id=s.user_id
		WHERE s.token_hash=$1 AND s.revoked_at IS NULL AND s.expires_at>now()
		AND u.status='active' AND u.email_verified_at IS NOT NULL`, token).
		Scan(&current.ID, &current.OwnerID, &current.AuthenticatedAt)
	return current, err
}

func createSession(ctx context.Context, tx pgx.Tx, owner string, hash []byte, login bool) error {
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO platform.sessions (user_id,token_hash,expires_at)
		VALUES ($1,$2,now()+$3*interval '1 second') RETURNING id::text`, owner, hash, int64(sessionTTL/time.Second)).Scan(&id)
	if err != nil || !login {
		return err
	}
	return emit(ctx, tx, "user.login_succeeded", id, map[string]string{"owner_id": owner, "session_id": id})
}

func (s *Store) login(ctx context.Context, user account, newHash, previousHash []byte) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		var currentHash string
		err := tx.QueryRow(ctx, `SELECT password_hash FROM platform.users WHERE id=$1
			AND status='active' AND email_verified_at IS NOT NULL FOR UPDATE`, user.ID).Scan(&currentHash)
		if err != nil {
			return err
		}
		if currentHash != user.PasswordHash {
			return pgx.ErrNoRows
		}
		if len(previousHash) > 0 {
			if _, err := tx.Exec(ctx, "UPDATE platform.sessions SET revoked_at=now() WHERE user_id=$1 AND token_hash=$2 AND revoked_at IS NULL", user.ID, previousHash); err != nil {
				return err
			}
		}
		return createSession(ctx, tx, user.ID, newHash, true)
	})
}

func lockSessionOwner(ctx context.Context, tx pgx.Tx, current session) (string, error) {
	var password string
	err := tx.QueryRow(ctx, `SELECT password_hash FROM platform.users WHERE id=$1
		AND status='active' FOR UPDATE`, current.OwnerID).Scan(&password)
	if err != nil {
		return "", err
	}
	var active bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM platform.sessions WHERE id=$1
		AND user_id=$2 AND revoked_at IS NULL AND expires_at>now())`, current.ID, current.OwnerID).Scan(&active)
	if err == nil && !active {
		err = pgx.ErrNoRows
	}
	return password, err
}

func (s *Store) revoke(ctx context.Context, current session, id, requestID string) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		if _, err := lockSessionOwner(ctx, tx, current); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `UPDATE platform.sessions SET revoked_at=coalesce(revoked_at,now())
			WHERE user_id=$1 AND id=$2`, current.OwnerID, id)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return pgx.ErrNoRows
		}
		return audit(ctx, tx, current.OwnerID, "session.revoke", id, "succeeded", requestID)
	})
}
