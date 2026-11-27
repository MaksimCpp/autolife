package execution

import (
	"context"
	"time"

	"autolife/backend/internal/workflow"
	"github.com/jackc/pgx/v5"
)

func (s *Store) List(ctx context.Context, owner, wid, status, id string, at *time.Time, n int) ([]Execution, error) {
	result := make([]Execution, 0, n)
	err := s.ownerTx(ctx, owner, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, "SELECT "+summaryColumns+",NULL::jsonb,fence"+` FROM platform.executions WHERE owner_id=$1
   AND (nullif($2,'') IS NULL OR workflow_id=nullif($2,'')::uuid)
   AND ($3='' OR status=$3) AND ($5::timestamptz IS NULL OR (queued_at,id)<($5,nullif($4,'')::uuid))
   ORDER BY queued_at DESC,id DESC LIMIT $6`, owner, wid, status, id, at, n)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e Execution
			if err := read(rows, &e); err != nil {
				return err
			}
			result = append(result, e)
		}
		return rows.Err()
	})
	return result, err
}
func (s *Store) Details(ctx context.Context, owner, id string) (Details, error) {
	var result Details
	result.Steps = []StepExecution{}
	result.Files = []FileStatus{}
	if !workflow.ValidID(id) {
		return result, workflow.ErrNotFound
	}
	err := s.ownerTx(ctx, owner, func(tx pgx.Tx) error {
		if err := read(tx.QueryRow(ctx, "SELECT "+columns+" FROM platform.executions WHERE owner_id=$1 AND id=$2", owner, id), &result.Execution); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT ordinal,step_key,action_type,action_version,status,error_code FROM platform.execution_steps
    WHERE execution_id=$1 ORDER BY ordinal`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var step StepExecution
			step.Attempts = []Attempt{}
			if err := rows.Scan(&step.Ordinal, &step.Key, &step.Action, &step.Version, &step.Status, &step.ErrorCode); err != nil {
				rows.Close()
				return err
			}
			result.Steps = append(result.Steps, step)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT ordinal,number,state,started_at,ended_at,error_code,(file_usage->>'failed_index')::int FROM platform.execution_attempts
    WHERE execution_id=$1 ORDER BY ordinal, number`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var ordinal int
			var a Attempt
			if err := rows.Scan(&ordinal, &a.Number, &a.State, &a.StartedAt, &a.EndedAt, &a.ErrorCode, &a.FailedIndex); err != nil {
				rows.Close()
				return err
			}
			result.Steps[ordinal].Attempts = append(result.Steps[ordinal].Attempts, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT r.file_id::text,r.role,
   NOT coalesce(f.state='ready' AND f.expires_at>clock_timestamp(),false)
   FROM platform.execution_file_refs r LEFT JOIN platform.files f ON f.id=r.live_file_id AND f.owner_id=r.owner_id
   WHERE r.owner_id=$1 AND r.execution_id=$2 AND (r.role='input' OR $3) ORDER BY r.role,r.file_id`, owner, id, result.Status == Succeeded)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f FileStatus
			if err := rows.Scan(&f.ID, &f.Role, &f.Expired); err != nil {
				return err
			}
			result.Files = append(result.Files, f)
		}
		return rows.Err()
	})
	return result, err
}

func (s *Store) Cancel(ctx context.Context, owner, id string) (Execution, error) {
	var e Execution
	if !workflow.ValidID(id) {
		return e, workflow.ErrNotFound
	}
	err := s.ownerTx(ctx, owner, func(tx pgx.Tx) error {
		if err := read(tx.QueryRow(ctx, "SELECT "+columns+" FROM platform.executions WHERE id=$1 AND owner_id=$2 FOR UPDATE", id, owner), &e); err != nil {
			return err
		}
		if e.Status.Terminal() || e.Status == CancelRequested {
			return nil
		}
		tag, err := tx.Exec(ctx, `UPDATE platform.executions SET status='cancel_requested' WHERE id=$1 AND owner_id=$2
    AND status IN ('queued','running','retrying') AND clock_timestamp()<coalesce(run_deadline,start_deadline)`, id, owner)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			e.Status = CancelRequested
		}
		return s.enqueue(ctx, tx, id, time.Time{})
	})
	return e, err
}

type Usage struct {
	Active              int64 `json:"active_executions"`
	Limit               int64 `json:"active_execution_limit"`
	ReservedBytes       int64 `json:"reserved_input_bytes"`
	StorageBytes        int64 `json:"stored_file_bytes"`
	UploadReservedBytes int64 `json:"reserved_upload_bytes"`
}

func (s *Store) Usage(ctx context.Context, owner string) (Usage, error) {
	result := Usage{}
	err := s.ownerTx(ctx, owner, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*),coalesce(sum(input_bytes),0),
    (SELECT coalesce(sum(bytes),0) FROM platform.files WHERE owner_id=$1),
    (SELECT coalesce(sum(reserved_bytes),0) FROM platform.quota_reservations WHERE owner_id=$1),
    coalesce((SELECT limit_value FROM platform.quota_limits WHERE owner_id=$1 AND resource_type='concurrent_executions'),$2)
    FROM platform.execution_reservations WHERE owner_id=$1`, owner, s.limits().Owner).Scan(&result.Active, &result.ReservedBytes, &result.StorageBytes, &result.UploadReservedBytes, &result.Limit)
	})
	return result, err
}

// ExportHistory returns only executions under the privacy job owner.
func ExportHistory(ctx context.Context, tx pgx.Tx, owner string) ([]byte, error) {
	var out []byte
	err := tx.QueryRow(ctx, `SELECT coalesce(jsonb_agg((to_jsonb(e)-'owner_id'-'lease_until'-'fence')
   || jsonb_build_object('steps',coalesce((SELECT jsonb_agg(to_jsonb(s)-'execution_id' ORDER BY s.ordinal)
      FROM platform.execution_steps s WHERE s.execution_id=e.id),'[]'::jsonb))
   ORDER BY e.queued_at,e.id),'[]'::jsonb)
   FROM platform.executions e WHERE e.owner_id=$1`, owner).Scan(&out)
	return out, err
}
