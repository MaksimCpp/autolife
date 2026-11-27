package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"unicode/utf8"
)

func DecodeDefinition(raw json.RawMessage) (Definition, error) {
	var d Definition
	if !validDraft(raw, 2*MaxDefinitionBytes) {
		return d, &Invalid{[]Problem{{Code: "INVALID_DEFINITION", Path: "/definition"}}}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&d) != nil || dec.Decode(new(any)) != io.EOF {
		return d, &Invalid{[]Problem{{Code: "INVALID_DEFINITION", Path: "/definition"}}}
	}
	return d, nil
}

// Drafts can be incomplete and keep unknown fields for a newer editor.
func ValidDraft(raw json.RawMessage) bool {
	return validDraft(raw, MaxDefinitionBytes)
}

func validDraft(raw json.RawMessage, limit int) bool {
	if len(raw) > limit || !utf8.Valid(raw) {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}

func (r *Registry) Validate(ctx context.Context, d Definition, lookup FileLookup) ([]Problem, error) {
	problems := make([]Problem, 0)
	if d.SchemaVersion != SchemaVersion {
		return append(problems, Problem{Code: "UNSUPPORTED_SCHEMA_VERSION", Path: "/schema_version"}), nil
	}
	if len(d.Steps) < 1 || len(d.Steps) > MaxSteps {
		problems = append(problems, Problem{Code: "STEP_LIMIT", Path: "/steps"})
		if len(d.Steps) > MaxSteps {
			return problems, nil
		}
	}
	if !validFields(d.UserInputs, 0) {
		return append(problems, Problem{Code: "INVALID_SCHEMA", Path: "/user_inputs"}), nil
	}
	triggerOutputs, triggerProblems, err := r.triggerPorts(ctx, d.Trigger, lookup)
	if err != nil {
		return nil, err
	}
	problems = append(problems, triggerProblems...)
	previous := make(map[string]Fields, len(d.Steps))
	for index, step := range d.Steps {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := "/steps/" + strconv.Itoa(index)
		if !namePattern.MatchString(step.ID) || slices.Contains([]string{"trigger", "constant", "user_input"}, step.ID) {
			problems = append(problems, Problem{Code: "INVALID_STEP_ID", Path: p + "/id"})
			continue
		}
		if _, seen := previous[step.ID]; seen {
			problems = append(problems, Problem{"INVALID_STEP_ID", p + "/id", step.ID})
			continue
		}
		if step.TimeoutSeconds != nil && (*step.TimeoutSeconds < 1 || *step.TimeoutSeconds > 900) {
			problems = append(problems, Problem{"INVALID_VALUE", p + "/timeout_seconds", step.ID})
		}
		in, out, stepProblems, err := r.stepPorts(ctx, step, lookup, p)
		if err != nil {
			return nil, err
		}
		problems = append(problems, stepProblems...)
		if len(stepProblems) > 0 {
			previous[step.ID] = nil
			continue
		}
		if len(step.InputBindings) > 64 {
			problems = append(problems, Problem{"LIMIT_EXCEEDED", p + "/input_bindings", step.ID})
			previous[step.ID] = out
			continue
		}
		for name, input := range in {
			binding, ok := step.InputBindings[name]
			bp := fieldPath(p+"/input_bindings", name)
			if !ok {
				if !input.Optional {
					problems = append(problems, Problem{"REQUIRED", bp, step.ID})
				}
				continue
			}
			code, err := checkBinding(ctx, lookup, binding, input, triggerOutputs, previous, d.UserInputs)
			if err != nil {
				return nil, err
			}
			if code != "" {
				problems = append(problems, Problem{code, bp, step.ID})
			}
		}
		for name := range step.InputBindings {
			if _, ok := in[name]; !ok {
				problems = append(problems, Problem{"UNKNOWN_PORT", fieldPath(p+"/input_bindings", name), step.ID})
			}
		}
		previous[step.ID] = out
	}
	sortProblems(problems)
	return problems, nil
}

func sortProblems(p []Problem) {
	slices.SortFunc(p, func(a, b Problem) int {
		if a.Path < b.Path {
			return -1
		}
		if a.Path > b.Path {
			return 1
		}
		if a.Code < b.Code {
			return -1
		}
		if a.Code > b.Code {
			return 1
		}
		return 0
	})
}

func (r *Registry) triggerPorts(ctx context.Context, t Trigger, lookup FileLookup) (Fields, []Problem, error) {
	trigger, ok := r.trigger(Key{t.Type, t.Version})
	if !ok {
		return nil, []Problem{{Code: "UNKNOWN_TRIGGER_VERSION", Path: "/trigger"}}, nil
	}
	problems, err := checkValues(ctx, lookup, trigger.Config, t.Config, "/trigger/config", "")
	if err != nil || len(problems) > 0 {
		return nil, problems, err
	}
	if t.Type == "core.manual" && t.Version == 1 && t.Config["file_formats"] != nil {
		var formats []string
		_ = json.Unmarshal(t.Config["file_formats"], &formats)
		for _, f := range formats {
			if !mimePattern.MatchString(f) {
				return nil, []Problem{{Code: "INVALID_VALUE", Path: "/trigger/config/file_formats"}}, nil
			}
		}
		return Fields{"files": {Kind: "array", Items: &Field{Kind: "file", Formats: formats}}}, nil, nil
	}
	return trigger.Outputs, nil, nil
}

func (r *Registry) stepPorts(ctx context.Context, s Step, lookup FileLookup, p string) (Fields, Fields, []Problem, error) {
	a, ok := r.action(Key{s.ActionType, s.ActionVersion})
	if !ok {
		return nil, nil, []Problem{{"UNKNOWN_ACTION_VERSION", p + "/action_version", s.ID}}, nil
	}
	if a.Available != nil && !a.Available() {
		return nil, nil, []Problem{{"ACTION_UNAVAILABLE", p + "/action_type", s.ID}}, nil
	}
	problems, err := checkValues(ctx, lookup, a.Config, s.Config, p+"/config", s.ID)
	if err != nil {
		return nil, nil, nil, err
	}
	if s.RetryPolicy != nil && (s.RetryPolicy.MaxAttempts < 1 || s.RetryPolicy.MaxAttempts > max(a.MaxAttempts, 1)) {
		problems = append(problems, Problem{"INVALID_VALUE", p + "/retry_policy/max_attempts", s.ID})
	}
	if len(problems) == 0 && a.ValidateConfig != nil {
		for _, problem := range a.ValidateConfig(s.Config) {
			problems = append(problems, Problem{problem.Code, p + "/config" + problem.Path, s.ID})
		}
	}
	if len(problems) > 0 {
		return nil, nil, problems, nil
	}
	in, out, err := a.ports(s.Config)
	return in, out, nil, err
}

func checkBinding(ctx context.Context, lookup FileLookup, b Binding, want Field, trigger Fields, previous map[string]Fields, user Fields) (string, error) {
	if b.Source == "constant" {
		if b.Output != "" || len(b.Value) == 0 {
			return "INVALID_BINDING", nil
		}
		return want.check(ctx, lookup, b.Value)
	}
	if b.Output == "" || len(b.Value) != 0 {
		return "INVALID_BINDING", nil
	}
	var fields Fields
	switch b.Source {
	case "trigger":
		fields = trigger
	case "user_input":
		fields = user
	default:
		var seen bool
		fields, seen = previous[b.Source]
		if !seen {
			return "INVALID_STEP_REFERENCE", nil
		}
	}
	port, ok := fields[b.Output]
	if !ok {
		return "UNKNOWN_PORT", nil
	}
	if !want.accepts(port, b.Source == "trigger" || b.Source == "user_input") {
		return "TYPE_MISMATCH", nil
	}
	return "", nil
}

type BindingData struct {
	Trigger    Values
	Steps      map[string]Values
	UserInputs Values
}

// Resolve checks finalized input types and file access before returning values to a handler.
func (r *Registry) Resolve(ctx context.Context, d Definition, index int, data BindingData, lookup FileLookup) (Values, error) {
	problems, err := r.Validate(ctx, d, lookup)
	if err != nil {
		return nil, err
	}
	if len(problems) != 0 {
		return nil, &Invalid{problems}
	}
	if index < 0 || index >= len(d.Steps) {
		return nil, &Invalid{[]Problem{{Code: "INVALID_STEP_REFERENCE", Path: "/steps"}}}
	}
	trigger, _, err := r.triggerPorts(ctx, d.Trigger, lookup)
	if err != nil {
		return nil, err
	}
	problems, err = checkValues(ctx, lookup, trigger, data.Trigger, "/trigger/outputs", "")
	if err != nil {
		return nil, err
	}
	userProblems, err := checkValues(ctx, lookup, d.UserInputs, data.UserInputs, "/user_inputs", "")
	if err != nil {
		return nil, err
	}
	problems = append(problems, userProblems...)
	if len(problems) != 0 {
		return nil, &Invalid{problems}
	}
	s := d.Steps[index]
	p := "/steps/" + strconv.Itoa(index)
	expected, _, problems, err := r.stepPorts(ctx, s, lookup, p)
	if err != nil {
		return nil, err
	}
	if len(problems) != 0 {
		return nil, &Invalid{problems}
	}
	resolved := make(Values, len(s.InputBindings))
	for name, b := range s.InputBindings {
		var v json.RawMessage
		switch b.Source {
		case "constant":
			v = b.Value
		case "trigger":
			v = data.Trigger[b.Output]
		case "user_input":
			v = data.UserInputs[b.Output]
		default:
			outputs, available := data.Steps[b.Source]
			if !available {
				return nil, &Invalid{[]Problem{{"SOURCE_UNAVAILABLE", fieldPath(p+"/input_bindings", name), s.ID}}}
			}
			v = outputs[b.Output]
		}
		if v != nil {
			resolved[name] = slices.Clone(v)
		}
	}
	problems, err = checkValues(ctx, lookup, expected, resolved, p+"/inputs", s.ID)
	if err != nil {
		return nil, err
	}
	if len(problems) != 0 {
		return nil, &Invalid{problems}
	}
	return resolved, nil
}

func (r *Registry) ValidateOutput(ctx context.Context, s Step, values Values, lookup FileLookup) error {
	_, out, problems, err := r.stepPorts(ctx, s, lookup, "")
	if err != nil {
		return err
	}
	if len(problems) == 0 {
		problems, err = checkValues(ctx, lookup, out, values, "/outputs", s.ID)
	}
	if err != nil {
		return err
	}
	if len(problems) != 0 {
		return &Invalid{problems}
	}
	return nil
}

// Invoke dispatches to an exact registered handler and validates both sides of
// the call. Scheduling and durable execution state belong to TASK-005.
func (r *Registry) Invoke(ctx context.Context, step Step, inputs Values, lookup FileLookup) (Values, error) {
	in, out, problems, err := r.stepPorts(ctx, step, lookup, "")
	if err != nil {
		return nil, err
	}
	if len(problems) == 0 {
		problems, err = checkValues(ctx, lookup, in, inputs, "/inputs", step.ID)
	}
	if err != nil {
		return nil, err
	}
	if len(problems) != 0 {
		return nil, &Invalid{problems}
	}
	action, _ := r.action(Key{step.ActionType, step.ActionVersion})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if action.Available != nil && !action.Available() {
		return nil, &Invalid{[]Problem{{"ACTION_UNAVAILABLE", "/action_type", step.ID}}}
	}
	result, err := action.Handler(ctx, step.Config, inputs)
	if err != nil {
		return nil, err
	}
	problems, err = checkValues(ctx, lookup, out, result, "/outputs", step.ID)
	if err != nil {
		return nil, err
	}
	if len(problems) != 0 {
		return nil, &Invalid{problems}
	}
	return result, nil
}

// ValidateInputs checks the manual trigger and user parameters before a run is enqueued.
func (r *Registry) ValidateInputs(ctx context.Context, d Definition, trigger, user Values, lookup FileLookup) ([]Problem, error) {
	out, problems, err := r.triggerPorts(ctx, d.Trigger, lookup)
	if err != nil || len(problems) > 0 {
		return problems, err
	}
	problems, err = checkValues(ctx, lookup, out, trigger, "/trigger/outputs", "")
	if err != nil {
		return nil, err
	}
	other, err := checkValues(ctx, lookup, d.UserInputs, user, "/user_inputs", "")
	problems = append(problems, other...)
	sortProblems(problems)
	return problems, err
}
