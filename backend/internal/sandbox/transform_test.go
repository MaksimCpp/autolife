package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"autolife/backend/internal/pdfproto"
)

func TestAttemptRevocationSurvivesRestart(t *testing.T) {
	config := Config{DockerPath: "/usr/bin/docker", DockerSocket: "/run/docker.sock", DockerConfigDir: t.TempDir(), Instance: "image",
		ImageProfile: "sha256:" + strings.Repeat("1", 64), PDFProfile: "sha256:" + strings.Repeat("2", 64),
		TransformProfile: "sha256:" + strings.Repeat("3", 64)}
	setup := func() *Launcher {
		t.Helper()
		launch, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := launch.prepareTransforms(); err != nil {
			t.Fatal(err)
		}
		return launch
	}
	first := setup()
	var mu sync.Mutex
	first.run = func(_ context.Context, _ io.Reader, _ int, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if args[0] != "ps" || !strings.Contains(strings.Join(args, " "), "label=io.autolife.job="+fixtureID) {
			t.Fatalf("unexpected runtime call %v", args)
		}
		return nil, nil
	}
	if err := first.ConfirmStopped(t.Context(), fixtureID); err != nil {
		t.Fatal(err)
	}
	second := setup()
	second.run = func(_ context.Context, _ io.Reader, _ int, args ...string) ([]byte, error) {
		t.Fatalf("revoked attempt reached runtime: %v", args)
		return nil, nil
	}
	if _, err := second.Transform(t.Context(), fixtureID, strings.NewReader(strings.Repeat("x", 20)), 20); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestConfirmStopsActiveTransformBeforeProof(t *testing.T) {
	cfg := Config{DockerPath: "/usr/bin/docker", DockerSocket: "/run/docker.sock", DockerConfigDir: t.TempDir(), Instance: "proof",
		ImageProfile: "sha256:" + strings.Repeat("1", 64), PDFProfile: "sha256:" + strings.Repeat("2", 64), TransformProfile: "sha256:" + strings.Repeat("3", 64)}
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.prepareTransforms(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	id := strings.Repeat("7", 64)
	present := false
	l.run = func(ctx context.Context, _ io.Reader, _ int, args ...string) ([]byte, error) {
		switch args[0] {
		case "info":
			return json.Marshal(enabledDaemon())
		case "create":
			if !slices.Contains(args, "/usr/local/bin/autolife-image") || !slices.Contains(args, "/work:rw,noexec,nosuid,nodev,size=384m,mode=1777") {
				return nil, ErrUnavailable
			}
			present = true
		case "inspect":
			return []byte(strings.ReplaceAll(string(profileFixture()), "128m", "384m")), nil
		case "start":
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		case "ps":
			if present {
				return []byte(id), nil
			}
		case "rm":
			present = false
		default:
			return nil, ErrUnavailable
		}
		return nil, nil
	}
	result := make(chan error, 1)
	go func() {
		_, err := l.Transform(ctx, fixtureID, strings.NewReader(strings.Repeat("x", 20)), 20)
		result <- err
	}()
	select {
	case <-started:
	case err := <-result:
		t.Fatalf("test transform stopped before cancellation: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := l.ConfirmStopped(ctx, fixtureID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if present {
		t.Fatal("proof reported before container cleanup")
	}
}

func TestPDFTransformProfileBounds(t *testing.T) {
	cfg := Config{DockerPath: "/usr/bin/docker", DockerSocket: "/run/docker.sock", DockerConfigDir: t.TempDir(), Instance: "pdf-test",
		ImageProfile: "sha256:" + strings.Repeat("1", 64), PDFProfile: "sha256:" + strings.Repeat("2", 64),
		PDFTransformProfile: "sha256:" + strings.Repeat("3", 64)}
	l, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.prepareTransforms(); err != nil {
		t.Fatal(err)
	}
	args := l.createArgs("fixture", fixtureID, l.profiles["pdf-transform"])
	if !slices.Contains(args, "/usr/local/bin/autolife-pdf") ||
		!slices.Contains(args, "/work:rw,noexec,nosuid,nodev,size=768m,mode=1777") ||
		!slices.Contains(args, "apparmor=docker-default") {
		t.Fatalf("incorrect PDF profile: %v", args)
	}
	l.run = func(context.Context, io.Reader, int, ...string) ([]byte, error) {
		t.Fatal("invalid request reached runtime")
		return nil, nil
	}
	if _, err := l.TransformProfile(t.Context(), fixtureID, "pdf-transform", strings.NewReader("large"), pdfproto.MaxRequest+1); !errors.Is(err, ErrRejected) {
		t.Fatal(err)
	}
	if _, err := l.Transform(t.Context(), fixtureID, strings.NewReader(strings.Repeat("x", 20)), 20); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}
