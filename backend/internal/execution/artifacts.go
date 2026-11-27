package execution

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

type attemptScope struct {
	store           *Store
	execution       Execution
	ordinal, number int
}
type attemptKey struct{}

type VerifiedFile struct {
	Name   string
	MIME   string
	Bytes  int64
	SHA256 [sha256.Size]byte
}

// StageFile is called by a registered handler only after validating a sandbox
// output. write receives a server-generated private object key and must write
// exactly the inspected bytes. The ID cannot be downloaded until commit.
func StageFile(ctx context.Context, file VerifiedFile, write func(context.Context, string) error) (string, error) {
	scope, ok := ctx.Value(attemptKey{}).(attemptScope)
	if !ok || write == nil {
		return "", ErrLease
	}
	if file.Bytes < 1 || file.Bytes > 100_000_000 || !utf8.ValidString(file.Name) || utf8.RuneCountInString(file.Name) > 255 ||
		strings.TrimSpace(file.Name) != file.Name || file.Name == "" || file.Name == "." || file.Name == ".." ||
		strings.ContainsAny(file.Name, "/\\") || strings.IndexFunc(file.Name, func(ch rune) bool { return unicode.IsControl(ch) || unicode.Is(unicode.Cf, ch) }) >= 0 {
		return "", ErrInputs
	}
	e := scope.execution
	var id, key string
	err := pgx.BeginFunc(ctx, scope.store.Pool, func(tx pgx.Tx) error {
		var active bool
		if err := tx.QueryRow(ctx, "SELECT platform.lock_execution_storage_owner($1)", e.OwnerID).Scan(&active); err != nil {
			return err
		}
		if !active {
			return ErrLease
		}
		state, err := ownedState(ctx, tx, e)
		if err != nil {
			return err
		}
		if state != Running {
			return ErrLease
		}
		var bytesUsed, storage, storageLimit int64
		var files int
		err = tx.QueryRow(ctx, `SELECT count(*),coalesce(sum(f.bytes),0),
   (SELECT coalesce(sum(bytes),0) FROM platform.files WHERE owner_id=$2)
   +(SELECT coalesce(sum(reserved_bytes),0) FROM platform.quota_reservations WHERE owner_id=$2),
   coalesce((SELECT limit_value FROM platform.quota_limits WHERE owner_id=$2 AND resource_type='storage_bytes'),$3)
   FROM platform.execution_staged_files st JOIN platform.files f ON f.id=st.file_id WHERE st.execution_id=$1`,
			e.ID, e.OwnerID, scope.store.limits().StorageBytes).Scan(&files, &bytesUsed, &storage, &storageLimit)
		if err != nil {
			return err
		}
		if files >= 500 || bytesUsed > MaxBytes-file.Bytes || storage > storageLimit-file.Bytes {
			return ErrQuota
		}
		if err := tx.QueryRow(ctx, `INSERT INTO platform.files(owner_id,display_name,mime,bytes,sha256)
    VALUES($1,$2,$3,$4,$5) RETURNING id::text,object_key`, e.OwnerID, file.Name, file.MIME, file.Bytes, file.SHA256[:]).Scan(&id, &key); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO platform.execution_staged_files(owner_id,execution_id,ordinal,attempt,fence,file_id)
   SELECT $1,$2,$3,$4,$5,$6 FROM platform.execution_attempts WHERE execution_id=$2 AND ordinal=$3 AND number=$4 AND fence=$5 AND state='running'`,
			e.OwnerID, e.ID, scope.ordinal, scope.number, e.Fence, id)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrLease
		}
		return err
	})
	if err != nil {
		return "", err
	}
	if err := write(ctx, key); err != nil {
		return "", err
	}
	err = pgx.BeginFunc(ctx, scope.store.Pool, func(tx pgx.Tx) error {
		state, err := ownedState(ctx, tx, e)
		if err != nil {
			return err
		}
		if state != Running {
			return ErrLease
		}
		tag, err := tx.Exec(ctx, `UPDATE platform.execution_staged_files SET uploaded=true WHERE file_id=$1 AND execution_id=$2 AND fence=$3`, id, e.ID, e.Fence)
		if err == nil && tag.RowsAffected() != 1 {
			return ErrLease
		}
		return err
	})
	return id, err
}

func discardStages(ctx context.Context, tx pgx.Tx, id string, unfinishedOnly bool) error {
	if _, err := tx.Exec(ctx, `UPDATE platform.files SET state='deleting'
  WHERE id IN(SELECT file_id FROM platform.execution_staged_files WHERE execution_id=$1 AND (NOT $2 OR NOT checkpoint))`, id, unfinishedOnly); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, "DELETE FROM platform.execution_staged_files WHERE execution_id=$1 AND (NOT $2 OR NOT checkpoint)", id, unfinishedOnly)
	return err
}

func checkpointArtifacts(ctx context.Context, tx pgx.Tx, e Execution, ordinal, n int, refs []string) error {
	for _, id := range refs {
		tag, err := tx.Exec(ctx, `UPDATE platform.execution_staged_files SET checkpoint=true
   WHERE execution_id=$1 AND file_id=$2 AND uploaded AND (checkpoint OR (ordinal=$3 AND attempt=$4 AND fence=$5))`, e.ID, id, ordinal, n, e.Fence)
		if err != nil {
			return err
		}
		var pin *string
		if tag.RowsAffected() == 0 {
			var existing bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM platform.execution_file_refs
    WHERE execution_id=$1 AND file_id=$2 AND pin_id IS NOT NULL)`, e.ID, id).Scan(&existing); err != nil {
				return err
			}
			if !existing {
				var newPin string
				err := tx.QueryRow(ctx, `INSERT INTO platform.file_pins(owner_id,file_id,expires_at) VALUES($1,$2,'infinity') RETURNING id::text`, e.OwnerID, id).Scan(&newPin)
				if err != nil {
					return err
				}
				pin = &newPin
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO platform.execution_file_refs(owner_id,execution_id,file_id,live_file_id,pin_id,role)
   VALUES($1,$2,$3,$3,$4,'output') ON CONFLICT DO NOTHING`, e.OwnerID, e.ID, id, pin); err != nil {
			return err
		}
	}
	var total int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(f.bytes),0) FROM platform.execution_file_refs r
  JOIN platform.files f ON f.id=r.live_file_id AND f.owner_id=r.owner_id WHERE r.execution_id=$1 AND r.role='output'`, e.ID).Scan(&total); err != nil {
		return err
	}
	if total > MaxBytes {
		return ErrQuota
	}
	return discardStages(ctx, tx, e.ID, true)
}

// ObjectKey resolves an owner-scoped input or prior staged checkpoint for the
// current handler. The key is never returned by the public execution API.
func ObjectKey(ctx context.Context, id string) (string, error) {
	meta, err := ObjectMetadata(ctx, id)
	return meta.Key, err
}

type Object struct {
	Key   string
	Bytes int64
	MIME  string
}

func ObjectMetadata(ctx context.Context, id string) (Object, error) {
	scope, ok := ctx.Value(attemptKey{}).(attemptScope)
	if !ok {
		return Object{}, ErrLease
	}
	var result Object
	err := scope.store.Pool.QueryRow(ctx, `SELECT f.object_key,f.bytes,f.mime FROM platform.files f
  JOIN platform.execution_file_refs r ON r.live_file_id=f.id AND r.owner_id=f.owner_id
  WHERE r.execution_id=$1 AND r.owner_id=$2 AND r.file_id=$3 AND (
   f.state='ready' OR EXISTS(SELECT 1 FROM platform.execution_staged_files st WHERE st.execution_id=$1 AND st.file_id=$3 AND checkpoint)) LIMIT 1`,
		scope.execution.ID, scope.execution.OwnerID, id).Scan(&result.Key, &result.Bytes, &result.MIME)
	if errors.Is(err, pgx.ErrNoRows) {
		return Object{}, ErrInputs
	}
	return result, err
}

// AttemptID lets a registered handler correlate its private launcher job with
// the durable attempt, including after process restart.
func AttemptID(ctx context.Context) (string, bool) {
	scope, ok := ctx.Value(attemptKey{}).(attemptScope)
	if !ok {
		return "", false
	}
	return attemptID(scope.execution, scope.ordinal, scope.number), true
}

func attemptID(e Execution, ordinal, n int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d:%d", e.ID, ordinal, n, e.Fence)))
	sum[6] = sum[6]&0x0f | 0x40
	sum[8] = sum[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", sum[:4], sum[4:6], sum[6:8], sum[8:10], sum[10:16])
}
