package process

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"autolife/backend/internal/accounts"
	"autolife/backend/internal/platform/config"
	"autolife/backend/internal/platform/database"
	"autolife/backend/internal/platform/httpserver"
	"github.com/go-chi/chi/v5"
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
	store := accounts.NewStore(pool)
	journal, err := accounts.NewJournal(os.Getenv("AUTOLIFE_RESTORE_JOURNAL_DIR"))
	if err != nil {
		logger.Error("invalid restore journal configuration", "error", err)
		return 1
	}
	var routes []func(chi.Router)
	if service == "api" {
		accountsHTTP, err := accounts.NewHTTP(store, logger, os.Getenv("AUTOLIFE_PUBLIC_ORIGIN"), os.Getenv("AUTOLIFE_DEV_SMTP_ADDR"), os.Getenv("AUTOLIFE_AUTH_SECRET"))
		if err != nil {
			logger.Error("invalid accounts configuration", "error", err)
			return 1
		}
		accountsHTTP.UseJournal(journal)
		accountsHTTP.TrustProxy = os.Getenv("AUTOLIFE_TRUST_PROXY") == "true"
		routes = append(routes, accountsHTTP.Routes)
	}
	if service == "worker" {
		workerContext, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			store.RunPrivacy(workerContext, journal, logger)
		}()
		defer func() {
			cancel()
			<-done
		}()
	}
	handler := httpserver.Handler(logger, cfg.MaxBodyBytes, func(ctx context.Context) error {
		return database.Ready(ctx, pool)
	}, routes...)
	if err := httpserver.Run(ctx, cfg.Address, handler, logger); err != nil {
		logger.Error("HTTP server stopped unexpectedly", "error", err)
		return 1
	}
	logger.Info("graceful shutdown complete")
	return 0
}
