package riversmoke

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

type smokeJob struct{}

func (smokeJob) Kind() string { return "autolife_migration_smoke" }

func TestInsertTx(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	api := openPool(t, ctx, "AUTOLIFE_API_DATABASE_URL")
	worker := openPool(t, ctx, "AUTOLIFE_WORKER_DATABASE_URL")
	client, err := river.NewClient(riverpgxv5.New(api), &river.Config{
		Schema: "queue", SkipUnknownJobCheck: true,
	})
	if err != nil {
		t.Fatal("cannot initialize River client")
	}

	for _, name := range []string{"commit", "rollback"} {
		t.Run(name, func(t *testing.T) {
			tx, err := api.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx)

			result, err := client.InsertTx(ctx, tx, smokeJob{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			id := result.Job.ID
			checkVisible(t, ctx, worker, id, false)
			if name == "commit" {
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if _, err := worker.Exec(cleanup, "DELETE FROM queue.river_job WHERE id = $1", id); err != nil {
						t.Error("could not clean up smoke job")
					}
				})
				checkVisible(t, ctx, worker, id, true)
			} else {
				if err := tx.Rollback(ctx); err != nil {
					t.Fatal(err)
				}
				checkVisible(t, ctx, worker, id, false)
			}
		})
	}
}

func openPool(t *testing.T, ctx context.Context, key string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv(key)
	if dsn == "" {
		t.Fatalf("%s required", key)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("invalid %s", key)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("database unavailable for %s", key)
	}

	var version int
	if err := pool.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil || version/10_000 != 17 {
		t.Fatalf("expected PostgreSQL 17, got %d (%v)", version, err)
	}
	return pool
}

func checkVisible(t *testing.T, ctx context.Context, db *pgxpool.Pool, id int64, want bool) {
	t.Helper()
	var found int64
	err := db.QueryRow(ctx, "SELECT id FROM queue.river_job WHERE id = $1", id).Scan(&found)
	if (want && (err != nil || found != id)) || (!want && err != pgx.ErrNoRows) {
		t.Fatalf("job visibility should be %v: %v", want, err)
	}
}
