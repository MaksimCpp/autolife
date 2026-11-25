package accounts

import (
	"context"

	"github.com/jackc/pgx/v5"
)

func updatePassword(ctx context.Context, tx pgx.Tx, owner, hash string) error {
	if _, err := tx.Exec(ctx, "UPDATE platform.users SET password_hash=$2 WHERE id=$1", owner, hash); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE platform.sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL", owner); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "UPDATE platform.auth_tokens SET used_at=now() WHERE user_id=$1 AND purpose='reset' AND used_at IS NULL", owner)
	return err
}

func (s *Store) resetPassword(ctx context.Context, token []byte, hash, requestID string) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		owner, err := tokenOwner(ctx, tx, token, "reset")
		if err != nil {
			return err
		}
		if err := updatePassword(ctx, tx, owner, hash); err != nil {
			return err
		}
		return audit(ctx, tx, owner, "password.reset", owner, "succeeded", requestID)
	})
}

func (s *Store) sessionPassword(ctx context.Context, current session) (string, error) {
	var hash string
	err := s.db.QueryRow(ctx, "SELECT password_hash FROM platform.users WHERE id=$1 AND status='active'", current.OwnerID).Scan(&hash)
	return hash, err
}

func (s *Store) changePassword(ctx context.Context, current session, expected, hash string, rotated []byte, requestID string) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		actual, err := lockSessionOwner(ctx, tx, current)
		if err != nil {
			return err
		}
		if actual != expected {
			return pgx.ErrNoRows
		}
		if err := updatePassword(ctx, tx, current.OwnerID, hash); err != nil {
			return err
		}
		if err := createSession(ctx, tx, current.OwnerID, rotated, false); err != nil {
			return err
		}
		return audit(ctx, tx, current.OwnerID, "password.change", current.OwnerID, "succeeded", requestID)
	})
}
