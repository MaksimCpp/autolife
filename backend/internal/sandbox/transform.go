package sandbox

import (
	"autolife/backend/internal/htmlproto"
	"autolife/backend/internal/officeproto"
	"autolife/backend/internal/pdfproto"
	"autolife/backend/internal/tableproto"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const ImageHeaderBytes = 18

type transformJob struct {
	cancel context.CancelFunc
	done   chan struct{}
}
type transformState struct {
	mu       *sync.Mutex
	dir      string
	active   map[string]*transformJob
	prunedAt time.Time
}

func (l *Launcher) prepareTransforms() error {
	state := &l.transforms
	if state.mu == nil {
		return nil
	}
	if err := os.Mkdir(state.dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return ErrUnavailable
	}
	info, err := os.Lstat(state.dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return ErrUnavailable
	}
	return nil
}

func (l *Launcher) TransformationReady(ctx context.Context, kind string) bool {
	if kind != "image-transform" && kind != "pdf-transform" && kind != "office-transform" && kind != tableproto.ConvertProfile && kind != htmlproto.ConvertProfile {
		return false
	}
	p, ok := l.profiles[kind]
	if !ok || kind == htmlproto.ConvertProfile && !l.htmlReady || l.preflight(ctx) != nil {
		return false
	}
	_, err := l.run(ctx, nil, 1024, "image", "inspect", "--format", "{{.Id}}", p.image)
	return err == nil
}

func (l *Launcher) Transform(ctx context.Context, id string, body io.Reader, size int64) ([]byte, error) {
	return l.TransformProfile(ctx, id, "image-transform", body, size)
}

func (l *Launcher) TransformProfile(ctx context.Context, id, kind string, body io.Reader, size int64) ([]byte, error) {
	var prefix, min, maxReq, maxRes int64
	switch kind {
	case "image-transform":
		prefix, min, maxReq, maxRes = ImageHeaderBytes, ImageHeaderBytes+1, MaxInputBytes+ImageHeaderBytes, MaxInputBytes
	case htmlproto.ConvertProfile:
		prefix, min, maxReq, maxRes = htmlproto.HeaderBytes, htmlproto.HeaderBytes+1, htmlproto.MaxInput+htmlproto.HeaderBytes, htmlproto.MaxOutput
	case tableproto.ConvertProfile:
		prefix, min, maxReq, maxRes = tableproto.HeaderBytes, tableproto.HeaderBytes+1, tableproto.MaxBytes+tableproto.HeaderBytes, tableproto.MaxBytes+8
	case "office-transform":
		prefix, min, maxReq, maxRes = officeproto.HeaderBytes, officeproto.HeaderBytes+1, officeproto.MaxInput+officeproto.HeaderBytes, officeproto.MaxOutput
	case "pdf-transform":
		prefix, min, maxReq, maxRes = 4, 5, pdfproto.MaxRequest, pdfproto.MaxResponse
	default:
		return nil, ErrRejected
	}
	p, ok := l.profiles[kind]
	if !ok || kind == htmlproto.ConvertProfile && !l.htmlReady {
		return nil, ErrUnavailable
	}
	if !jobID.MatchString(id) || size < min || size > maxReq || size <= prefix || body == nil {
		return nil, ErrRejected
	}
	state := &l.transforms
	state.mu.Lock()
	if _, err := os.Lstat(filepath.Join(state.dir, id)); !errors.Is(err, os.ErrNotExist) {
		state.mu.Unlock()
		return nil, ErrUnavailable
	}
	if state.active[id] != nil || len(state.active) >= cap(l.slots) {
		state.mu.Unlock()
		return nil, ErrSaturated
	}
	ctx, cancel := context.WithCancel(ctx)
	job := &transformJob{cancel: cancel, done: make(chan struct{})}
	state.active[id] = job
	state.mu.Unlock()
	defer func() {
		cancel()
		state.mu.Lock()
		delete(state.active, id)
		close(job.done)
		state.mu.Unlock()
	}()
	return l.execute(ctx, id, p, body, size, int(maxRes))
}

func (l *Launcher) ConfirmStopped(ctx context.Context, id string) error {
	if !l.Accepts(id, "image-transform") && !l.Accepts(id, "pdf-transform") && !l.Accepts(id, "office-transform") && !l.Accepts(id, tableproto.ConvertProfile) && !l.Accepts(id, htmlproto.ConvertProfile) {
		return ErrUnavailable
	}
	state := &l.transforms
	state.mu.Lock()
	err := state.markAndPrune(id, time.Now())
	job := state.active[id]
	if job != nil {
		job.cancel()
	}
	state.mu.Unlock()
	if err != nil {
		return err
	}
	if job != nil {
		select {
		case <-ctx.Done():
			return ErrUnavailable
		case <-job.done:
		}
	}
	// Never interpret an unavailable runtime as evidence that a job stopped.
	return l.purge(ctx, "label=io.autolife.job="+id)
}

func (s *transformState) markAndPrune(id string, now time.Time) error {
	name := filepath.Join(s.dir, id)
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		info, statErr := os.Lstat(name)
		if statErr != nil || !info.Mode().IsRegular() {
			return ErrUnavailable
		}
		if os.Chtimes(name, now, now) != nil {
			return ErrUnavailable
		}
	} else if err != nil {
		return ErrUnavailable
	} else {
		syncErr := file.Sync()
		closeErr := file.Close()
		if syncErr != nil || closeErr != nil {
			return ErrUnavailable
		}
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return ErrUnavailable
	}
	syncErr := dir.Sync()
	closeErr := dir.Close()
	if syncErr != nil || closeErr != nil {
		return ErrUnavailable
	}
	if now.Sub(s.prunedAt) < time.Minute {
		return nil
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return ErrUnavailable
	}
	for _, entry := range entries {
		if !jobID.MatchString(entry.Name()) || entry.Name() == id {
			continue
		}
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() && now.Sub(info.ModTime()) > 30*time.Minute {
			_ = os.Remove(filepath.Join(s.dir, entry.Name()))
		}
	}
	s.prunedAt = now
	return nil
}
