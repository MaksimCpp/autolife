package accounts

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAccountMigration(t *testing.T) {
	apiURL, migrationURL := os.Getenv("AUTOLIFE_API_DATABASE_URL"), os.Getenv("AUTOLIFE_MIGRATION_DATABASE_URL")
	if apiURL == "" || migrationURL == "" {
		t.Skip("requires an isolated migrated PostgreSQL database")
	}
	ctx := t.Context()
	api, err := pgxpool.New(ctx, apiURL)
	if err != nil {
		t.Fatal("cannot configure API pool")
	}
	defer api.Close()
	owner, err := pgxpool.New(ctx, migrationURL)
	if err != nil {
		t.Fatal("cannot configure migration pool")
	}
	defer owner.Close()

	email := fmt.Sprintf("accounts-%d@example.test", time.Now().UnixNano())
	store := NewStore(api)
	var group sync.WaitGroup
	var inserted atomic.Int32
	for range 8 {
		group.Go(func() {
			token := make([]byte, 32)
			if _, err := rand.Read(token); err != nil {
				t.Error(err)
				return
			}
			created, err := store.register(ctx, email, "fixture", token)
			if err != nil {
				t.Error(err)
			}
			if created {
				inserted.Add(1)
			}
		})
	}
	group.Wait()
	if inserted.Load() != 1 {
		t.Fatalf("created %d accounts", inserted.Load())
	}
	var id string
	if err := owner.QueryRow(ctx, "SELECT id::text FROM platform.users WHERE normalized_email=$1", email).Scan(&id); err != nil {
		t.Fatal(err)
	}
	defer owner.Exec(ctx, "DELETE FROM platform.users WHERE id=$1", id)
	defer owner.Exec(ctx, "DELETE FROM platform.outbox_events WHERE logical_key=$1", id)

	var count int
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM platform.outbox_events WHERE type='user.registered' AND logical_key=$1", id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("outbox has %d events: %v", count, err)
	}
	var token []byte
	if err := owner.QueryRow(ctx, "SELECT token_hash FROM platform.auth_tokens WHERE user_id=$1", id).Scan(&token); err != nil {
		t.Fatal(err)
	}
	rolledBackEmail := "rollback-" + email
	if _, err := store.register(ctx, rolledBackEmail, "fixture", token); !isSQLState(err, "23505") {
		t.Fatalf("expected duplicate token failure: %v", err)
	}
	if err := owner.QueryRow(ctx, "SELECT count(*) FROM platform.users WHERE normalized_email=$1", rolledBackEmail).Scan(&count); err != nil || count != 0 {
		t.Fatalf("failed transaction left %d users: %v", count, err)
	}
	abort := errors.New("abort fixture transaction")
	if err := store.transaction(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO platform.users (normalized_email,password_hash) VALUES ($1,'fixture')", rolledBackEmail); err != nil {
			return err
		}
		if err := emit(ctx, tx, "user.registered", "rollback:"+id, map[string]string{}); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatalf("aborted transaction: %v", err)
	}
	if err := owner.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM platform.users WHERE normalized_email=$1) +
		(SELECT count(*) FROM platform.outbox_events WHERE logical_key=$2)`, rolledBackEmail, "rollback:"+id).Scan(&count); err != nil || count != 0 {
		t.Fatalf("aborted transaction left %d rows: %v", count, err)
	}
	if _, err := owner.Exec(ctx, "UPDATE platform.users SET status='deleting' WHERE id=$1", id); !isSQLState(err, "23514") {
		t.Fatalf("expected account check violation: %v", err)
	}
	if _, err := owner.Exec(ctx, "INSERT INTO platform.user_settings (user_id) VALUES (gen_random_uuid())"); !isSQLState(err, "23503") {
		t.Fatalf("expected settings foreign key violation: %v", err)
	}
	if _, err := api.Exec(ctx, "SELECT id FROM platform.audit_events"); !isSQLState(err, "42501") {
		t.Fatalf("expected append-only audit permissions: %v", err)
	}
}

func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
