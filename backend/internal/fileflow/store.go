package fileflow

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const MaxFileBytes int64 = 100_000_000

var ErrNotFound = errors.New("file resource not found")

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

type File struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"display_name"`
	MIME        *string    `json:"mime,omitempty"`
	Bytes       *int64     `json:"bytes,omitempty"`
	State       string     `json:"state"`
	CreatedAt   time.Time  `json:"created_at"`
	ReadyAt     *time.Time `json:"ready_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	ObjectKey   string     `json:"-"`
}

const fileFields = "id::text, display_name, mime, bytes, state, created_at, ready_at, expires_at, object_key"

func readFile(row pgx.Row, file *File) error {
	err := row.Scan(&file.ID, &file.DisplayName, &file.MIME, &file.Bytes, &file.State,
		&file.CreatedAt, &file.ReadyAt, &file.ExpiresAt, &file.ObjectKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) asOwner(ctx context.Context, owner string, run func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('autolife.file_owner', $1::uuid::text, true)", owner); err != nil {
			return err
		}
		return run(tx)
	})
}

func (s *Store) createUpload(ctx context.Context, owner, name string) (File, error) {
	var file File
	err := s.asOwner(ctx, owner, func(tx pgx.Tx) error {
		var active string
		err := tx.QueryRow(ctx, "SELECT id::text FROM platform.users WHERE id=$1 AND status='active' FOR UPDATE", owner).Scan(&active)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return readFile(tx.QueryRow(ctx, "INSERT INTO platform.files (owner_id,display_name) VALUES ($1,$2) RETURNING "+fileFields,
			owner, name), &file)
	})
	return file, err
}

func (s *Store) GetReady(ctx context.Context, owner, id string) (File, error) {
	var file File
	err := s.asOwner(ctx, owner, func(tx pgx.Tx) error {
		return readFile(tx.QueryRow(ctx, "SELECT "+fileFields+
			" FROM platform.files WHERE owner_id=$1 AND id=$2 AND state='ready' AND expires_at>now()",
			owner, id), &file)
	})
	return file, err
}
