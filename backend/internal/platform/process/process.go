package process

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"autolife/backend/internal/accounts"
	"autolife/backend/internal/fileflow"
	"autolife/backend/internal/platform/config"
	"autolife/backend/internal/platform/database"
	"autolife/backend/internal/platform/httpserver"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	files, err := configureFiles(pool, service == "api")
	if err != nil {
		logger.Error("invalid file service configuration")
		return 1
	}
	if service == "api" {
		accountsHTTP, err := accounts.NewHTTP(store, logger, os.Getenv("AUTOLIFE_PUBLIC_ORIGIN"), os.Getenv("AUTOLIFE_DEV_SMTP_ADDR"), os.Getenv("AUTOLIFE_AUTH_SECRET"))
		if err != nil {
			logger.Error("invalid accounts configuration", "error", err)
			return 1
		}
		accountsHTTP.UseJournal(journal)
		accountsHTTP.TrustProxy = os.Getenv("AUTOLIFE_TRUST_PROXY") == "true"
		routes = append(routes, accountsHTTP.Routes)
		if files != nil {
			routes = append(routes, func(r chi.Router) {
				files.Routes(r, accountsHTTP.RequireSession, accountsHTTP.ProtectSession, accounts.SessionOwner)
			})
		} else {
			routes = append(routes, func(r chi.Router) {
				r.With(accountsHTTP.RequireSession, accountsHTTP.ProtectSession).Post("/api/v1/files", func(w http.ResponseWriter, r *http.Request) {
					httpserver.WriteError(w, r, 503, "FILE_UNAVAILABLE", "File service unavailable")
				})
			})
		}
	}
	if service == "worker" {
		workerContext, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			store.RunPrivacy(workerContext, journal, logger)
		}()
		var fileDone chan struct{}
		if files != nil {
			fileDone = make(chan struct{})
			go func() { defer close(fileDone); files.RunCleanup(workerContext, logger) }()
		}
		defer func() {
			cancel()
			<-done
			if fileDone != nil {
				<-fileDone
			}
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

func configureFiles(pool *pgxpool.Pool, api bool) (*fileflow.Service, error) {
	endpoint := os.Getenv("AUTOLIFE_S3_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}
	objects, err := fileflow.NewS3(fileflow.S3Config{
		Endpoint: endpoint, PublicEndpoint: os.Getenv("AUTOLIFE_S3_PUBLIC_ENDPOINT"),
		Bucket: "autolife", Region: "us-east-1", AccessKey: os.Getenv("AUTOLIFE_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("AUTOLIFE_S3_SECRET_KEY"), AllowHTTP: os.Getenv("AUTOLIFE_S3_ALLOW_HTTP") == "true",
	})
	if err != nil {
		return nil, err
	}
	files := &fileflow.Service{Store: fileflow.NewStore(pool), Objects: objects}
	if !api {
		return files, nil
	}
	files.StorageBytes, err = fileLimit("AUTOLIFE_FILE_STORAGE_BYTES", 1_000_000_000, 1<<50)
	if err != nil {
		return nil, err
	}
	files.ConcurrentUploads, err = fileLimit("AUTOLIFE_FILE_OWNER_UPLOADS", 2, 128)
	if err != nil {
		return nil, err
	}
	slots, err := fileLimit("AUTOLIFE_FILE_UPLOAD_SLOTS", 4, 128)
	if err != nil {
		return nil, err
	}
	files.Slots = make(chan struct{}, slots)
	if socket := os.Getenv("AUTOLIFE_FILE_INSPECTOR_SOCKET"); socket != "" {
		files.Inspector, err = fileflow.NewUnixInspector(socket)
	}
	return files, err
}

func fileLimit(key string, fallback, ceiling int64) (int64, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 1 || value > ceiling {
		return 0, fileflow.ErrUnavailable
	}
	return value, nil
}
