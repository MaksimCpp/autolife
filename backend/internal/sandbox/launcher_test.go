package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const fixtureID = "12345678-1234-4321-8123-123456789abc"

func enabledDaemon() daemonInfo {
	return daemonInfo{
		OSType: "linux", CgroupVersion: "2", MemoryLimit: true, SwapLimit: true,
		CPUCfsQuota: true, PidsLimit: true, SecurityOptions: []string{"name=apparmor", "name=seccomp,profile=builtin"},
	}
}

func testLauncher(t *testing.T) *Launcher {
	t.Helper()
	l, err := New(Config{
		DockerPath: "/usr/bin/docker", DockerSocket: "/run/docker.sock", DockerConfigDir: "/srv/launcher/config", Instance: "test",
		ImageProfile: "autolife/image@sha256:" + strings.Repeat("1", 64),
		PDFProfile:   "autolife/pdf@sha256:" + strings.Repeat("2", 64),
	})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestRejectIncompleteIsolation(t *testing.T) {
	for _, missing := range []func(*daemonInfo){
		func(info *daemonInfo) { info.OSType = "windows" },
		func(info *daemonInfo) { info.CgroupVersion = "1" },
		func(info *daemonInfo) { info.MemoryLimit = false },
		func(info *daemonInfo) { info.SwapLimit = false },
		func(info *daemonInfo) { info.PidsLimit = false },
		func(info *daemonInfo) { info.CPUCfsQuota = false },
		func(info *daemonInfo) { info.SecurityOptions = []string{"name=seccomp,profile=builtin"} },
		func(info *daemonInfo) {
			info.SecurityOptions = []string{"name=apparmor", "name=seccomp,profile=unconfined"}
		},
	} {
		info := enabledDaemon()
		missing(&info)
		if supportsIsolation(info) {
			t.Fatalf("accepted incomplete isolation: %+v", info)
		}
	}
	if !supportsIsolation(enabledDaemon()) {
		t.Fatal("expected a supported daemon")
	}
	l := testLauncher(t)
	l.run = func(_ context.Context, _ io.Reader, _ int, args ...string) ([]byte, error) {
		if !slices.Equal(args, []string{"info", "--format", "{{json .}}"}) {
			t.Fatalf("runtime action without required isolation: %v", args)
		}
		return []byte(`{"OSType":"linux","CgroupVersion":"2","SecurityOptions":["name=seccomp,profile=builtin"]}`), nil
	}
	if _, err := l.Inspect(t.Context(), fixtureID, "image", strings.NewReader("x"), 1); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("expected fail-closed refusal, got %v", err)
	}
}

func profileFixture() []byte {
	const state = `{
	"AppArmorProfile":"docker-default",
	"HostConfig":{"NetworkMode":"none","IpcMode":"none","CgroupnsMode":"private","ReadonlyRootfs":true,
		"SecurityOpt":["no-new-privileges=true","apparmor=docker-default"],
		"CapDrop":["ALL"], "Memory":536870912, "MemorySwap":536870912, "NanoCpus":1000000000, "PidsLimit":32,
		"Tmpfs":{"/work":"rw,noexec,nosuid,nodev,size=128m,mode=1777"}},
	"Config":{"User":"65534:65534"},
	"Mounts":[]}`
	return []byte(state)
}

func TestLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name         string
		state        string
		output       string
		createError  bool
		unsafeMount  bool
		shortInput   bool
		cleanupFail  bool
		cancelStart  bool
		timeoutStart bool
		want         error
	}{
		{name: "success", state: "0 false", output: "{}"},
		{name: "invalid file", state: "20 false", want: ErrRejected},
		{name: "inspector error", state: "1 false", want: ErrUnavailable},
		{name: "invalid output", state: "0 false", output: "invalid", want: ErrUnavailable},
		{name: "short upload", state: "0 false", output: "{}", shortInput: true, want: ErrUnavailable},
		{name: "unsafe volume", unsafeMount: true, want: ErrUnavailable},
		{name: "late create", createError: true, want: ErrUnavailable},
		{name: "cleanup failure", state: "0 false", output: "{}", cleanupFail: true, want: ErrUnavailable},
		{name: "cancelled start", state: "137 false", cancelStart: true, want: ErrUnavailable},
		{name: "timed out start", state: "137 false", timeoutStart: true, want: ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := testLauncher(t)
			id := strings.Repeat("9", 64)
			present := false
			started := false
			cleaned := false
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			l.run = func(commandContext context.Context, body io.Reader, _ int, args ...string) ([]byte, error) {
				switch args[0] {
				case "info":
					return json.Marshal(enabledDaemon())
				case "create":
					expected := l.createArgs(args[2], fixtureID, l.profiles["image"])
					if !reflect.DeepEqual(args, expected) {
						t.Fatalf("unexpected create command: %v", args)
					}
					if !slices.Contains(args, "--read-only") || !slices.Contains(args, "--pids-limit") ||
						slices.Contains(args, "--privileged") || slices.Contains(args, "--volume") ||
						slices.Contains(args, "--mount") || slices.Contains(args, "--env-file") {
						t.Fatalf("invalid confinement arguments: %v", args)
					}
					present = true
					if tc.createError {
						return nil, context.DeadlineExceeded
					}
				case "inspect":
					if args[len(args)-2] == "{{.State.ExitCode}} {{.State.Running}}" {
						return []byte(tc.state), nil
					}
					if tc.unsafeMount {
						return []byte(strings.Replace(string(profileFixture()), `"Mounts":[]`, `"Mounts":[{"Type":"bind","Destination":"/secret"}]`, 1)), nil
					}
					return profileFixture(), nil
				case "start":
					started = true
					if tc.cancelStart {
						cancel()
						return nil, &exec.ExitError{}
					}
					if tc.timeoutStart {
						<-commandContext.Done()
						return nil, commandContext.Err()
					}
					if _, err := io.Copy(io.Discard, body); err != nil {
						return nil, err
					}
					return []byte(tc.output), nil
				case "ps":
					if commandContext.Err() != nil || !slices.Contains(args, "label=io.autolife.sandbox=test") {
						t.Fatal("cleanup must use its own bounded context and an installation label")
					}
					if present {
						return []byte(id), nil
					}
				case "rm":
					if !slices.Equal(args, []string{"rm", "--force", "--volumes", id}) {
						t.Fatalf("attempted cleanup of another container: %v", args)
					}
					cleaned = true
					present = tc.cleanupFail
				default:
					t.Fatalf("unexpected command: %v", args)
				}
				return nil, nil
			}
			if tc.timeoutStart {
				profile := l.profiles["image"]
				profile.timeout = 100 * time.Millisecond
				l.profiles["image"] = profile
			}
			size := int64(1)
			if tc.shortInput {
				size = 2
			}
			out, err := l.Inspect(ctx, fixtureID, "image", strings.NewReader("x"), size)
			if !errors.Is(err, tc.want) || (tc.want != nil && out != nil) {
				t.Fatalf("expected %v, got %v / %q", tc.want, err, out)
			}
			if tc.timeoutStart && !started {
				t.Fatal("deadline expired before the simulated handler started")
			}
			if !cleaned || (started && (tc.createError || tc.unsafeMount)) {
				t.Fatal("unsafe startup or missing cleanup")
			}
		})
	}
}

func TestRestartCleanup(t *testing.T) {
	l := testLauncher(t)
	id := strings.Repeat("8", 64)
	present := true
	l.run = func(_ context.Context, _ io.Reader, _ int, args ...string) ([]byte, error) {
		switch args[0] {
		case "ps":
			if present {
				return []byte(id), nil
			}
		case "rm":
			present = false
		case "info":
			if present {
				t.Fatal("launcher enabled before reconciliation")
			}
			return json.Marshal(enabledDaemon())
		}
		return nil, nil
	}
	if err := l.Prepare(t.Context()); err != nil || present {
		t.Fatalf("reconciliation failed: %v", err)
	}
}
