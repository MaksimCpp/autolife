package fileflow

import (
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnixInspectorProtocol(t *testing.T) {
	dir, err := os.MkdirTemp("", "autolife-socket-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "inspect.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			t.Skip("local Unix sockets restricted")
		}
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil || r.Method != http.MethodPost || r.ContentLength != int64(len(body)) ||
			r.Header.Get("Content-Type") != "application/octet-stream" || !strings.HasSuffix(r.URL.Path, "/image") {
			w.WriteHeader(400)
			return
		}
		switch string(body) {
		case "valid":
			_, _ = io.WriteString(w, `{"mime":"image/png"}`)
		case "invalid":
			w.WriteHeader(422)
		case "down":
			w.WriteHeader(503)
		default:
			_, _ = io.WriteString(w, strings.Repeat("x", 20<<10))
		}
	})}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	defer func() { _ = server.Close(); <-done }()
	inspector, err := NewUnixInspector(socket)
	if err != nil {
		t.Fatal(err)
	}
	for input, want := range map[string]error{"valid": nil, "invalid": ErrInvalid, "down": ErrUnavailable, "oversized response": ErrUnavailable} {
		mime, err := inspector.Inspect(t.Context(), "12345678-1234-4321-8123-123456789abc", "image", strings.NewReader(input), int64(len(input)))
		if !errors.Is(err, want) || (want == nil && mime != "image/png") {
			t.Fatalf("%s: got %q, %v", input, mime, err)
		}
	}
}
