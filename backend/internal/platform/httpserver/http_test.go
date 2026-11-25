package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHealthAndErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, test := range []struct {
		name, method, path, body, contains string
		probe                              func(context.Context) error
		status                             int
	}{
		{"live", "GET", "/health/live", "", "ok", nil, 200},
		{"ready", "GET", "/health/ready", "", "ready", func(context.Context) error { return nil }, 200},
		{"unavailable", "GET", "/health/ready", "", "NOT_READY", func(context.Context) error { return errors.New("private database detail") }, 503},
		{"recovery", "GET", "/health/ready", "", "INTERNAL_ERROR", func(context.Context) error { panic("private database detail") }, 500},
		{"unknown", "GET", "/api/v1/unknown", "", "NOT_FOUND", nil, 404},
		{"method", "POST", "/health/live", "", "METHOD_NOT_ALLOWED", nil, 405},
		{"oversized", "POST", "/health/live", "12345", "PAYLOAD_TOO_LARGE", nil, 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			r.Header.Set("X-Request-ID", "untrusted-client-value")
			Handler(logger, 4, test.probe).ServeHTTP(w, r)
			body := w.Body.String()
			id := w.Header().Get("X-Request-ID")
			if w.Code != test.status || !strings.Contains(body, test.contains) || strings.Contains(body, "private database detail") ||
				len(id) != 32 || (w.Code >= 400 && !strings.Contains(body, id)) {
				t.Fatalf("status %d, request ID %q, body %s", w.Code, id, body)
			}
		})
	}
}

// A listener that blocks in Accept until shutdown closes it.
type testListener struct {
	started chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (l *testListener) Accept() (net.Conn, error) {
	close(l.started)
	<-l.closed
	return nil, net.ErrClosed
}

func (l *testListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *testListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestServeShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	l := &testListener{started: make(chan struct{}), closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- serve(ctx, l, http.NotFoundHandler()) }()

	select {
	case <-l.started:
	case <-ctx.Done():
		t.Fatal("server did not start accepting connections")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down")
	}
}
