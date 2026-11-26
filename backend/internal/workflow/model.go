package workflow

import (
	"encoding/json"
	"errors"
	"time"
)

const SchemaVersion = 1
const MaxSteps = 20
const MaxDefinitionBytes = 256 << 10

var (
	ErrNotFound = errors.New("workflow resource not found")
	ErrConflict = errors.New("workflow revision conflict")
	ErrArchived = errors.New("workflow archived")
)

type Workflow struct {
	ID              string    `json:"id"`
	Name            string    `json:"name"`
	Description     string    `json:"description"`
	Status          string    `json:"status"`
	ActiveVersionID *string   `json:"active_version_id"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Draft struct {
	Revision   int64           `json:"revision"`
	Definition json.RawMessage `json:"definition"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

type Version struct {
	ID            string          `json:"id"`
	Number        int64           `json:"version"`
	Revision      int64           `json:"revision"`
	SchemaVersion int             `json:"schema_version"`
	Definition    json.RawMessage `json:"definition,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

type Details struct {
	Workflow
	Draft Draft `json:"draft"`
}

type Definition struct {
	SchemaVersion int     `json:"schema_version"`
	Trigger       Trigger `json:"trigger"`
	Steps         []Step  `json:"steps"`
	UserInputs    Fields  `json:"user_inputs,omitempty"`
}

type Trigger struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
	Config  Values `json:"config"`
}

type Step struct {
	ID             string             `json:"id"`
	ActionType     string             `json:"action_type"`
	ActionVersion  int                `json:"action_version"`
	Config         Values             `json:"config"`
	InputBindings  map[string]Binding `json:"input_bindings"`
	TimeoutSeconds *int               `json:"timeout_seconds,omitempty"`
	RetryPolicy    *RetryPolicy       `json:"retry_policy,omitempty"`
}

type RetryPolicy struct {
	MaxAttempts int `json:"max_attempts"`
}

type Binding struct {
	Source string          `json:"source"`
	Output string          `json:"output,omitempty"`
	Value  json.RawMessage `json:"value,omitempty"`
}

type Problem struct {
	Code   string `json:"code"`
	Path   string `json:"path"`
	StepID string `json:"step_id,omitempty"`
}

type Invalid struct{ Fields []Problem }

func (e *Invalid) Error() string { return "workflow validation failed" }

func EmptyDraft() json.RawMessage {
	return json.RawMessage(`{"schema_version":1,"trigger":{"type":"core.manual","version":1,"config":{}},"steps":[]}`)
}
