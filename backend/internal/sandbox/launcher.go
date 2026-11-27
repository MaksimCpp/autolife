package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	MaxInputBytes  int64 = 100_000_000
	MaxResultBytes       = 16 << 10
)

var (
	ErrUnavailable = errors.New("isolation profile unavailable")
	ErrRejected    = errors.New("inspector rejected input")
	ErrSaturated   = errors.New("launcher capacity reached")
)

var (
	imageReference = regexp.MustCompile(`^([a-z0-9][a-z0-9./:_-]*@)?sha256:[0-9a-f]{64}$`)
	instanceName   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	jobID          = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
	containerID    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type Config struct {
	DockerPath, DockerSocket, DockerConfigDir                                 string
	Instance, ImageProfile, PDFProfile, TransformProfile, PDFTransformProfile string
}

type profile struct {
	image   string
	memory  string
	timeout time.Duration
}

type command func(context.Context, io.Reader, int, ...string) ([]byte, error)

type Launcher struct {
	instance   string
	profiles   map[string]profile
	run        command
	slots      chan struct{}
	transforms transformState
}

func New(config Config) (*Launcher, error) {
	if !filepath.IsAbs(config.DockerPath) || !filepath.IsAbs(config.DockerSocket) || !filepath.IsAbs(config.DockerConfigDir) ||
		!instanceName.MatchString(config.Instance) ||
		!imageReference.MatchString(config.ImageProfile) || !imageReference.MatchString(config.PDFProfile) ||
		config.ImageProfile == config.PDFProfile ||
		(config.TransformProfile != "" && (!imageReference.MatchString(config.TransformProfile) || config.TransformProfile == config.ImageProfile || config.TransformProfile == config.PDFProfile)) {
		return nil, errors.New("launcher configuration requires local absolute paths and two distinct digest-pinned images")
	}
	run := func(ctx context.Context, input io.Reader, limit int, args ...string) ([]byte, error) {
		return docker(ctx, config, input, limit, args...)
	}
	if config.PDFTransformProfile != "" && (!imageReference.MatchString(config.PDFTransformProfile) ||
		slices.Contains([]string{config.ImageProfile, config.PDFProfile, config.TransformProfile}, config.PDFTransformProfile)) {
		return nil, errors.New("PDF transform requires a distinct digest-pinned image")
	}
	launcher := &Launcher{
		instance: config.Instance,
		run:      run,
		profiles: map[string]profile{
			"image": {image: config.ImageProfile, memory: "512m", timeout: 20 * time.Second},
			"pdf":   {image: config.PDFProfile, memory: "384m", timeout: 25 * time.Second},
		},
		slots: make(chan struct{}, 2),
	}
	if config.PDFTransformProfile != "" {
		launcher.profiles["pdf-transform"] = profile{image: config.PDFTransformProfile, memory: "768m", timeout: 90 * time.Second}
	}
	if config.TransformProfile != "" {
		launcher.profiles["image-transform"] = profile{image: config.TransformProfile, memory: "512m", timeout: 30 * time.Second}
	}
	if config.TransformProfile != "" || config.PDFTransformProfile != "" {
		launcher.transforms = transformState{dir: filepath.Join(config.DockerConfigDir, "autolife-stopped-"+config.Instance), active: make(map[string]*transformJob), mu: new(sync.Mutex)}
	}
	return launcher, nil
}

type boundedOutput struct {
	buf bytes.Buffer
	max int
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > w.max-w.buf.Len() {
		return 0, errors.New("launcher output limit exceeded")
	}
	return w.buf.Write(p)
}

func docker(ctx context.Context, cfg Config, input io.Reader, limit int, args ...string) ([]byte, error) {
	flags := []string{"--host", "unix://" + cfg.DockerSocket, "--config", cfg.DockerConfigDir}
	cmd := exec.CommandContext(ctx, cfg.DockerPath, append(flags, args...)...)
	cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	cmd.Stdin = input
	output := &boundedOutput{max: limit}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return output.buf.Bytes(), nil
}

type daemonInfo struct {
	OSType, CgroupVersion               string
	MemoryLimit, SwapLimit, CPUCfsQuota bool
	PidsLimit                           bool
	SecurityOptions                     []string
}

func supportsIsolation(info daemonInfo) bool {
	return info.OSType == "linux" && info.CgroupVersion == "2" &&
		info.MemoryLimit && info.SwapLimit && info.CPUCfsQuota && info.PidsLimit &&
		slices.Contains(info.SecurityOptions, "name=apparmor") &&
		slices.Contains(info.SecurityOptions, "name=seccomp,profile=builtin")
}

func (l *Launcher) preflight(ctx context.Context) error {
	data, err := l.run(ctx, nil, 256<<10, "info", "--format", "{{json .}}")
	if err != nil {
		return ErrUnavailable
	}
	var info daemonInfo
	if json.Unmarshal(data, &info) != nil || !supportsIsolation(info) {
		return ErrUnavailable
	}
	return nil
}

func (l *Launcher) label() string { return "io.autolife.sandbox=" + l.instance }

func (l *Launcher) managed(ctx context.Context, filters ...string) ([]string, error) {
	args := []string{"ps", "-aq", "--no-trunc", "--filter", "label=" + l.label()}
	for _, filter := range filters {
		args = append(args, "--filter", filter)
	}
	result, err := l.run(ctx, nil, 64<<10, args...)
	if err != nil {
		return nil, ErrUnavailable
	}
	ids := strings.Fields(string(result))
	for _, id := range ids {
		if !containerID.MatchString(id) {
			return nil, ErrUnavailable
		}
	}
	return ids, nil
}

func (l *Launcher) purge(ctx context.Context, filters ...string) error {
	ids, err := l.managed(ctx, filters...)
	if err != nil {
		return err
	}
	for _, id := range ids {
		// Only containers returned by the installation-specific label query qualify.
		_, _ = l.run(ctx, nil, 4096, "rm", "--force", "--volumes", id)
	}
	remaining, err := l.managed(ctx, filters...)
	if err != nil || len(remaining) != 0 {
		return ErrUnavailable
	}
	return nil
}

func (l *Launcher) Prepare(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := l.purge(ctx); err != nil {
		return err
	}
	if err := l.prepareTransforms(); err != nil {
		return err
	}
	return l.preflight(ctx)
}

func (l *Launcher) Accepts(id, kind string) bool {
	_, ok := l.profiles[kind]
	return ok && jobID.MatchString(id)
}

func (l *Launcher) settings(p profile) (string, string) {
	if transformed, ok := l.profiles["image-transform"]; ok && p.image == transformed.image {
		return "/usr/local/bin/autolife-image", "rw,noexec,nosuid,nodev,size=384m,mode=1777"
	}
	if transformed, ok := l.profiles["pdf-transform"]; ok && p.image == transformed.image {
		return "/usr/local/bin/autolife-pdf", "rw,noexec,nosuid,nodev,size=768m,mode=1777"
	}
	return "/usr/local/bin/autolife-inspect", "rw,noexec,nosuid,nodev,size=128m,mode=1777"
}

func (l *Launcher) createArgs(name, id string, p profile) []string {
	entry, workspace := l.settings(p)
	return []string{
		"create", "--name", name, "--label", l.label(), "--label", "io.autolife.job=" + id,
		"--pull", "never", "--interactive", "--read-only", "--network", "none", "--ipc", "none",
		"--user", "65534:65534", "--cap-drop", "ALL", "--security-opt", "no-new-privileges=true",
		"--security-opt", "apparmor=docker-default", "--cgroupns", "private",
		"--cpus", "1", "--memory", p.memory, "--memory-swap", p.memory, "--pids-limit", "32",
		"--log-driver", "none", "--restart", "no", "--ulimit", "nofile=128:128",
		"--tmpfs", "/work:" + workspace,
		"--workdir", "/work", "--env", "TMPDIR=/work",
		"--entrypoint", entry, p.image,
	}
}

type containerState struct {
	AppArmorProfile string
	HostConfig      struct {
		NetworkMode                  string
		IpcMode                      string
		CgroupnsMode                 string
		ReadonlyRootfs               bool
		Privileged                   bool
		SecurityOpt                  []string
		CapAdd, CapDrop              []string
		Memory, MemorySwap, NanoCpus int64
		PidsLimit                    int64
		Tmpfs                        map[string]string
	}
	Config struct {
		Volumes map[string]any
		User    string
	}
	Mounts []struct {
		Type        string
		Destination string
	}
}

func (l *Launcher) verifyContainer(ctx context.Context, name string, p profile, started bool) error {
	info, err := l.run(ctx, nil, 64<<10, "inspect", "--type", "container", "--format", "{{json .}}", name)
	var state containerState
	if err != nil || json.Unmarshal(info, &state) != nil ||
		len(state.Config.Volumes) != 0 || state.Config.User != "65534:65534" ||
		(started && state.AppArmorProfile != "docker-default") {
		return ErrUnavailable
	}
	host := state.HostConfig
	_, workspace := l.settings(p)
	megabytes, parseErr := strconv.ParseInt(strings.TrimSuffix(p.memory, "m"), 10, 64)
	noNewPrivileges := slices.Contains(host.SecurityOpt, "no-new-privileges:true") ||
		slices.Contains(host.SecurityOpt, "no-new-privileges=true")
	appArmor := slices.Contains(host.SecurityOpt, "apparmor=docker-default") ||
		slices.Contains(host.SecurityOpt, "apparmor:docker-default")
	if parseErr != nil || host.Privileged || !host.ReadonlyRootfs || host.NetworkMode != "none" ||
		host.IpcMode != "none" || host.CgroupnsMode != "private" || host.NanoCpus != 1_000_000_000 ||
		host.Memory != megabytes<<20 || host.MemorySwap != host.Memory || host.PidsLimit != 32 ||
		len(host.CapAdd) != 0 || !slices.Equal(host.CapDrop, []string{"ALL"}) ||
		len(host.SecurityOpt) != 2 || !noNewPrivileges || !appArmor ||
		len(host.Tmpfs) != 1 || host.Tmpfs["/work"] != workspace {
		return ErrUnavailable
	}
	for _, mount := range state.Mounts {
		if mount.Type != "tmpfs" || mount.Destination != "/work" {
			return ErrUnavailable
		}
	}
	return nil
}

func (l *Launcher) Inspect(ctx context.Context, id, kind string, input io.Reader, size int64) (output []byte, err error) {
	if (kind != "image" && kind != "pdf") || !l.Accepts(id, kind) || size < 1 || size > MaxInputBytes || input == nil {
		return nil, ErrRejected
	}
	data, err := l.execute(ctx, id, l.profiles[kind], input, size, MaxResultBytes)
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, ErrUnavailable
	}
	return data, nil
}

