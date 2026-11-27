package fileflow

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrSize   = errors.New("invalid file size")
	ErrQuota  = errors.New("file quota exceeded")
	ErrPinned = errors.New("file is pinned")
)

type Objects interface {
	Put(context.Context, string, io.ReadSeeker, int64, string, [sha256.Size]byte) error
	Delete(context.Context, string) error
	PresignDownload(context.Context, string, string, time.Time) (string, error)
}

type Service struct {
	Store             *Store
	Objects           Objects
	Inspector         Inspector
	StorageBytes      int64
	ConcurrentUploads int64
	Slots             chan struct{}
}

func (s *Service) Acquire() bool {
	select {
	case s.Slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Service) Release() { <-s.Slots }

func (s *Service) Upload(ctx context.Context, owner, name, declared string, src io.Reader, finish func() error) (File, error) {
	if s.Inspector == nil {
		return File{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	file, err := s.Store.reserve(ctx, owner, name, s.StorageBytes, s.ConcurrentUploads)
	if err != nil {
		return File{}, err
	}
	published := false
	defer func() {
		if !published {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer stop()
			_ = s.Store.abandon(cleanup, owner, file.ID)
		}
	}()
	spool, err := os.CreateTemp("", "autolife-upload-*")
	if err != nil {
		return File{}, err
	}
	defer spool.Close()
	// The open POSIX file remains usable, and even a process crash cannot
	// leave a named spool file behind.
	if err := os.Remove(spool.Name()); err != nil {
		return File{}, err
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(spool, hash), io.LimitReader(src, MaxFileBytes+1))
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return File{}, ErrInvalid
		}
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return File{}, ErrSize
		}
		return File{}, err
	}
	if size < 1 || size > MaxFileBytes {
		return File{}, ErrSize
	}
	if err := finish(); err != nil {
		return File{}, err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return File{}, err
	}
	header := make([]byte, min(size, 512))
	if _, err := io.ReadFull(spool, header); err != nil {
		return File{}, err
	}
	mime := http.DetectContentType(header)
	profile, ok := map[string]string{"image/jpeg": "image", "image/png": "image", "image/webp": "image", "application/pdf": "pdf"}[mime]
	if !ok || (declared != "" && declared != "application/octet-stream" && declared != mime) {
		return File{}, ErrType
	}
	verified, err := s.Inspector.Inspect(ctx, file.ID, profile, io.NewSectionReader(spool, 0, size), size)
	if err != nil {
		return File{}, err
	}
	if verified != mime {
		return File{}, ErrInvalid
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return File{}, err
	}
	var sum [sha256.Size]byte
	copy(sum[:], hash.Sum(nil))
	result, err := s.Store.publish(ctx, owner, file.ID, mime, size, sum, func() error {
		return s.Objects.Put(ctx, file.ObjectKey, spool, size, mime, sum)
	})
	published = err == nil
	return result, err
}

