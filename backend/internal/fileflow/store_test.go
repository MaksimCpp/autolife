package fileflow

import (
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFileModel(t *testing.T) {
	ctx := t.Context()
	apiURL, migrationURL := os.Getenv("AUTOLIFE_API_DATABASE_URL"), os.Getenv("AUTOLIFE_MIGRATION_DATABASE_URL")
	if apiURL == "" || migrationURL == "" {
		t.Skip("requires migrated test database credentials")
	}
	api, err := pgxpool.New(ctx, apiURL)
	if err != nil {
		t.Fatal("cannot configure API database")
	}
	defer api.Close()
	admin, err := pgxpool.New(ctx, migrationURL)
	if err != nil {
		t.Fatal("cannot configure migration database")
	}
	defer admin.Close()
	createOwner := func() string {
		t.Helper()
		var id string
		if err := admin.QueryRow(ctx, `INSERT INTO platform.users(normalized_email,password_hash)
			VALUES (gen_random_uuid()::text || '@example.test','fixture') RETURNING id::text`).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	owner, other := createOwner(), createOwner()
	defer func() {
		if _, err := admin.Exec(ctx, "DELETE FROM platform.files WHERE owner_id=$1", owner); err != nil {
			t.Error(err)
		}
		if _, err := admin.Exec(ctx, "DELETE FROM platform.users WHERE id IN ($1,$2)", owner, other); err != nil {
			t.Error(err)
		}
	}()
	store := NewStore(api)
	file, err := store.createUpload(ctx, owner, "fixture.png")
	if err != nil || file.ObjectKey != "files/"+file.ID || file.State != "uploading" {
		t.Fatalf("invalid upload allocation: %v", err)
	}
	if _, err := store.GetReady(ctx, owner, file.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("upload must remain unpublished: %v", err)
	}
	sqlState := func(err error, code string) bool {
		var dbErr *pgconn.PgError
		return errors.As(err, &dbErr) && dbErr.Code == code
	}
	if _, err := admin.Exec(ctx, "UPDATE platform.files SET state='ready' WHERE id=$1", file.ID); !sqlState(err, "23514") {
		t.Fatalf("invalid ready state: %v", err)
	}
	if _, err := admin.Exec(ctx, `UPDATE platform.files SET state='ready', mime='image/png', bytes=12,
		sha256=decode(repeat('00',32),'hex'), ready_at=now(), expires_at=now()+interval '7 days' WHERE id=$1`, file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetReady(ctx, owner, file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetReady(ctx, other, file.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign file must be hidden: %v", err)
	}
	if err := store.asOwner(ctx, other, func(tx pgx.Tx) error {
		var count int
		err := tx.QueryRow(ctx, "SELECT count(*) FROM platform.files WHERE id=$1", file.ID).Scan(&count)
		if err == nil && count != 0 {
			t.Error("RLS allowed a foreign file lookup without an owner predicate")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `INSERT INTO platform.file_pins(owner_id,file_id,expires_at)
		VALUES ($1,$2,now()+interval '1 hour')`, other, file.ID); !sqlState(err, "23503") {
		t.Fatalf("foreign pin association must fail: %v", err)
	}
}
