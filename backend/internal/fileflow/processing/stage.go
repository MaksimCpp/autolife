package processing

import (
	"context"
	"crypto/sha256"
	"io"
	"os"

	"autolife/backend/internal/execution"
	"autolife/backend/internal/sandbox"
)

type Storage interface {
	Open(context.Context, string) (io.ReadCloser, int64, error)
	Put(context.Context, string, io.ReadSeeker, int64, string, [sha256.Size]byte) error
}

// Stage copies exactly one verified output into an anonymous spool, hashes it,
// and registers it with the execution's existing staging transaction.
func Stage(ctx context.Context, objects Storage, source io.Reader, size int64, name, mime string) (string, error) {
	if size < 1 || size > sandbox.MaxInputBytes {
		return "", execution.ErrInputs
	}
	tmp, err := os.CreateTemp("", "autolife-converted-*")
	if err != nil {
		return "", execution.ErrTemporary
	}
	defer tmp.Close()
	if err := os.Remove(tmp.Name()); err != nil {
		return "", execution.ErrTemporary
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, hash), io.LimitReader(source, size+1))
	if err != nil {
		return "", execution.ErrTemporary
	}
	if n != size {
		return "", execution.ErrInputs
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return "", execution.ErrTemporary
	}
	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	return execution.StageFile(ctx, execution.VerifiedFile{Name: name, MIME: mime, Bytes: n, SHA256: sum}, func(ctx context.Context, key string) error {
		if err := objects.Put(ctx, key, tmp, n, mime, sum); err != nil {
			return execution.ErrTemporary
		}
		return nil
	})
}
