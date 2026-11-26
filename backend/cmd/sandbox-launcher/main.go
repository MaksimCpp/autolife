package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"autolife/backend/internal/sandbox"
	"golang.org/x/sys/unix"
)

func main() {
	config := sandbox.Config{}
	socket := flag.String("socket", "", "Unix socket for the worker")
	flag.StringVar(&config.DockerPath, "docker", "/usr/bin/docker", "operator-managed Docker executable")
	flag.StringVar(&config.DockerSocket, "docker-socket", "/var/run/docker.sock", "local operator Docker socket")
	flag.StringVar(&config.DockerConfigDir, "docker-config", "", "private operator Docker configuration directory")
	flag.StringVar(&config.Instance, "instance", "", "unique installation identifier")
	flag.StringVar(&config.ImageProfile, "image", "", "digest-pinned image inspection image")
	flag.StringVar(&config.PDFProfile, "pdf", "", "digest-pinned PDF inspection image")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, config, *socket); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func serve(ctx context.Context, config sandbox.Config, socket string) error {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return errors.New("launcher socket must have an absolute normalized path")
	}
	parent, err := os.Stat(filepath.Dir(socket))
	if err != nil || !parent.IsDir() || parent.Mode().Perm()&0022 != 0 {
		return errors.New("launcher needs an existing operator-controlled socket directory")
	}
	launcher, err := sandbox.New(config)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(socket), config.Instance+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return errors.New("launcher lock unavailable")
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("launcher instance already active")
	}
	if err := launcher.Prepare(ctx); err != nil {
		return err
	}
	if existing, err := os.Lstat(socket); err == nil {
		if existing.Mode()&os.ModeSocket == 0 || os.Remove(socket) != nil {
			return errors.New("launcher socket path is already occupied")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("launcher cannot inspect socket path")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return errors.New("cannot listen on launcher socket")
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0660); err != nil {
		return errors.New("cannot restrict launcher socket")
	}

	routes := http.NewServeMux()
	routes.HandleFunc("POST /v1/jobs/{id}/{profile}", func(w http.ResponseWriter, r *http.Request) {
		id, profile := r.PathValue("id"), r.PathValue("profile")
		if !launcher.Accepts(id, profile) || r.URL.RawQuery != "" || r.Header.Get("Content-Type") != "application/octet-stream" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.ContentLength < 1 || r.ContentLength > sandbox.MaxInputBytes {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		output, err := launcher.Inspect(r.Context(), id, profile, r.Body, r.ContentLength)
		switch {
		case errors.Is(err, sandbox.ErrRejected):
			w.WriteHeader(http.StatusUnprocessableEntity)
		case err != nil:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(output)
		}
	})
	server := &http.Server{
		Handler: routes, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 35 * time.Second,
		WriteTimeout: 50 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return errors.New("launcher shutdown timeout")
		}
		return nil
	}
}
