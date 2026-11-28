package execution

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"autolife/backend/internal/workflow"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
)

const MaxBytes int64 = 250_000_000
const OwnerLimit = 2
const GlobalLimit = 256

type Limits struct {
	Owner        int64
	Global       int64
	StorageBytes int64
}

func (s *Store) limits() Limits {
	l := s.Limits
	if l.Owner == 0 {
		l.Owner = OwnerLimit
	}
	if l.Global == 0 {
		l.Global = GlobalLimit
	}
	if l.StorageBytes == 0 {
		l.StorageBytes = 1_000_000_000
	}
	return l
}

type Job struct {
	ID string `json:"execution_id"`
}

func (Job) Kind() string { return "workflow_execution" }

type Store struct {
	Pool     *pgxpool.Pool
	Queue    *river.Client[pgx.Tx]
	Registry *workflow.Registry
	Limits   Limits
}

type Start struct {
	VersionID        string          `json:"version_id,omitempty"`
	ExpectedRevision int64           `json:"expected_revision,omitempty"`
	Files            *[]string       `json:"files,omitempty"`
	Inputs           workflow.Values `json:"user_inputs,omitempty"`
}

type QuickStart struct {
	ActionType    string          `json:"action_type"`
	ActionVersion int             `json:"action_version"`
	Config        workflow.Values `json:"config,omitempty"`
	Files         []string        `json:"files"`
}

func (s *Store) StartQuick(ctx context.Context, owner, key string, input QuickStart) (Execution, error) {
	if len(input.Files) < 1 || len(input.Files) > 50 || input.ActionType == "" || input.ActionVersion < 1 {
		return Execution{}, ErrInputs
	}
	return s.admit(ctx, admission{owner: owner, key: key, mode: "quick", quick: &input, request: Start{Files: &input.Files}})
}

type admission struct {
	workflowID, mode, retryID, owner, key string
	request                               Start
	quick                                 *QuickStart
}

func (s *Store) Start(ctx context.Context, owner, wid, key string, input Start, test bool) (Execution, error) {
	mode := "start"
	if test {
		mode = "test"
	}
	return s.admit(ctx, admission{workflowID: wid, mode: mode, owner: owner, key: key, request: input})
}
func (s *Store) Retry(ctx context.Context, owner, id, key string, input Start) (Execution, error) {
	return s.admit(ctx, admission{retryID: id, mode: "retry", owner: owner, key: key, request: input})
}

const summaryColumns = "id::text,owner_id::text,workflow_id::text,version_id::text,snapshot_id::text,quick_snapshot_id::text,status,queued_at,start_deadline,started_at,run_deadline,terminal_at,error_code"
const columns = summaryColumns + ",outputs,fence"

func read(row pgx.Row, e *Execution) error {
	err := row.Scan(&e.ID, &e.OwnerID, &e.WorkflowID, &e.VersionID, &e.SnapshotID, &e.QuickSnapshotID, &e.Status, &e.QueuedAt, &e.StartDeadline, &e.StartedAt, &e.RunDeadline, &e.TerminalAt, &e.ErrorCode, &e.Outputs, &e.Fence)
	if errors.Is(err, pgx.ErrNoRows) {
		return workflow.ErrNotFound
	}
	return err
}
func (s *Store) ownerTx(ctx context.Context, owner string, run func(pgx.Tx) error) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if !workflow.ValidID(owner) {
		return workflow.ErrNotFound
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('autolife.execution_owner',$1,true),
   set_config('autolife.workflow_owner',$1,true),set_config('autolife.file_owner',$1,true)`, owner); err != nil {
			return err
		}
		return run(tx)
	})
}
func event(ctx context.Context, tx pgx.Tx, e Execution, kind, key, code string, duration time.Duration) error {
	payload := map[string]any{"execution_id": e.ID, "owner_id": e.OwnerID}
	if code != "" {
		payload["outcome"] = code
	}
	if duration > 0 {
		payload["duration_ms"] = duration.Milliseconds()
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO platform.outbox_events(type,logical_key,payload) VALUES($1,$2,$3)`, kind, key, raw)
	return err
}

