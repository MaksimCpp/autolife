package execution

import (
	"context"
	"encoding/json"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
)

var fileUsageFormat = regexp.MustCompile(`^[a-z0-9][a-z0-9_+-]{0,23}$`)

type FileUsage struct {
	InputFormat  string `json:"input_format,omitempty"`
	OutputFormat string `json:"output_format,omitempty"`
	InputCount   int    `json:"input_count"`
	OutputCount  int    `json:"output_count"`
	InputBytes   int64  `json:"input_bytes"`
	OutputBytes  int64  `json:"output_bytes"`
	FailedIndex  int    `json:"failed_index,omitempty"`
}

// ReportFileUsage accepts counters only from an active action attempt. Raw
// output, paths, names and user-provided error messages never enter the event.
func ReportFileUsage(ctx context.Context, u FileUsage) error {
	a, ok := ctx.Value(attemptKey{}).(attemptScope)
	if !ok {
		return ErrLease
	}
	validFormat := func(f string) bool { return f == "" || fileUsageFormat.MatchString(f) }
	if !validFormat(u.InputFormat) || !validFormat(u.OutputFormat) || u.InputCount < 0 || u.InputCount > 50 ||
		u.OutputCount < 0 || u.OutputCount > 500 || u.InputBytes < 0 || u.InputBytes > 50*100_000_000 ||
		u.OutputBytes < 0 || u.OutputBytes > MaxBytes || u.FailedIndex < 0 || u.FailedIndex > 50 {
		return ErrInputs
	}
	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	report, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	tag, err := a.store.Pool.Exec(report, `UPDATE platform.execution_attempts x SET file_usage=$5
 FROM platform.executions e WHERE e.id=x.execution_id AND x.execution_id=$1 AND x.ordinal=$2 AND x.number=$3
  AND x.fence=$4 AND e.fence=$4 AND x.state='running' AND e.terminal_at IS NULL`,
		a.execution.ID, a.ordinal, a.number, a.execution.Fence, raw)
	if err == nil && tag.RowsAffected() != 1 {
		return ErrLease
	}
	return err
}

func emitFileUsage(ctx context.Context, tx pgx.Tx, e Execution, ordinal *int, outcome string) error {
	_, err := tx.Exec(ctx, `INSERT INTO platform.outbox_events(type,logical_key,payload)
 SELECT 'file.action_completed',$1::uuid::text||':'||a.ordinal::text,
  a.file_usage || jsonb_build_object('execution_id',$1::uuid::text,'owner_id',$2::uuid::text,
   'step_id',s.step_key,'action_type',s.action_type,'action_version',s.action_version,
   'duration_ms',greatest(0,(extract(epoch FROM (coalesce(a.ended_at,clock_timestamp())-s.started_at))*1000)::bigint),
   'outcome',CASE WHEN s.status='succeeded' THEN 'succeeded' ELSE $4::text END)
 FROM platform.execution_attempts a JOIN platform.execution_steps s USING(execution_id,ordinal)
 WHERE execution_id=$1 AND ($3::int IS NULL OR a.ordinal=$3)
  AND a.number=(SELECT max(b.number) FROM platform.execution_attempts b WHERE b.execution_id=a.execution_id AND b.ordinal=a.ordinal)
  AND a.file_usage IS NOT NULL
 ON CONFLICT DO NOTHING`, e.ID, e.OwnerID, ordinal, outcome)
	return err
}
