package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"autolife/backend/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/riverqueue/river"
)

type Worker struct {
	river.WorkerDefaults[Job]
	Store *Store
}

type plan struct {
	Execution
	definition workflow.Definition
	data       workflow.BindingData
}

func (w *Worker) Work(ctx context.Context, job *river.Job[Job]) error {
	if !workflow.ValidID(job.Args.ID) {
		return nil
	}
	return w.Store.run(ctx, job.Args.ID)
}

func (s *Store) run(ctx context.Context, id string) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	var locked bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1,511))", id).Scan(&locked); err != nil || !locked {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, err := conn.Exec(c, "SELECT pg_advisory_unlock(hashtextextended($1,511))", id); err != nil {
			_ = conn.Conn().Close(c)
		}
	}()
	var p plan
	var raw json.RawMessage
	claimed := false
	err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		if err := read(tx.QueryRow(ctx, "SELECT "+columns+" FROM platform.executions WHERE id=$1 FOR UPDATE", id), &p.Execution); err != nil {
			if errors.Is(err, workflow.ErrNotFound) {
				return nil
			}
			return err
		}
		if p.Status.Terminal() {
			return nil
		}
		var now time.Time
		var lease, retry *time.Time
		if err := tx.QueryRow(ctx, "SELECT clock_timestamp(),lease_until,retry_at FROM platform.executions WHERE id=$1", id).Scan(&now, &lease, &retry); err != nil {
			return err
		}
		if lease != nil && now.Before(*lease) {
			return nil
		}
		if p.Status == Retrying && retry != nil && now.Before(*retry) {
			return nil
		}
		if p.Status != CancelRequested && p.StartedAt == nil && now.Before(p.Deadline()) {
			var ownerActive bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM platform.users WHERE id=$1 AND status='active')", p.OwnerID).Scan(&ownerActive); err != nil {
				return err
			}
			if !ownerActive {
				return nil
			}
		}
		err := tx.QueryRow(ctx, `SELECT coalesce(
      (SELECT definition FROM platform.workflow_versions WHERE id=e.version_id AND owner_id=e.owner_id AND workflow_id=e.workflow_id),
      (SELECT definition FROM platform.draft_execution_snapshots WHERE id=e.snapshot_id AND owner_id=e.owner_id AND workflow_id=e.workflow_id),
      (SELECT definition FROM platform.quick_execution_snapshots WHERE id=e.quick_snapshot_id AND owner_id=e.owner_id)),
      trigger_values,user_inputs FROM platform.executions e WHERE e.id=$1`, id).
			Scan(&raw, &p.data.Trigger, &p.data.UserInputs)
		if err != nil {
			return err
		}
		p.definition, err = workflow.DecodeDefinition(raw)
		if err != nil {
			return err
		}
		// Once an advisory lock is acquired, any surviving attempt must be treated
		// as interrupted. Non-replayable work retains its reservations for reconciliation.
		var ordinal, number int
		err = tx.QueryRow(ctx, "SELECT ordinal,number FROM platform.execution_attempts WHERE execution_id=$1 AND state='running' LIMIT 1", id).Scan(&ordinal, &number)
		if err == nil {
			step := p.definition.Steps[ordinal]
			maxAttempts, safe, _ := s.Registry.ExecutionPolicy(step)
			proof, stop := context.WithTimeout(ctx, 3*time.Second)
			stopped, err := s.Registry.ConfirmStopped(proof, step, attemptID(p.Execution, ordinal, number))
			stop()
			if err != nil {
				return err
			}
			if !stopped {
				return nil
			}
			if !safe && p.Status == Running && now.Before(p.Deadline()) {
				if _, err := tx.Exec(ctx, `UPDATE platform.execution_attempts SET state='unknown_outcome',
				 error_code='UNKNOWN_OUTCOME',ended_at=clock_timestamp() WHERE execution_id=$1 AND ordinal=$2 AND number=$3`, id, ordinal, number); err != nil {
					return err
				}
				return finishTx(ctx, tx, p.Execution, Failed, "UNKNOWN_OUTCOME", nil)
			}
			if _, err := tx.Exec(ctx, `UPDATE platform.execution_attempts SET state='interrupted',ended_at=clock_timestamp()
     WHERE execution_id=$1 AND ordinal=$2 AND number=$3`, id, ordinal, number); err != nil {
				return err
			}
			if err := discardStages(ctx, tx, id, true); err != nil {
				return err
			}
			if number >= maxAttempts && p.Status == Running && now.Before(p.Deadline()) {
				return finishTx(ctx, tx, p.Execution, Failed, "ATTEMPTS_EXHAUSTED", nil)
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if p.Status == CancelRequested {
			return finishTx(ctx, tx, p.Execution, Cancelled, "CANCELLED", nil)
		}
		if !now.Before(p.Deadline()) {
			return finishTx(ctx, tx, p.Execution, TimedOut, "ACTION_TIMEOUT", nil)
		}
		if p.Status == Retrying {
			if _, err := tx.Exec(ctx, "UPDATE platform.executions SET status='queued' WHERE id=$1 AND status='retrying'", id); err != nil {
				return err
			}
		}
		first := p.StartedAt == nil
		if err := read(tx.QueryRow(ctx, `UPDATE platform.executions SET status='running',fence=fence+1,
    started_at=coalesce(started_at,$2),run_deadline=coalesce(run_deadline,$2+interval '15 minutes'),
    retry_at=NULL,lease_until=$2+interval '15 seconds'
    WHERE id=$1 AND status IN ('queued','running') RETURNING `+columns, id, now), &p.Execution); err != nil {
			return err
		}
		claimed = true
		if first {
			return event(ctx, tx, p.Execution, "execution.started", id, "", now.Sub(p.QueuedAt))
		}
		return nil
	})
	if err != nil || !claimed {
		return err
	}

	// The checked-out connection holds the exclusion lock, while short state
	// transactions use it only from the worker goroutine. Polling uses the pool.
	run, cancel := context.WithDeadline(ctx, p.Deadline())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-run.Done():
				return
			case <-ticker.C:
				ping, stop := context.WithTimeout(run, 2*time.Second)
				tag, err := s.Pool.Exec(ping, `UPDATE platform.executions SET lease_until=clock_timestamp()+interval '15 seconds'
      WHERE id=$1 AND fence=$2 AND status='running' AND run_deadline>clock_timestamp()`, id, p.Fence)
				stop()
				if err != nil || tag.RowsAffected() != 1 {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-done }()
	p.data.Steps = map[string]workflow.Values{}
	for ordinal, step := range p.definition.Steps {
		var checkpoint workflow.Values
		var found bool
		err := conn.QueryRow(run, `SELECT status='succeeded',output_values FROM platform.execution_steps
    WHERE execution_id=$1 AND ordinal=$2`, id, ordinal).Scan(&found, &checkpoint)
		if err != nil {
			return s.endInterrupted(conn, p.Execution, err)
		}
		if found {
			p.data.Steps[step.ID] = checkpoint
			continue
		}
		lookup := func(ctx context.Context, fid string) (string, error) {
			var mime string
			var scope attemptScope
			if current, ok := ctx.Value(attemptKey{}).(attemptScope); ok {
				scope = current
			}
			err := conn.QueryRow(ctx, `SELECT mime FROM platform.files WHERE id=$1 AND owner_id=$2 AND (
              state='ready' AND (expires_at>clock_timestamp() OR EXISTS(SELECT 1 FROM platform.execution_file_refs WHERE execution_id=$3 AND file_id=$1 AND pin_id IS NOT NULL))
              OR state='uploading' AND EXISTS(SELECT 1 FROM platform.execution_staged_files
                WHERE execution_id=$3 AND file_id=$1 AND uploaded AND (checkpoint OR (ordinal=$4 AND attempt=$5 AND fence=$6))))`,
				fid, p.OwnerID, id, scope.ordinal, scope.number, p.Fence).Scan(&mime)
			if errors.Is(err, pgx.ErrNoRows) {
				return "", nil
			}
			return mime, err
		}
		// Resolve can use any completed checkpoint, but cannot invoke future steps.
		inputs, err := s.Registry.Resolve(run, p.definition, ordinal, p.data, lookup)
		if err != nil {
			if run.Err() != nil {
				return s.endInterrupted(conn, p.Execution, run.Err())
			}
			return s.finalize(conn, p.Execution, Failed, "INVALID_INPUT", nil)
		}
		n, err := s.beginAttempt(run, conn, p.Execution, ordinal, inputs)
		if err != nil {
			return s.endInterrupted(conn, p.Execution, err)
		}
		stepCtx := run
		stop := func() {}
		if step.TimeoutSeconds != nil {
			stepCtx, stop = context.WithTimeout(run, time.Duration(*step.TimeoutSeconds)*time.Second)
		}
		scope := attemptScope{s, p.Execution, ordinal, n, new(storageUsage)}
		stepCtx = context.WithValue(stepCtx, attemptKey{}, scope)
		values, callErr := s.Registry.Invoke(stepCtx, step, inputs, lookup)
		_ = scope.persistStorageUsage(stepCtx)
		if values == nil && callErr == nil {
			values = workflow.Values{}
		}
		proof, stopProof := context.WithTimeout(context.Background(), 3*time.Second)
		stopped, proofErr := s.Registry.ConfirmStopped(proof, step, attemptID(p.Execution, ordinal, n))
		stopProof()
		if proofErr != nil || !stopped {
			stop()
			return ErrLease
		}
		refs := map[string]bool{}
		ids := []string{}
		if callErr == nil {
			callErr = s.Registry.ValidateOutput(stepCtx, step, values, func(ctx context.Context, id string) (string, error) {
				mime, err := lookup(ctx, id)
				if err == nil && mime != "" && !refs[id] {
					ids = append(ids, id)
					refs[id] = true
				}
				return mime, err
			})
			if raw, err := json.Marshal(values); err != nil || len(raw) > 256<<10 {
				callErr = ErrInputs
			}
		}
		stepErr := stepCtx.Err()
		stop()
		if callErr == nil && stepErr != nil {
			callErr = stepErr
		}
		processed, err := s.finishAttempt(conn, p.Execution, ordinal, n, step, values, ids, callErr)
		var pgErr *pgconn.PgError
		if errors.Is(err, ErrQuota) {
			processed, err = s.finishAttempt(conn, p.Execution, ordinal, n, step, nil, nil, ErrQuota)
		} else if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			processed, err = s.finishAttempt(conn, p.Execution, ordinal, n, step, nil, nil, ErrInputs)
		}
		if err != nil {
			return err
		}
		if !processed {
			return nil
		}
		p.data.Steps[step.ID] = values
	}
	return s.finalize(conn, p.Execution, Succeeded, "", p.data.Steps)
}