func (s *Store) reserve(ctx context.Context, owner, name string, storage, parallel int64) (File, error) {
	var file File
	err := s.asOwner(ctx, owner, func(tx pgx.Tx) error {
		if err := lockActiveOwner(ctx, tx, owner); err != nil {
			return err
		}
		var used, uploads int64
		err := tx.QueryRow(ctx, `SELECT
   (SELECT coalesce(sum(bytes),0) FROM platform.files WHERE owner_id=$1)
   + (SELECT coalesce(sum(reserved_bytes),0) FROM platform.quota_reservations WHERE owner_id=$1),
   (SELECT count(*) FROM platform.quota_reservations WHERE owner_id=$1),
   coalesce((SELECT limit_value FROM platform.quota_limits WHERE owner_id=$1 AND resource_type='storage_bytes'),$2),
   coalesce((SELECT limit_value FROM platform.quota_limits WHERE owner_id=$1 AND resource_type='concurrent_uploads'),$3)`,
			owner, storage, parallel).Scan(&used, &uploads, &storage, &parallel)
		if err != nil {
			return err
		}
		if uploads >= parallel || used > storage-MaxFileBytes {
			return ErrQuota
		}
		if err := readFile(tx.QueryRow(ctx, "INSERT INTO platform.files (owner_id,display_name) VALUES ($1,$2) RETURNING "+fileFields, owner, name), &file); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO platform.quota_reservations(owner_id,file_id,reserved_bytes,expires_at)
   VALUES ($1,$2,$3,now()+interval '5 minutes')`, owner, file.ID, MaxFileBytes)
		return err
	})
	return file, err
}

func lockActiveOwner(ctx context.Context, tx pgx.Tx, owner string) error {
	var id string
	err := tx.QueryRow(ctx, "SELECT id::text FROM platform.users WHERE id=$1 AND status='active' FOR UPDATE", owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Store) abandon(ctx context.Context, owner, id string) error {
	return s.asOwner(ctx, owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "UPDATE platform.files SET state='deleting' WHERE owner_id=$1 AND id=$2 AND state='uploading'", owner, id)
		return err
	})
}

func (s *Store) publish(ctx context.Context, owner, id, mime string, size int64, sum [sha256.Size]byte, put func() error) (File, error) {
	var file File
	err := s.asOwner(ctx, owner, func(tx pgx.Tx) error {
		if err := lockActiveOwner(ctx, tx, owner); err != nil {
			return err
		}
		if err := readFile(tx.QueryRow(ctx, "SELECT "+fileFields+` FROM platform.files
   WHERE owner_id=$1 AND id=$2 AND state='uploading' AND EXISTS
    (SELECT 1 FROM platform.quota_reservations WHERE owner_id=$1 AND file_id=$2 AND expires_at>clock_timestamp())
   FOR UPDATE`, owner, id), &file); err != nil {
			return err
		}
		if err := put(); err != nil {
			return ErrUnavailable
		}
		if err := readFile(tx.QueryRow(ctx, `UPDATE platform.files SET state='ready', mime=$3, bytes=$4, sha256=$5,
   ready_at=statement_timestamp(),expires_at=statement_timestamp()+interval '168 hours'
   WHERE owner_id=$1 AND id=$2 RETURNING `+fileFields, owner, id, mime, size, sum[:]), &file); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM platform.quota_reservations WHERE owner_id=$1 AND file_id=$2", owner, id); err != nil {
			return err
		}
		return fileEvent(ctx, tx, "file.upload_completed", id, map[string]any{"owner_id": owner, "file_id": id, "format": mime, "bytes": size, "count": 1})
	})
	return file, err
}

func fileEvent(ctx context.Context, tx pgx.Tx, kind, key string, payload map[string]any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, "INSERT INTO platform.outbox_events(type,logical_key,payload) VALUES ($1,$2,$3)", kind, key, data)
	return err
}

func (s *Service) RunCleanup(ctx context.Context, logger *slog.Logger) {
	ticker := time.NewTicker(5 * time.Second)
	orphanScan := time.NewTicker(time.Minute)
	defer orphanScan.Stop()
	var cursor *string
	defer ticker.Stop()
	for {
		for range 20 {
			cleanup, cancel := context.WithTimeout(ctx, 20*time.Second)
			found, err := s.cleanupOne(cleanup)
			cancel()
			if err != nil && ctx.Err() == nil {
				logger.Warn("file cleanup failed")
			}
			if err != nil || !found {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-orphanScan.C:
			if storage, ok := s.Objects.(*S3); ok {
				scan, cancel := context.WithTimeout(ctx, 20*time.Second)
				next, err := storage.orphanPage(scan, cursor, time.Now().Add(-time.Hour), func(id, key string) error {
					var exists bool
					if err := s.Store.pool.QueryRow(scan, "SELECT EXISTS(SELECT 1 FROM platform.files WHERE id=$1)", id).Scan(&exists); err != nil {
						return err
					}
					if exists {
						return nil
					}
					return storage.Delete(scan, key)
				})
				cancel()
				if err == nil {
					cursor = next
				} else if ctx.Err() == nil {
					logger.Warn("file orphan scan failed")
				}
			}
		}
	}
}

func (s *Service) cleanupOne(ctx context.Context) (bool, error) {
	// Claim first, then delete idempotently. A failed or interrupted object removal
	// leaves a durable deleting row and is retried before metadata can disappear.
	var id, key string
	err := pgx.BeginFunc(ctx, s.Store.pool, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT f.id::text,f.object_key FROM platform.files f
   LEFT JOIN platform.privacy_jobs j ON j.owner_id=f.owner_id AND j.type='delete' AND j.state='running'
   WHERE (f.state='deleting' OR f.state='ready' AND (f.expires_at<=now() OR j.id IS NOT NULL)
   OR f.state='uploading' AND (f.created_at<=now()-interval '5 minutes' OR j.id IS NOT NULL))
   AND NOT EXISTS (SELECT 1 FROM platform.execution_staged_files st WHERE st.file_id=f.id)
			AND NOT EXISTS (SELECT 1 FROM platform.file_pins p WHERE p.file_id=f.id AND p.owner_id=f.owner_id AND p.expires_at>clock_timestamp())
   ORDER BY f.created_at,f.id FOR UPDATE OF f SKIP LOCKED LIMIT 1`).Scan(&id, &key)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// Repeat the pin check after taking the same row lock used by pin inserts.
		var pinned bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM platform.file_pins WHERE file_id=$1 AND expires_at>clock_timestamp())", id).Scan(&pinned); err != nil {
			return err
		}
		if pinned {
			id = ""
			return nil
		}
		_, err = tx.Exec(ctx, "UPDATE platform.files SET state='deleting' WHERE id=$1", id)
		return err
	})
	if err != nil || id == "" {
		return false, err
	}
	if err := s.Objects.Delete(ctx, key); err != nil {
		return true, err
	}
	err = pgx.BeginFunc(ctx, s.Store.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "DELETE FROM platform.file_pins WHERE file_id=$1 AND expires_at<=clock_timestamp()", id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "DELETE FROM platform.files WHERE id=$1 AND state='deleting'", id)
		return err
	})
	return true, err
}
