package imageflow

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"autolife/backend/internal/execution"
	"autolife/backend/internal/sandbox"
	"autolife/backend/internal/workflow"
)

type Launcher struct {
	http    *http.Client
	mu      sync.Mutex
	checked time.Time
	ready   bool
}

func NewLauncher(socket string) (*Launcher, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return nil, errors.New("invalid image launcher socket")
	}
	dialer := net.Dialer{Timeout: time.Second}
	return &Launcher{http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socket)
			},
			DisableKeepAlives: true,
		},
		Timeout:       55 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (l *Launcher) Available() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.checked) < 2*time.Second {
		return l.ready
	}
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	l.ready, _ = l.status(ctx, http.MethodGet, "ready")
	l.checked = time.Now()
	return l.ready
}

func (l *Launcher) ConfirmStopped(ctx context.Context, id string) (bool, error) {
	if !workflow.ValidID(id) {
		return false, execution.ErrInputs
	}
	return l.status(ctx, http.MethodDelete, id)
}
func (l *Launcher) status(ctx context.Context, method, id string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://launcher/v1/image-transform/"+id, nil)
	if err != nil {
		return false, err
	}
	res, err := l.http.Do(req)
	if err != nil {
		return false, execution.ErrTemporary
	}
	defer res.Body.Close()
	return res.StatusCode == http.StatusNoContent, nil
}

func (l *Launcher) Transform(ctx context.Context, id string, header []byte, source io.Reader, size int64) (io.ReadCloser, int64, error) {
	if !workflow.ValidID(id) || len(header) != sandbox.ImageHeaderBytes || size < 1 || size > sandbox.MaxInputBytes {
		return nil, 0, execution.ErrInputs
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://launcher/v1/image-transform/"+id, io.MultiReader(bytes.NewReader(header), source))
	if err != nil {
		return nil, 0, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, 0, execution.ErrInputs
	}
	req.ContentLength = size + int64(len(header))
	req.Header.Set("X-AutoLife-Deadline", strconv.FormatInt(deadline.UnixMilli(), 10))
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := l.http.Do(req)
	if err != nil {
		return nil, 0, execution.ErrTemporary
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		if res.StatusCode == http.StatusUnprocessableEntity {
			return nil, 0, execution.ErrInputs
		}
		return nil, 0, execution.ErrTemporary
	}
	if res.ContentLength < 1 || res.ContentLength > sandbox.MaxInputBytes || res.Header.Get("Content-Type") != "application/octet-stream" {
		res.Body.Close()
		return nil, 0, execution.ErrInputs
	}
	return res.Body, res.ContentLength, nil
}
