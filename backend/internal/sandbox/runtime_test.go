package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLinuxRuntime(t *testing.T) {
	image, pdf := os.Getenv("AUTOLIFE_PROBE_IMAGE"), os.Getenv("AUTOLIFE_PROBE_PDF")
	if image == "" || pdf == "" {
		t.Skip("requires field-test probe images on an AppArmor-enabled Linux Docker host")
	}
	const marker = "/tmp/autolife-host-probe-marker"
	if f, err := os.OpenFile(marker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600); err == nil {
		_ = f.Close()
		t.Cleanup(func() { _ = os.Remove(marker) })
	} else if !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	l, err := New(Config{
		DockerPath: os.Getenv("AUTOLIFE_TEST_DOCKER_PATH"), DockerSocket: os.Getenv("AUTOLIFE_TEST_DOCKER_SOCKET"),
		DockerConfigDir: t.TempDir(), Instance: "probe-" + strings.ReplaceAll(time.Now().Format("150405.000000000"), ".", ""),
		ImageProfile: image, PDFProfile: pdf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Prepare(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if l.purge(ctx) != nil {
			t.Error("probe container cleanup failed")
		}
	}()
	for _, kind := range []string{"image", "pdf"} {
		out, err := l.Inspect(t.Context(), fixtureID, kind, strings.NewReader("probe"), 5)
		var response struct{ Confined bool }
		if err != nil || json.Unmarshal(out, &response) != nil || !response.Confined {
			t.Fatalf("%s confinement probe failed: %v", kind, err)
		}
	}
	started := make(chan struct{})
	run := l.run
	l.run = func(ctx context.Context, in io.Reader, limit int, args ...string) ([]byte, error) {
		if args[0] == "start" {
			close(started)
		}
		return run(ctx, in, limit, args...)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := l.Inspect(ctx, fixtureID, "image", strings.NewReader("hang"), 4)
		done <- err
	}()
	select {
	case <-started:
		wait := time.NewTimer(time.Second)
		<-wait.C
	case err := <-done:
		cancel()
		t.Fatalf("probe never launched: %v", err)
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("probe startup timed out")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("expected cancelled inspection to fail: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancelled probe did not finish cleanup")
	}
	containers, err := l.managed(t.Context())
	if err != nil || len(containers) != 0 {
		t.Fatalf("probe left runtime state: %d, %v", len(containers), err)
	}
	l.run = run
	profile := l.profiles["image"]
	profile.timeout = 5 * time.Second
	l.profiles["image"] = profile
	if _, err := l.Inspect(t.Context(), fixtureID, "image", strings.NewReader("hang"), 4); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected bounded process timeout, got %v", err)
	}
	if containers, err := l.managed(t.Context()); err != nil || len(containers) != 0 {
		t.Fatalf("timeout left runtime state: %d, %v", len(containers), err)
	}

	orphan := "autolife-" + l.instance + "-restart-test"
	if _, err := run(t.Context(), nil, 4096, l.createArgs(orphan, fixtureID, profile)...); err != nil {
		t.Fatal("cannot prepare restart probe")
	}
	if err := l.Prepare(t.Context()); err != nil {
		t.Fatalf("restart reconciliation failed: %v", err)
	}
	if containers, err := l.managed(t.Context()); err != nil || len(containers) != 0 {
		t.Fatalf("restart left runtime state: %d, %v", len(containers), err)
	}
}
