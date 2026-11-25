package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store {
	return &Store{db: db}
}

type account struct {
	ID, Email, PasswordHash string
	Verified                bool
}

type session struct {
	ID, OwnerID     string
	AuthenticatedAt time.Time
}

func (s *Store) transaction(ctx context.Context, work func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.db, work)
}

func (s *Store) findAccount(ctx context.Context, email string) (account, error) {
	var user account
	err := s.db.QueryRow(ctx, `SELECT id::text, normalized_email, password_hash, email_verified_at IS NOT NULL
		FROM platform.users WHERE normalized_email = $1 AND status = 'active'`, email).
		Scan(&user.ID, &user.Email, &user.PasswordHash, &user.Verified)
	return user, err
}

func emit(ctx context.Context, tx pgx.Tx, kind, logicalKey string, payload map[string]string) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO platform.outbox_events (type, logical_key, payload) VALUES ($1, $2, $3)`, kind, logicalKey, data)
	return err
}

func (s *Store) register(ctx context.Context, email, passwordHash string, tokenHash []byte) (bool, error) {
	var inserted bool
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		var id string
		err := tx.QueryRow(ctx, `INSERT INTO platform.users (normalized_email, password_hash)
			VALUES ($1,$2) ON CONFLICT (normalized_email) DO NOTHING RETURNING id::text`, email, passwordHash).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO platform.user_settings (user_id) VALUES ($1)`, id); err != nil {
			return err
		}
		if err := insertToken(ctx, tx, id, "verify", tokenHash, 24*time.Hour); err != nil {
			return err
		}
		if err := emit(ctx, tx, "user.registered", id, map[string]string{"owner_id": id}); err != nil {
			return err
		}
		inserted = true
		return nil
	})
	return inserted && err == nil, err
}

func insertToken(ctx context.Context, tx pgx.Tx, owner, purpose string, tokenHash []byte, ttl time.Duration) error {
	_, err := tx.Exec(ctx, `INSERT INTO platform.auth_tokens (user_id,purpose,token_hash,expires_at)
		VALUES ($1,$2,$3,now()+$4*interval '1 second')`, owner, purpose, tokenHash, int64(ttl/time.Second))
	return err
}

func (s *Store) issueToken(ctx context.Context, user account, purpose string, token []byte, ttl time.Duration) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		var passwordHash string
		err := tx.QueryRow(ctx, `SELECT password_hash FROM platform.users WHERE id=$1 AND status='active'
			AND (email_verified_at IS NOT NULL)=($2='reset') FOR UPDATE`, user.ID, purpose).Scan(&passwordHash)
		if err != nil {
			return err
		}
		if passwordHash != user.PasswordHash {
			return pgx.ErrNoRows
		}
		return insertToken(ctx, tx, user.ID, purpose, token, ttl)
	})
}

func tokenOwner(ctx context.Context, tx pgx.Tx, hash []byte, purpose string) (string, error) {
	var id string
	err := tx.QueryRow(ctx, `SELECT u.id::text FROM platform.users u
		JOIN platform.auth_tokens t ON t.user_id=u.id
		WHERE t.token_hash=$1 AND t.purpose=$2 AND u.status='active' FOR UPDATE OF u`, hash, purpose).Scan(&id)
	if err != nil {
		return "", err
	}
	result, err := tx.Exec(ctx, `UPDATE platform.auth_tokens SET used_at=now()
		WHERE token_hash=$1 AND used_at IS NULL AND expires_at > now()`, hash)
	if err == nil && result.RowsAffected() != 1 {
		err = pgx.ErrNoRows
	}
	return id, err
}

func audit(ctx context.Context, tx pgx.Tx, actor, action, resource, outcome, requestID string) error {
	_, err := tx.Exec(ctx, `INSERT INTO platform.audit_events (actor,action,resource_ref,outcome,request_id)
		VALUES (nullif($1,'')::uuid,$2,nullif($3,'')::uuid,$4,nullif($5,''))`, actor, action, resource, outcome, requestID)
	return err
}

func (s *Store) verify(ctx context.Context, hash []byte, requestID string) error {
	return s.transaction(ctx, func(tx pgx.Tx) error {
		id, err := tokenOwner(ctx, tx, hash, "verify")
		if err != nil {
			return err
		}
		result, err := tx.Exec(ctx, "UPDATE platform.users SET email_verified_at=now() WHERE id=$1 AND email_verified_at IS NULL", id)
		if err != nil {
			return err
		}
		if result.RowsAffected() != 1 {
			return pgx.ErrNoRows
		}
		if _, err := tx.Exec(ctx, "UPDATE platform.auth_tokens SET used_at=now() WHERE user_id=$1 AND purpose='verify' AND used_at IS NULL", id); err != nil {
			return err
		}
		return audit(ctx, tx, id, "account.verify", id, "succeeded", requestID)
	})
}