func hashRequest(a admission) ([32]byte, error) {
	var req any = a.request
	if a.quick != nil {
		req = a.quick
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return [32]byte{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return [32]byte{}, err
	}
	canonical, err := json.Marshal([]any{a.workflowID, a.retryID, value})
	return sha256.Sum256(canonical), err
}

func (s *Store) admit(ctx context.Context, a admission) (Execution, error) {
	var result Execution
	if !workflow.ValidID(a.owner) || (a.mode == "retry" && !workflow.ValidID(a.retryID)) || (a.mode != "retry" && a.mode != "quick" && !workflow.ValidID(a.workflowID)) {
		return result, workflow.ErrNotFound
	}
	hash, err := hashRequest(a)
	if err != nil {
		return result, err
	}
	err = s.ownerTx(ctx, a.owner, func(tx pgx.Tx) error {
		var owner string
		err := tx.QueryRow(ctx, "SELECT id::text FROM platform.users WHERE id=$1 AND status='active' FOR UPDATE", a.owner).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return workflow.ErrNotFound
		}
		if err != nil {
			return err
		}
		var oldID string
		var oldHash []byte
		err = tx.QueryRow(ctx, `SELECT execution_id::text,request_hash FROM platform.execution_idempotency
   WHERE owner_id=$1 AND operation=$2 AND key=$3 AND expires_at>clock_timestamp()`, a.owner, a.mode, a.key).Scan(&oldID, &oldHash)
		if err == nil {
			if !bytes.Equal(oldHash, hash[:]) {
				return ErrIdempotency
			}
			return read(tx.QueryRow(ctx, "SELECT "+columns+" FROM platform.executions WHERE id=$1 AND owner_id=$2", oldID, a.owner), &result)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var wid, version, snapshot, quickSnapshot *string
		if a.workflowID != "" {
			wid = &a.workflowID
		}
		trigger := workflow.Values{"files": json.RawMessage("[]")}
		user := workflow.Values{}
		if a.mode == "retry" {
			var prior State
			err = tx.QueryRow(ctx, `SELECT workflow_id::text,version_id::text,snapshot_id::text,quick_snapshot_id::text,trigger_values,user_inputs,status
    FROM platform.executions WHERE id=$1 AND owner_id=$2`, a.retryID, a.owner).Scan(&wid, &version, &snapshot, &quickSnapshot, &trigger, &user, &prior)
			if errors.Is(err, pgx.ErrNoRows) {
				return workflow.ErrNotFound
			}
			if err != nil {
				return err
			}
			if !prior.Terminal() {
				return ErrState
			}
		}
		if a.request.Files != nil {
			if len(*a.request.Files) > 50 {
				return ErrInputs
			}
			trigger["files"], err = json.Marshal(*a.request.Files)
			if err != nil {
				return err
			}
		}
		if a.request.Inputs != nil {
			user = a.request.Inputs
		}
		var status string
		var active *string
		if wid != nil {
			err = tx.QueryRow(ctx, `SELECT status,active_version_id::text FROM platform.workflows
   WHERE owner_id=$1 AND id=$2 FOR UPDATE`, a.owner, wid).Scan(&status, &active)
			if errors.Is(err, pgx.ErrNoRows) {
				return workflow.ErrNotFound
			}
			if err != nil {
				return err
			}
			if status == "archived" {
				return workflow.ErrArchived
			}
		}
		var raw json.RawMessage
		switch {
		case a.mode == "quick":
			def, buildErr := s.Registry.QuickDefinition(workflow.Key{Type: a.quick.ActionType, Version: a.quick.ActionVersion}, a.quick.Config)
			if buildErr != nil {
				return buildErr
			}
			raw, err = json.Marshal(def)
		case quickSnapshot != nil:
			err = tx.QueryRow(ctx, `SELECT definition FROM platform.quick_execution_snapshots WHERE owner_id=$1 AND id=$2`, a.owner, *quickSnapshot).Scan(&raw)
		case a.mode == "test":
			var revision int64
			if err := tx.QueryRow(ctx, `SELECT definition,revision FROM platform.workflow_drafts WHERE owner_id=$1 AND workflow_id=$2`, a.owner, wid).Scan(&raw, &revision); err != nil {
				return err
			}
			if revision != a.request.ExpectedRevision {
				return workflow.ErrConflict
			}
		case snapshot != nil:
			err = tx.QueryRow(ctx, `SELECT definition FROM platform.draft_execution_snapshots WHERE owner_id=$1 AND workflow_id=$2 AND id=$3`, a.owner, wid, *snapshot).Scan(&raw)
		default:
			if version == nil {
				if a.request.VersionID != "" {
					version = &a.request.VersionID
				} else {
					version = active
				}
			}
			if version == nil || !workflow.ValidID(*version) {
				return workflow.ErrNotFound
			}
			err = tx.QueryRow(ctx, `SELECT definition FROM platform.workflow_versions WHERE owner_id=$1 AND workflow_id=$2 AND id=$3`, a.owner, wid, *version).Scan(&raw)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return workflow.ErrNotFound
		}
		if err != nil {
			return err
		}
		def, err := workflow.DecodeDefinition(raw)
		if err != nil {
			return err
		}
		if a.mode == "quick" || quickSnapshot != nil {
			var inputs []json.RawMessage
			if json.Unmarshal(trigger["files"], &inputs) != nil || len(inputs) == 0 {
				return ErrInputs
			}
			user = workflow.Values{}
			if _, scalar := def.UserInputs["file"]; scalar {
				if len(inputs) != 1 {
					return ErrInputs
				}
				user["file"] = inputs[0]
			} else {
				user["files"] = trigger["files"]
			}
		}
		refs := map[string]string{}
		var bytesUsed int64
		lookup := func(ctx context.Context, id string) (string, error) {
			if value, ok := refs[id]; ok {
				return value, nil
			}
			if len(refs) >= 50 {
				return "", ErrInputs
			}
			var mime string
			var size int64
			err := tx.QueryRow(ctx, `SELECT mime,bytes FROM platform.files
    WHERE owner_id=$1 AND id=$2 AND state='ready' AND expires_at>clock_timestamp()`, a.owner, id).Scan(&mime, &size)
			if errors.Is(err, pgx.ErrNoRows) {
				return "", ErrInputs
			}
			if err != nil {
				return "", err
			}
			bytesUsed += size
			if bytesUsed > MaxBytes {
				return "", ErrInputs
			}
			refs[id] = mime
			return mime, nil
		}
		problems, err := s.Registry.Validate(ctx, def, lookup)
		if err == nil && len(problems) == 0 {
			problems, err = s.Registry.ValidateInputs(ctx, def, trigger, user, lookup)
		}
		if err != nil {
			return err
		}
		if len(problems) != 0 {
			return &workflow.Invalid{Fields: problems}
		}

		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1910567,5)"); err != nil {
			return err
		}
		var global, owned, ownerLimit int64
		limits := s.limits()
		if err := tx.QueryRow(ctx, `SELECT platform.active_execution_count(),
   (SELECT count(*) FROM platform.execution_reservations WHERE owner_id=$1),
   coalesce((SELECT limit_value FROM platform.quota_limits WHERE owner_id=$1 AND resource_type='concurrent_executions'),$2)`,
			a.owner, limits.Owner).Scan(&global, &owned, &ownerLimit); err != nil {
			return err
		}
		if global >= limits.Global || owned >= ownerLimit {
			return ErrQuota
		}
		if a.mode == "test" {
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO platform.draft_execution_snapshots(owner_id,workflow_id,revision,schema_version,definition)
    VALUES($1,$2,$3,$4,$5) RETURNING id::text`, a.owner, wid, a.request.ExpectedRevision, def.SchemaVersion, raw).Scan(&id); err != nil {
				return err
			}
			snapshot = &id
		}
		if a.mode == "quick" {
			var id string
			if err := tx.QueryRow(ctx, `INSERT INTO platform.quick_execution_snapshots(owner_id,schema_version,definition)
 VALUES($1,$2,$3) RETURNING id::text`, a.owner, def.SchemaVersion, raw).Scan(&id); err != nil {
				return err
			}
			quickSnapshot = &id
		}
		if err := read(tx.QueryRow(ctx, `INSERT INTO platform.executions(owner_id,workflow_id,version_id,snapshot_id,quick_snapshot_id,trigger_type,trigger_values,user_inputs,start_deadline)
   VALUES($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp()+interval '30 minutes') RETURNING `+columns,
			a.owner, wid, version, snapshot, quickSnapshot, def.Trigger.Type, trigger, user), &result); err != nil {
			return err
		}
		ids := make([]string, 0, len(refs))
		for id := range refs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		position := map[string]int{}
		var ordered []string
		_ = json.Unmarshal(trigger["files"], &ordered)
		for index, id := range ordered {
			if _, exists := position[id]; !exists {
				position[id] = index
			}
		}
		for index, id := range ids {
			order, exists := position[id]
			if !exists {
				order = len(ordered) + index
			}
			var pin string
			if err := tx.QueryRow(ctx, `INSERT INTO platform.file_pins(owner_id,file_id,expires_at) VALUES($1,$2,'infinity') RETURNING id::text`, a.owner, id).Scan(&pin); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO platform.execution_file_refs(owner_id,execution_id,file_id,live_file_id,pin_id,role,position)
    VALUES($1,$2,$3,$3,$4,'input',$5)`, a.owner, result.ID, id, pin, order); err != nil {
				return err
			}
		}
		for i, step := range def.Steps {
			if _, err := tx.Exec(ctx, `INSERT INTO platform.execution_steps(execution_id,ordinal,step_key,action_type,action_version)
    VALUES($1,$2,$3,$4,$5)`, result.ID, i, step.ID, step.ActionType, step.ActionVersion); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO platform.execution_reservations(owner_id,execution_id,input_bytes) VALUES($1,$2,$3)`, a.owner, result.ID, bytesUsed); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM platform.execution_idempotency WHERE owner_id=$1 AND operation=$2 AND key=$3`, a.owner, a.mode, a.key); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO platform.execution_idempotency(owner_id,operation,key,request_hash,execution_id,expires_at)
   VALUES($1,$2,$3,$4,$5,clock_timestamp()+interval '24 hours')`, a.owner, a.mode, a.key, hash[:], result.ID); err != nil {
			return err
		}
		if err := event(ctx, tx, result, "execution.created", result.ID, "", 0); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, result.ID, time.Time{})
	})
	return result, err
}

func (s *Store) enqueue(ctx context.Context, tx pgx.Tx, id string, when time.Time) error {
	if s.Queue == nil {
		return fmt.Errorf("execution queue unavailable")
	}
	_, err := s.Queue.InsertTx(ctx, tx, Job{ID: id}, &river.InsertOpts{ScheduledAt: when, MaxAttempts: 8})
	return err
}
