package process

import (
	"context"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"autolife/backend/internal/accounts"
	"autolife/backend/internal/execution"
	"autolife/backend/internal/fileflow"
	"autolife/backend/internal/fileflow/imageflow"
	"autolife/backend/internal/fileflow/officeflow"
	"autolife/backend/internal/fileflow/pdfflow"
	"autolife/backend/internal/fileflow/processing"
	"autolife/backend/internal/fileflow/tableflow"
	"autolife/backend/internal/htmlproto"
	"autolife/backend/internal/platform/config"
	"autolife/backend/internal/platform/database"
	"autolife/backend/internal/platform/httpserver"
	"autolife/backend/internal/tableproto"
	"autolife/backend/internal/workflow"
	workflowdb "autolife/backend/internal/workflow/postgres"
	"autolife/backend/internal/workflow/workflowhttp"
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
	ownerLimit, ownerErr := fileLimit("AUTOLIFE_EXECUTION_OWNER_LIMIT", execution.OwnerLimit, 128)
	globalLimit, globalErr := fileLimit("AUTOLIFE_EXECUTION_GLOBAL_LIMIT", execution.GlobalLimit, 100_000)
	storageLimit, storageErr := fileLimit("AUTOLIFE_FILE_STORAGE_BYTES", 1_000_000_000, 1<<50)
	if ownerErr != nil || globalErr != nil || storageErr != nil {
		logger.Error("invalid execution limits")
		return 1
	}
	registry := workflow.NewRegistry()
	executions, err := execution.New(pool, registry, service == "worker")
	if err != nil {
		logger.Error("cannot configure execution queue")
		return 1
	}

	executions.Limits = execution.Limits{Owner: ownerLimit, Global: globalLimit, StorageBytes: storageLimit}
	files, err := configureFiles(pool, service == "api")
	if err != nil {
		logger.Error("invalid file service configuration")
		return 1
	}
	var imageObjects *fileflow.S3
	if files != nil {
		imageObjects, _ = files.Objects.(*fileflow.S3)
	}
	var imageLauncher *imageflow.Launcher
	if socket := os.Getenv("AUTOLIFE_IMAGE_LAUNCHER_SOCKET"); socket != "" {
		imageLauncher, err = imageflow.NewLauncher(socket)
		if err != nil {
			logger.Error("invalid image launcher configuration")
			return 1
		}
	}
	if service == "api" && files != nil && imageLauncher != nil {
		files.Previewer = imageLauncher
	}
	if imageObjects == nil || imageLauncher == nil {
		err = imageflow.Register(registry, nil, nil)
	} else {
		err = imageflow.RegisterFormats(registry, imageObjects, imageLauncher, os.Getenv("AUTOLIFE_SVG_ENABLED") == "true")
	}
	if err != nil {
		logger.Error("cannot register image actions")
		return 1
	}
	var pdfLauncher *pdfflow.Launcher
	if socket := os.Getenv("AUTOLIFE_PDF_LAUNCHER_SOCKET"); socket != "" {
		pdfLauncher, err = pdfflow.NewLauncher(socket)
		if err != nil {
			logger.Error("invalid PDF launcher configuration")
			return 1
		}
	}
	if imageObjects == nil || pdfLauncher == nil {
		err = pdfflow.Register(registry, nil, nil)
	} else {
		err = pdfflow.RegisterOperations(registry, imageObjects, pdfLauncher, os.Getenv("AUTOLIFE_PDF_RASTER_ENABLED") == "true")
	}
	if err != nil {
		logger.Error("cannot register PDF actions")
		return 1
	}
	var officeLauncher *processing.Launcher
	if socket := os.Getenv("AUTOLIFE_OFFICE_LAUNCHER_SOCKET"); socket != "" {
		officeLauncher, err = processing.NewLauncher(socket, officeflow.Profile)
		if err != nil {
			logger.Error("invalid Office launcher configuration")
			return 1
		}
	}
	if imageObjects == nil || officeLauncher == nil {
		err = officeflow.Register(registry, nil, nil)
	} else {
		err = officeflow.Register(registry, imageObjects, officeLauncher)
	}
	if err != nil {
		logger.Error("cannot register Office actions")
		return 1
	}
	var tableLauncher *processing.Launcher
	if socket := os.Getenv("AUTOLIFE_TABLE_LAUNCHER_SOCKET"); socket != "" {
		tableLauncher, err = processing.NewLauncher(socket, tableproto.ConvertProfile)
		if err != nil {
			logger.Error("invalid table launcher configuration")
			return 1
		}
	}
	if imageObjects == nil || tableLauncher == nil {
		err = tableflow.Register(registry, nil, nil)
	} else {
		err = tableflow.Register(registry, imageObjects, tableLauncher)
	}
	if err != nil {
		logger.Error("cannot register table action")
		return 1
	}
	var htmlLauncher *processing.Launcher
	if socket := os.Getenv("AUTOLIFE_HTML_LAUNCHER_SOCKET"); socket != "" {
		htmlLauncher, err = processing.NewLauncher(socket, htmlproto.ConvertProfile)
		if err != nil {
			logger.Error("invalid HTML launcher configuration")
			return 1
		}
	}
	if imageObjects == nil || htmlLauncher == nil {
		err = officeflow.RegisterHTML(registry, nil, nil)
	} else {
		err = officeflow.RegisterHTML(registry, imageObjects, htmlLauncher)
	}
	if err != nil {
		logger.Error("cannot register HTML action")
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
		workflowAPI := workflowhttp.API{Store: workflowdb.New(pool, registry), Registry: registry}
		routes = append(routes, func(r chi.Router) {
			workflowAPI.Routes(r, accountsHTTP.RequireSession, accountsHTTP.ProtectSession, accounts.SessionOwner)
			executions.Routes(r, accountsHTTP.RequireSession, accountsHTTP.ProtectSession, accounts.SessionOwner)
		})
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
		routes = append(routes, func(r chi.Router) { r.Get("/metrics", executions.Metrics) })
		if err := executions.Queue.Start(context.WithoutCancel(ctx)); err != nil {
			logger.Error("cannot start execution queue")
			return 1
		}
		defer func() {
			if executions.Stop() != nil {
				logger.Warn("execution queue shutdown timed out")
			}
		}()
		workerContext, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		executionDone := make(chan struct{})
		go func() { defer close(executionDone); executions.Maintain(workerContext, logger) }()
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
			<-executionDone
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
