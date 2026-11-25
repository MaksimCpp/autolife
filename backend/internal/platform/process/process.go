package process

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"autolife/backend/internal/platform/config"
	"autolife/backend/internal/platform/database"
	"autolife/backend/internal/platform/httpserver"
)

func Start(service, databaseKey string) int {
	cfg, err := config.Load(databaseKey)
	if err != nil {
		config.Logger(service).Error("invalid configuration", "error", err)
		return 1
	}
	logger := cfg.Logger(service)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := database.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		logger.Error("cannot initialize PostgreSQL pool")
		return 1
	}
	defer pool.Close()
	handler := httpserver.Handler(logger, cfg.MaxBodyBytes, func(ctx context.Context) error {
		return database.Ready(ctx, pool)
	})
	if err := httpserver.Run(ctx, cfg.Address, handler, logger); err != nil {
		logger.Error("HTTP server stopped unexpectedly", "error", err)
		return 1
	}
	logger.Info("graceful shutdown complete")
	return 0
}
