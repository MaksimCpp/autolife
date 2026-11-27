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
		_, err = tx.Exec(ctx, "DELETE FROM platform.executions WHERE terminal_at<clock_timestamp()-interval '30 days'")
		return err
	})
}

func (s *Store) consume(ctx context.Context) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `WITH batch AS (
    SELECT e.event_id,e.type,e.payload FROM platform.outbox_events e
    WHERE e.type IN ('execution.created','execution.started','execution.succeeded','execution.failed','execution.cancelled','execution.timed_out',
      'step.started','step.succeeded','step.failed','step.retry_scheduled','step.skipped')
    AND NOT EXISTS(SELECT 1 FROM platform.outbox_consumptions c WHERE c.consumer='execution_metrics' AND c.event_id=e.event_id)
    ORDER BY e.occurred_at,e.event_id LIMIT 100
   ), accepted AS (
    INSERT INTO platform.outbox_consumptions(consumer,event_id)
    SELECT 'execution_metrics',event_id FROM batch ON CONFLICT DO NOTHING RETURNING event_id
   )
   INSERT INTO platform.execution_event_totals(type,outcome,count,duration_ms)
   SELECT b.type,coalesce(b.payload->>'outcome',''),count(*),sum(coalesce((b.payload->>'duration_ms')::bigint,0))
   FROM batch b JOIN accepted a USING(event_id) GROUP BY b.type,coalesce(b.payload->>'outcome','')
   ON CONFLICT(type,outcome) DO UPDATE SET
     count=platform.execution_event_totals.count+excluded.count,
     duration_ms=platform.execution_event_totals.duration_ms+excluded.duration_ms`)
		return err
	})
}

// Metrics is mounted on the worker's internal listener only.
func (s *Store) Metrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	rows, err := s.Pool.Query(ctx, `SELECT type,outcome,count,duration_ms FROM platform.execution_event_totals ORDER BY type,outcome`)
	if err != nil {
		w.WriteHeader(503)
		return
	}
	var body strings.Builder
	for rows.Next() {
		var kind, outcome string
		var count, duration int64
		if err := rows.Scan(&kind, &outcome, &count, &duration); err != nil {
			rows.Close()
			w.WriteHeader(503)
			return
		}
		fmt.Fprintf(&body, "autolife_execution_events_total{event=%q,outcome=%q} %d\n", kind, outcome, count)
		fmt.Fprintf(&body, "autolife_execution_event_duration_milliseconds_total{event=%q,outcome=%q} %d\n", kind, outcome, duration)
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