type transactionStarter interface {
	Begin(context.Context) (pgx.Tx, error)
}

func (s *Store) beginAttempt(ctx context.Context, conn transactionStarter, e Execution, ordinal int, in workflow.Values) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		current, err := ownedState(ctx, tx, e)
		if err != nil {
			return err
		}
		if current != Running {
			return ErrLease
		}
		if err := tx.QueryRow(ctx, `INSERT INTO platform.execution_attempts(execution_id,ordinal,number,fence)
    SELECT $1,$2,coalesce(max(number),0)+1,$3 FROM platform.execution_attempts WHERE execution_id=$1 AND ordinal=$2 RETURNING number`, e.ID, ordinal, e.Fence).Scan(&n); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE platform.execution_steps SET status='running',input_values=$3,
    started_at=coalesce(started_at,clock_timestamp()) WHERE execution_id=$1 AND ordinal=$2`, e.ID, ordinal, in); err != nil {
			return err
		}
		return event(ctx, tx, e, "step.started", fmt.Sprintf("%s:%d:%d", e.ID, ordinal, n), "", 0)
	})
	return n, err
}

func ownedState(ctx context.Context, tx pgx.Tx, e Execution) (State, error) {
	var state State
	var now, deadline time.Time
	var lease *time.Time
	err := tx.QueryRow(ctx, `SELECT status,clock_timestamp(),coalesce(run_deadline,start_deadline),lease_until
   FROM platform.executions WHERE id=$1 AND fence=$2 FOR UPDATE`, e.ID, e.Fence).Scan(&state, &now, &deadline, &lease)
	if errors.Is(err, pgx.ErrNoRows) || state.Terminal() {
		return "", ErrLease
	}
	if err != nil {
		return "", err
	}
	if state != CancelRequested && !now.Before(deadline) {
		return TimedOut, nil
	}
	if state == Running && (lease == nil || !now.Before(*lease)) {
		return "", ErrLease
	}
	return state, nil
}

func (s *Store) finishAttempt(conn transactionStarter, e Execution, ordinal, n int, step workflow.Step, out workflow.Values, refs []string, cause error) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	success := false
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		current, err := ownedState(ctx, tx, e)
		if err != nil {
			return err
		}
		if current == CancelRequested {
			return finishTx(ctx, tx, e, Cancelled, "CANCELLED", nil)
		}
		if current == TimedOut {
			return finishTx(ctx, tx, e, TimedOut, "ACTION_TIMEOUT", nil)
		}
		if current != Running {
			return ErrLease
		}
		key := fmt.Sprintf("%s:%d:%d", e.ID, ordinal, n)
		if cause == nil {
			if out == nil {
				out = workflow.Values{}
			}
			if err := checkpointArtifacts(ctx, tx, e, ordinal, n, refs); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE platform.execution_steps SET status='succeeded',output_values=$3,ended_at=clock_timestamp()
     WHERE execution_id=$1 AND ordinal=$2`, e.ID, ordinal, out); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE platform.execution_attempts SET state='succeeded',ended_at=clock_timestamp()
     WHERE execution_id=$1 AND ordinal=$2 AND number=$3 AND fence=$4`, e.ID, ordinal, n, e.Fence); err != nil {
				return err
			}
			if err := emitFileUsage(ctx, tx, e, &ordinal, "succeeded"); err != nil {
				return err
			}
			success = true
			return event(ctx, tx, e, "step.succeeded", key, "", 0)
		}
		if err := discardStages(ctx, tx, e.ID, true); err != nil {
			return err
		}
		maxAttempts, safe, _ := s.Registry.ExecutionPolicy(step)
		state, code := "permanent_failed", "PROCESSING_ERROR"
		if errors.Is(cause, ErrInputs) {
			code = "INVALID_INPUT"
		}
		if errors.Is(cause, ErrQuota) {
			code = "QUOTA_EXCEEDED"
		}

		if errors.Is(cause, ErrUnknownOutcome) {
			state, code = "unknown_outcome", "UNKNOWN_OUTCOME"
		}
		if errors.Is(cause, context.DeadlineExceeded) {
			state, code = "timed_out", "ACTION_TIMEOUT"
		}
		if errors.Is(cause, context.Canceled) {
			state, code = "interrupted", "INTERRUPTED"
			if !safe {
				state, code = "unknown_outcome", "UNKNOWN_OUTCOME"
			}
		}
		if errors.Is(cause, ErrTemporary) {
			state, code = "retryable_failed", "TEMPORARY_FAILURE"
		}
		if _, err := tx.Exec(ctx, `UPDATE platform.execution_attempts SET state=$4,error_code=$5,ended_at=clock_timestamp()
    WHERE execution_id=$1 AND ordinal=$2 AND number=$3`, e.ID, ordinal, n, state, code); err != nil {
			return err
		}
		var now time.Time
		if err := tx.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&now); err != nil {
			return err
		}
		when := now.Add(retryDelay(n))
		if safe && n < maxAttempts && when.Before(e.Deadline()) && (state == "retryable_failed" || state == "interrupted") {
			if _, err := tx.Exec(ctx, `UPDATE platform.execution_steps SET status='retrying' WHERE execution_id=$1 AND ordinal=$2`, e.ID, ordinal); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE platform.executions SET status='retrying',retry_at=$2,lease_until=NULL WHERE id=$1 AND fence=$3`, e.ID, when, e.Fence); err != nil {
				return err
			}
			if err := event(ctx, tx, e, "step.retry_scheduled", key, code, 0); err != nil {
				return err
			}
			return s.enqueue(ctx, tx, e.ID, when)
		}
		if state == "timed_out" {
			if _, err := tx.Exec(ctx, `UPDATE platform.execution_steps SET status='timed_out',error_code=$3,ended_at=clock_timestamp()
			 WHERE execution_id=$1 AND ordinal=$2`, e.ID, ordinal, code); err != nil {
				return err
			}
		}
		if err := event(ctx, tx, e, "step.failed", key, code, 0); err != nil {
			return err
		}
		return finishTx(ctx, tx, e, Failed, code, nil)
	})
	return success, err
}

