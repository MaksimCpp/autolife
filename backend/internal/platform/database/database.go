package database

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func Open(ctx context.Context, url string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL connection configuration")
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.ConnectTimeout = 3 * time.Second
	return pgxpool.NewWithConfig(ctx, cfg)
}

func Ready(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var ready bool
	if err := pool.QueryRow(ctx, "SELECT version = 10 FROM platform.schema_stamp WHERE singleton").Scan(&ready); err != nil || !ready {
		return errors.New("PostgreSQL or the application schema is unavailable")
	}
	return nil
}
