package execution

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Store) Maintain(ctx context.Context, logger *slog.Logger) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		work, cancel := context.WithTimeout(ctx, 4*time.Second)
		if err := s.reconcile(work); err != nil && ctx.Err() == nil {
			logger.Warn("execution reconciliation failed")
		}
		if err := s.consume(work); err != nil && ctx.Err() == nil {
			logger.Warn("execution outbox delivery failed")
		}
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Store) reconcile(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var leader bool
		if err := tx.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock(1910567,6)").Scan(&leader); err != nil || !leader {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE platform.executions e SET status='cancel_requested'
   FROM platform.privacy_jobs j WHERE j.owner_id=e.owner_id AND j.type='delete' AND j.state='running'
   AND e.status IN ('queued','running','retrying')
   AND clock_timestamp()<coalesce(e.run_deadline,e.start_deadline)`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT e.id::text FROM platform.executions e WHERE terminal_at IS NULL
    AND (lease_until IS NULL OR lease_until<=clock_timestamp())
    AND (retry_at IS NULL OR retry_at<=clock_timestamp() OR status='cancel_requested')
    AND (status<>'queued' OR queued_at<=clock_timestamp()-interval '15 seconds')
    AND NOT EXISTS(SELECT 1 FROM queue.river_job j WHERE j.kind='workflow_execution'
      AND j.args->>'execution_id'=e.id::text AND j.state IN ('available','retryable','scheduled','pending'))
    ORDER BY e.queued_at,e.id LIMIT 32`)
		if err != nil {
			return err
		}
		ids := []string{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if err := s.enqueue(ctx, tx, id, time.Time{}); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "DELETE FROM platform.executions WHERE terminal_at<clock_timestamp()-interval '30 days'"); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `WITH expired AS (
 SELECT q.owner_id,q.id FROM platform.quick_execution_snapshots q
  WHERE q.created_at<clock_timestamp()-interval '31 days'
  AND NOT EXISTS(SELECT 1 FROM platform.executions e WHERE e.owner_id=q.owner_id AND e.quick_snapshot_id=q.id)
  LIMIT 100)
 DELETE FROM platform.quick_execution_snapshots q USING expired
 WHERE q.owner_id=expired.owner_id AND q.id=expired.id`)
		return err
	})
}

func (s *Store) consume(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `WITH batch AS (
    SELECT e.event_id,e.type,e.payload FROM platform.outbox_events e
    WHERE e.type IN ('execution.created','execution.started','execution.succeeded','execution.failed','execution.cancelled','execution.timed_out',
      'step.started','step.succeeded','step.failed','step.retry_scheduled','step.skipped','file.action_completed','file.action_attempted')
    AND NOT EXISTS(SELECT 1 FROM platform.outbox_consumptions c WHERE c.consumer='execution_metrics' AND c.event_id=e.event_id)
    ORDER BY e.occurred_at,e.event_id LIMIT 100
   ), accepted AS (
    INSERT INTO platform.outbox_consumptions(consumer,event_id)
    SELECT 'execution_metrics',event_id FROM batch ON CONFLICT DO NOTHING RETURNING event_id
   )
   INSERT INTO platform.execution_event_totals(type,outcome,count,duration_ms,input_bytes,output_bytes,file_count,sampled_jobs,cpu_ns,peak_memory_bytes,storage_gets,storage_puts,storage_read_bytes,storage_write_bytes)
   SELECT b.type,coalesce(b.payload->>'outcome',''),count(*),sum(coalesce((b.payload->>'duration_ms')::bigint,0)),
    sum(coalesce((b.payload->>'input_bytes')::bigint,0)),sum(coalesce((b.payload->>'output_bytes')::bigint,0)),
    sum(coalesce((b.payload->>'output_count')::bigint,0)),
    sum(coalesce((b.payload->>'sampled_jobs')::bigint,0)),sum(coalesce((b.payload->>'cpu_ns')::bigint,0)),
    max(coalesce((b.payload->>'peak_memory_bytes')::bigint,0)),
    sum(coalesce((b.payload->>'storage_gets')::bigint,0)),sum(coalesce((b.payload->>'storage_puts')::bigint,0)),
    sum(coalesce((b.payload->>'storage_read_bytes')::bigint,0)),sum(coalesce((b.payload->>'storage_write_bytes')::bigint,0))
   FROM batch b JOIN accepted a USING(event_id) GROUP BY b.type,coalesce(b.payload->>'outcome','')
   ON CONFLICT(type,outcome) DO UPDATE SET
     count=platform.execution_event_totals.count+excluded.count,
     duration_ms=platform.execution_event_totals.duration_ms+excluded.duration_ms,
     input_bytes=platform.execution_event_totals.input_bytes+excluded.input_bytes,
     output_bytes=platform.execution_event_totals.output_bytes+excluded.output_bytes,
     file_count=platform.execution_event_totals.file_count+excluded.file_count,
     sampled_jobs=platform.execution_event_totals.sampled_jobs+excluded.sampled_jobs,
     cpu_ns=platform.execution_event_totals.cpu_ns+excluded.cpu_ns,
     peak_memory_bytes=greatest(platform.execution_event_totals.peak_memory_bytes,excluded.peak_memory_bytes),
     storage_gets=platform.execution_event_totals.storage_gets+excluded.storage_gets,
     storage_puts=platform.execution_event_totals.storage_puts+excluded.storage_puts,
     storage_read_bytes=platform.execution_event_totals.storage_read_bytes+excluded.storage_read_bytes,
     storage_write_bytes=platform.execution_event_totals.storage_write_bytes+excluded.storage_write_bytes`)
		return err
	})
}