func (l *Launcher) execute(ctx context.Context, id string, p profile, input io.Reader, size int64, outputLimit int) (output []byte, err error) {
	select {
	case l.slots <- struct{}{}:
		defer func() { <-l.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		return nil, ErrSaturated
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	if err := l.preflight(ctx); err != nil {
		return nil, err
	}
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return nil, ErrUnavailable
	}
	name := "autolife-" + l.instance + "-" + hex.EncodeToString(suffix[:])
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if l.purge(stop, "name=^/"+name+"$") != nil {
			output, err = nil, ErrUnavailable
		}
	}()
	// A cancelled client cannot leave a running container. Creation is bounded
	// separately, and a failed or late create still takes the cleanup path.
	create, cancelCreate := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	_, createErr := l.run(create, nil, 4096, l.createArgs(name, id, p)...)
	cancelCreate()
	if createErr != nil || ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	if err := l.verifyContainer(ctx, name, p, false); err != nil {
		return nil, err
	}
	limited := &io.LimitedReader{R: input, N: size}
	if closer, ok := input.(io.ReadCloser); ok {
		stopRead := context.AfterFunc(ctx, func() { _ = closer.Close() })
		defer stopRead()
	}
	data, err := l.run(ctx, limited, outputLimit, "start", "--attach", "--interactive", name)
	if ctx.Err() != nil {
		return nil, ErrUnavailable
	}
	status, stateErr := l.run(ctx, nil, 100, "inspect", "--type", "container", "--format", "{{.State.ExitCode}} {{.State.Running}}", name)
	if stateErr != nil {
		return nil, ErrUnavailable
	}
	state := strings.Fields(string(status))
	if len(state) != 2 || state[1] != "false" {
		return nil, ErrUnavailable
	}
	code, parseErr := strconv.Atoi(state[0])
	if parseErr == nil && code == 20 {
		return nil, ErrRejected
	}
	if err != nil || parseErr != nil || code != 0 || limited.N != 0 || len(data) == 0 {
		return nil, ErrUnavailable
	}
	if l.verifyContainer(ctx, name, p, true) != nil {
		return nil, ErrUnavailable
	}
	return data, nil
}
