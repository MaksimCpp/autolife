package main

import (
	"context"
	"errors"
	"os"
	"time"

	"autolife/backend/internal/platform/config"
	"autolife/backend/migrations"
	"github.com/golang-migrate/migrate/v4"
	postgres "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func main() {
	logger := config.Logger("migrate")
	cfg, err := config.Load("AUTOLIFE_MIGRATION_DATABASE_URL")
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	if len(os.Args) == 2 && os.Args[1] == "grant-river-access" {
		if err := grantRiverAccess(cfg.DatabaseURL); err != nil {
			logger.Error("cannot grant API access to River")
			os.Exit(1)
		}
		return
	}
	if len(os.Args) != 1 {
		logger.Error("unknown migration command")
		os.Exit(1)
	}
	if err := run(cfg.DatabaseURL); err != nil {
		logger.Error("application migration failed", "error", err)
		os.Exit(1)
	}
	logger.Info("application migrations up to date")
}

func grantRiverAccess(dsn string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	// River InsertTx uses ON CONFLICT DO UPDATE on kind even for jobs without a unique key.
	_, err = conn.Exec(ctx, "GRANT UPDATE (kind) ON queue.river_job TO autolife_api")
	return err
}

func run(dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return errors.New("invalid migration database configuration")
	}
	cfg.ConnectTimeout = 5 * time.Second
	db := stdlib.OpenDB(*cfg)
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return errors.New("migration database unavailable")
	}
	driver, err := postgres.WithInstance(db, &postgres.Config{SchemaName: "platform", StatementTimeout: 30 * time.Second})
	if err != nil {
		return errors.New("cannot initialize migration tracking")
	}
	source, err := iofs.New(migrations.Files, ".")
	if err != nil {
		return errors.New("cannot open embedded migrations")
	}
	runner, err := migrate.NewWithInstance("iofs", source, "pgx", driver)
	if err != nil {
		return errors.New("cannot initialize migration runner")
	}
	defer runner.Close()
	if err := runner.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return errors.New("up migration failed; check platform.schema_migrations and PostgreSQL logs")
	}
	return nil
}