// Metrics is mounted on the worker's internal listener only.
func (s *Store) Metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	rows, err := s.Pool.Query(ctx, `SELECT type,outcome,count,duration_ms,input_bytes,output_bytes,file_count,sampled_jobs,cpu_ns,peak_memory_bytes,storage_gets,storage_puts,storage_read_bytes,storage_write_bytes FROM platform.execution_event_totals ORDER BY type,outcome`)
	if err != nil {
		w.WriteHeader(503)
		return
	}
	var body strings.Builder
	for rows.Next() {
		var kind, outcome string
		var count, duration, input, output, files, sampled, cpu, memory, gets, puts, read, written int64
		if err := rows.Scan(&kind, &outcome, &count, &duration, &input, &output, &files, &sampled, &cpu, &memory, &gets, &puts, &read, &written); err != nil {
			rows.Close()
			w.WriteHeader(503)
			return
		}
		fmt.Fprintf(&body, "autolife_execution_events_total{event=%q,outcome=%q} %d\n", kind, outcome, count)
		fmt.Fprintf(&body, "autolife_execution_event_duration_milliseconds_total{event=%q,outcome=%q} %d\n", kind, outcome, duration)
		if kind == "file.action_attempted" {
			fmt.Fprintf(&body, "autolife_file_attempt_duration_milliseconds_total{outcome=%q} %d\nautolife_file_attempt_input_bytes_total{outcome=%q} %d\nautolife_file_attempt_output_bytes_total{outcome=%q} %d\n", outcome, duration, outcome, input, outcome, output)
			fmt.Fprintf(&body, "autolife_file_attempt_sampled_jobs_total{outcome=%q} %d\n", outcome, sampled)
			fmt.Fprintf(&body, "autolife_file_attempt_s3_requests_total{outcome=%q,operation=\"get\"} %d\nautolife_file_attempt_s3_requests_total{outcome=%q,operation=\"put\"} %d\nautolife_file_attempt_s3_bytes_total{outcome=%q,operation=\"get\"} %d\nautolife_file_attempt_s3_bytes_total{outcome=%q,operation=\"put\"} %d\n", outcome, gets, outcome, puts, outcome, read, outcome, written)
			if sampled > 0 {
				fmt.Fprintf(&body, "autolife_file_attempt_sampled_cpu_nanoseconds_total{outcome=%q} %d\nautolife_file_attempt_sampled_peak_memory_bytes{outcome=%q} %d\n", outcome, cpu, outcome, memory)
			}
		}
		if kind == "file.action_completed" {
			fmt.Fprintf(&body, "autolife_file_action_input_bytes_total{outcome=%q} %d\nautolife_file_action_output_bytes_total{outcome=%q} %d\nautolife_file_action_outputs_total{outcome=%q} %d\n", outcome, input, outcome, output, outcome, files)
		}
	}
	rows.Close()
	if rows.Err() != nil {
		w.WriteHeader(503)
		return
	}
	var queued, running int64
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE status IN ('queued','retrying')),count(*) FILTER(WHERE status='running')
  FROM platform.executions WHERE terminal_at IS NULL`).Scan(&queued, &running); err != nil {
		w.WriteHeader(503)
		return
	}
	fmt.Fprintf(&body, "autolife_execution_queue_depth %d\nautolife_execution_running %d\n", queued, running)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, body.String())
}
