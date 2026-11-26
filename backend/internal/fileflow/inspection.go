package fileflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"autolife/backend/internal/sandbox"
)

var (
	ErrUnavailable = errors.New("file processing unavailable")
	ErrInvalid     = errors.New("file inspection rejected input")
	ErrType        = errors.New("unsupported file type")
)

type Inspector interface {
	Inspect(context.Context, string, string, io.Reader, int64) (string, error)
}

type UnixInspector struct{ client *http.Client }

func NewUnixInspector(socket string) (*UnixInspector, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return nil, ErrUnavailable
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	return &UnixInspector{client: &http.Client{
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

func (i *UnixInspector) Inspect(ctx context.Context, id, profile string, body io.Reader, size int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://launcher/v1/jobs/"+id+"/"+profile, body)
	if err != nil {
		return "", ErrUnavailable
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := i.client.Do(req)
	if err != nil {
		return "", ErrUnavailable
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnprocessableEntity {
		return "", ErrInvalid
	}
	if res.StatusCode != http.StatusOK {
		return "", ErrUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, sandbox.MaxResultBytes+1))
	var result struct{ MIME string }
	if err != nil || len(data) > sandbox.MaxResultBytes || json.Unmarshal(data, &result) != nil || result.MIME == "" {
		return "", ErrUnavailable
	}
	return result.MIME, nil
}
