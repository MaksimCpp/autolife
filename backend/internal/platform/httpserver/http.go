package httpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

type requestIDKey struct{}

type apiError struct {
	Code      string   `json:"code"`
	Message   string   `json:"message"`
	Fields    []string `json:"fields"`
	RequestID string   `json:"request_id"`
}

func Handler(logger *slog.Logger, maxBodyBytes int64, ready func(context.Context) error, routes ...func(chi.Router)) http.Handler {
	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var bytes [16]byte
			_, _ = rand.Read(bytes[:])
			id := hex.EncodeToString(bytes[:])
			w.Header().Set("X-Request-ID", id)
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Cache-Control", "no-store")
			r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
			defer func() {
				if recover() != nil {
					logger.Error("request handler panicked", "request_id", id)
					WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "Unexpected server error")
				}
			}()
			if r.ContentLength > maxBodyBytes {
				WriteError(w, r, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "Request body exceeds the limit")
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
			next.ServeHTTP(w, r)
		})
	})
	router.NotFound(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.StatusNotFound, "NOT_FOUND", "Resource not found")
	})
	router.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		WriteError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "HTTP method not allowed")
	})
	router.Get("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	router.Get("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		if err := ready(r.Context()); err != nil {
			WriteError(w, r, http.StatusServiceUnavailable, "NOT_READY", "Service is not ready")
			return
		}
		WriteJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	for _, register := range routes {
		register(router)
	}
	return router
}

func RequestID(r *http.Request) string {
	id, _ := r.Context().Value(requestIDKey{}).(string)
	return id
}

func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	id := RequestID(r)
	WriteJSON(w, status, struct {
		Error apiError `json:"error"`
	}{apiError{Code: code, Message: message, Fields: []string{}, RequestID: id}})
}

func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func Run(ctx context.Context, address string, handler http.Handler, logger *slog.Logger) error {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	logger.Info("HTTP listener started", "address", listener.Addr().String())
	return serve(ctx, listener, handler)
}

func serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 2 * time.Minute, WriteTimeout: 2 * time.Minute, IdleTimeout: time.Minute,
		MaxHeaderBytes: 32 << 10,
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