func retryDelay(n int) time.Duration {
	return time.Duration(5*(1<<min(max(n-1, 0), 5)))*time.Second + time.Duration(rand.IntN(1000))*time.Millisecond
}

func (s *Store) endInterrupted(conn transactionStarter, e Execution, cause error) error {
	if !errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) && !errors.Is(cause, ErrLease) {
		return cause
	}
	return s.finalize(conn, e, Running, "", nil)
}

func (s *Store) finalize(conn transactionStarter, e Execution, target State, code string, outputs map[string]workflow.Values) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		current, err := ownedState(ctx, tx, e)
		if err != nil {
			return err
		}
		if current == CancelRequested {
			target, code, outputs = Cancelled, "CANCELLED", nil
		}
		if current == TimedOut {
			target, code, outputs = TimedOut, "ACTION_TIMEOUT", nil
		}
		if target == Running {
			_, err := tx.Exec(ctx, "UPDATE platform.executions SET lease_until=NULL WHERE id=$1 AND fence=$2", e.ID, e.Fence)
			if err != nil {
				return err
			}
			return s.enqueue(ctx, tx, e.ID, time.Now().Add(time.Second))
		}
		return finishTx(ctx, tx, e, target, code, outputs)
	})
}

func finishTx(ctx context.Context, tx pgx.Tx, e Execution, status State, code string, outputs map[string]workflow.Values) error {
	var now time.Time
	var payload any
	if status == Succeeded {
		payload = outputs
	}
	err := tx.QueryRow(ctx, `UPDATE platform.executions SET status=$3,terminal_at=clock_timestamp(),error_code=nullif($4,''),
  outputs=$5,lease_until=NULL,retry_at=NULL WHERE id=$1 AND fence=$2 AND terminal_at IS NULL
  AND (($3='cancelled' AND status='cancel_requested')
    OR ($3='timed_out' AND status IN ('queued','running','retrying') AND clock_timestamp()>=coalesce(run_deadline,start_deadline))
    OR ($3 IN ('succeeded','failed') AND status='running' AND clock_timestamp()<run_deadline))
  AND ($3<>'succeeded' OR NOT EXISTS(SELECT 1 FROM platform.execution_steps WHERE execution_id=$1 AND status<>'succeeded'))
  RETURNING terminal_at`, e.ID, e.Fence, status, code, payload).Scan(&now)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrLease
	}
	if err != nil {
		return err
	}
	attempt, step := "permanent_failed", "failed"
	if status == TimedOut {
		attempt, step = "timed_out", "timed_out"
	}
	if status == Cancelled {
		attempt, step = "cancelled", "cancelled"
	}
	if _, err := tx.Exec(ctx, `UPDATE platform.execution_attempts SET state=$2,error_code=nullif($3,''),ended_at=$4
    WHERE execution_id=$1 AND state='running'`, e.ID, attempt, code, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform.outbox_events(type,logical_key,payload)
	 SELECT 'step.skipped',$1::uuid::text||':'||ordinal::text,jsonb_build_object(
	   'execution_id',$1::uuid::text,'owner_id',$2::uuid::text,'outcome',$3::text)
	 FROM platform.execution_steps WHERE execution_id=$1 AND status='pending'`, e.ID, e.OwnerID, code); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE platform.execution_steps SET status=CASE WHEN status='pending' THEN 'skipped' ELSE $2 END,
    error_code=nullif($3,''),ended_at=$4 WHERE execution_id=$1 AND status IN ('pending','running','retrying')`, e.ID, step, code, now); err != nil {
		return err
	}
	if status == Succeeded {
		if _, err := tx.Exec(ctx, `UPDATE platform.files SET state='ready',ready_at=$2::timestamptz,expires_at=$2::timestamptz+interval '168 hours'
		  WHERE id IN(SELECT file_id FROM platform.execution_staged_files WHERE execution_id=$1 AND checkpoint AND uploaded)`, e.ID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM platform.execution_staged_files WHERE execution_id=$1 AND checkpoint", e.ID); err != nil {
			return err
		}
	}
	if err := discardStages(ctx, tx, e.ID, false); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, "SELECT pin_id::text FROM platform.execution_file_refs WHERE execution_id=$1 AND pin_id IS NOT NULL", e.ID)
	if err != nil {
		return err
	}
	var pins []string
	for rows.Next() {
		var pin string
		if err := rows.Scan(&pin); err != nil {
			rows.Close()
			return err
		}
		pins = append(pins, pin)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE platform.execution_file_refs SET pin_id=NULL WHERE execution_id=$1 AND pin_id IS NOT NULL", e.ID); err != nil {
		return err
	}
	for _, pin := range pins {
		if _, err := tx.Exec(ctx, "DELETE FROM platform.file_pins WHERE id=$1", pin); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, "DELETE FROM platform.execution_reservations WHERE execution_id=$1", e.ID); err != nil {
		return err
	}
	if err := emitFileUsage(ctx, tx, e, nil, string(status)); err != nil {
		return err
	}
	return event(ctx, tx, e, "execution."+string(status), e.ID, code, now.Sub(e.QueuedAt))
}
