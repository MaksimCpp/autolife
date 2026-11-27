package execution

import (
	"errors"
	"time"

	"autolife/backend/internal/workflow"
)

type State string

const (
	Queued          State = "queued"
	Running         State = "running"
	Retrying        State = "retrying"
	CancelRequested State = "cancel_requested"
	Succeeded       State = "succeeded"
	Failed          State = "failed"
	Cancelled       State = "cancelled"
	TimedOut        State = "timed_out"
)

func (s State) Terminal() bool {
	return s == Succeeded || s == Failed || s == Cancelled || s == TimedOut
}
func (s State) CanBecome(next State) bool {
	switch s {
	case Queued:
		return next == Running || next == CancelRequested || next == TimedOut
	case Running:
		return next == Retrying || next == CancelRequested || next == Succeeded || next == Failed || next == TimedOut
	case Retrying:
		return next == Queued || next == CancelRequested || next == TimedOut
	case CancelRequested:
		return next == Cancelled
	default:
		return false
	}
}

var (
	ErrQuota          = errors.New("execution quota exceeded")
	ErrIdempotency    = errors.New("idempotency key already used")
	ErrState          = errors.New("execution cannot be retried in its current state")
	ErrLease          = errors.New("execution lease no longer owned")
	ErrInputs         = errors.New("execution input unavailable")
	ErrTemporary      = errors.New("temporary action failure")
	ErrUnknownOutcome = errors.New("unknown action outcome")
)

type Execution struct {
	ID            string                     `json:"id"`
	OwnerID       string                     `json:"-"`
	WorkflowID    string                     `json:"workflow_id"`
	VersionID     *string                    `json:"version_id"`
	SnapshotID    *string                    `json:"snapshot_id"`
	Status        State                      `json:"status"`
	QueuedAt      time.Time                  `json:"queued_at"`
	StartDeadline time.Time                  `json:"start_deadline"`
	StartedAt     *time.Time                 `json:"started_at"`
	RunDeadline   *time.Time                 `json:"run_deadline"`
	TerminalAt    *time.Time                 `json:"terminal_at"`
	ErrorCode     *string                    `json:"error_code"`
	Outputs       map[string]workflow.Values `json:"outputs,omitempty"`
	Fence         int64                      `json:"-"`
}

func (e Execution) Deadline() time.Time {
	if e.RunDeadline != nil {
		return *e.RunDeadline
	}
	return e.StartDeadline
}

type StepExecution struct {
	Ordinal   int       `json:"ordinal"`
	Key       string    `json:"step_id"`
	Action    string    `json:"action_type"`
	Version   int       `json:"action_version"`
	Status    string    `json:"status"`
	ErrorCode *string   `json:"error_code"`
	Attempts  []Attempt `json:"attempts"`
}
type Attempt struct {
	FailedIndex *int       `json:"failed_index,omitempty"`
	Number      int        `json:"number"`
	State       string     `json:"state"`
	StartedAt   time.Time  `json:"started_at"`
	EndedAt     *time.Time `json:"ended_at"`
	ErrorCode   *string    `json:"error_code"`
}
type Details struct {
	Execution
	Steps []StepExecution `json:"steps"`
	Files []FileStatus    `json:"files"`
}
type FileStatus struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Expired bool   `json:"expired"`
}
