package workflow

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
)

var namePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)
var typePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$`)
var mimePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.+_-]*/[a-z0-9][a-z0-9.+_-]*$`)

type Key struct {
	Type    string `json:"type"`
	Version int    `json:"version"`
}

type FormatPair struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

type Descriptor struct {
	Key
	Config          Fields       `json:"config"`
	Inputs          Fields       `json:"inputs"`
	Outputs         Fields       `json:"outputs"`
	Capability      string       `json:"capability,omitempty"`
	ResourceProfile string       `json:"resource_profile,omitempty"`
	FormatPairs     []FormatPair `json:"format_pairs,omitempty"`
	MaxAttempts     int          `json:"max_attempts,omitempty"`
}

type Handler func(context.Context, Values, Values) (Values, error)
type ActionDefinition struct {
	Descriptor
	Handler        Handler
	SafeReplay     bool
	ConfirmStopped func(context.Context, string) (bool, error)
	Available      func() bool
	ValidateConfig func(Values) []Problem
	Ports          func(Values) (Fields, Fields)
}

type Registry struct {
	mu       sync.RWMutex
	actions  map[Key]ActionDefinition
	triggers map[Key]Descriptor
}

func NewRegistry() *Registry {
	r := &Registry{actions: make(map[Key]ActionDefinition), triggers: make(map[Key]Descriptor)}
	_ = r.RegisterTrigger(Descriptor{
		Key:    Key{"core.manual", 1},
		Config: Fields{"file_formats": {Kind: "array", Optional: true, Items: &Field{Kind: "string"}}},
		Inputs: Fields{}, Outputs: Fields{"files": {Kind: "array", Items: &Field{Kind: "file"}}},
	})
	return r
}

func (r *Registry) RegisterAction(a ActionDefinition) error {
	if a.Handler == nil || a.MaxAttempts < 0 || a.MaxAttempts > 3 || (a.ResourceProfile != "" && a.ConfirmStopped == nil) {
		return errors.New("invalid action handler or policy")
	}
	desc, err := copyDescriptor(a.Descriptor)
	if err != nil {
		return err
	}
	a.Descriptor = desc
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, found := r.actions[a.Key]; found {
		return errors.New("action version already registered")
	}
	r.actions[a.Key] = a
	return nil
}

func (r *Registry) RegisterTrigger(d Descriptor) error {
	desc, err := copyDescriptor(d)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, found := r.triggers[d.Key]; found {
		return errors.New("trigger version already registered")
	}
	r.triggers[d.Key] = desc
	return nil
}

func copyDescriptor(d Descriptor) (Descriptor, error) {
	if len(d.Type) > 128 || !typePattern.MatchString(d.Type) || d.Version < 1 {
		return Descriptor{}, errors.New("invalid registry key")
	}
	for _, fields := range []Fields{d.Config, d.Inputs, d.Outputs} {
		if !validFields(fields, 0) {
			return Descriptor{}, errors.New("invalid registry schema")
		}
	}
	for _, pair := range d.FormatPairs {
		if !mimePattern.MatchString(pair.Input) || !mimePattern.MatchString(pair.Output) {
			return Descriptor{}, errors.New("invalid format pair")
		}
	}
	data, err := json.Marshal(d)
	if err != nil {
		return Descriptor{}, err
	}
	var result Descriptor
	err = json.Unmarshal(data, &result)
	if result.Config == nil {
		result.Config = Fields{}
	}
	if result.Inputs == nil {
		result.Inputs = Fields{}
	}
	if result.Outputs == nil {
		result.Outputs = Fields{}
	}
	return result, err
}

func validFields(fields Fields, depth int) bool {
	if len(fields) > 64 {
		return false
	}
	for name, f := range fields {
		if !namePattern.MatchString(name) || !validField(f, depth) {
			return false
		}
	}
	return true
}

func validField(f Field, depth int) bool {
	if depth >= 12 || len(f.Formats) > 50 || len(f.Enum) > 64 || f.MaxItems < 0 || f.MaxItems > 1000 {
		return false
	}
	if f.Kind != "array" && (f.Items != nil || f.MaxItems != 0) || f.Kind != "object" && len(f.Properties) != 0 ||
		f.Kind != "file" && len(f.Formats) != 0 || f.Kind != "string" && len(f.Enum) != 0 {
		return false
	}
	switch f.Kind {
	case "array":
		return f.Items != nil && !f.Items.Optional && validField(*f.Items, depth+1)
	case "object":
		return validFields(f.Properties, depth+1)
	case "file":
		for _, m := range f.Formats {
			if !mimePattern.MatchString(m) {
				return false
			}
		}
		return true
	case "string", "integer", "number", "boolean", "date", "datetime":
		return true
	}
	return false
}

func (r *Registry) action(key Key) (ActionDefinition, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.actions[key]
	return a, ok
}

func (r *Registry) trigger(key Key) (Descriptor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.triggers[key]
	return t, ok
}

func (r *Registry) Actions() []Descriptor {
	r.mu.RLock()
	actions := make([]ActionDefinition, 0, len(r.actions))
	for _, a := range r.actions {
		actions = append(actions, a)
	}
	r.mu.RUnlock()
	out := make([]Descriptor, 0, len(actions))
	for _, a := range actions {
		if a.Available != nil && !a.Available() {
			continue
		}
		d, _ := copyDescriptor(a.Descriptor)
		out = append(out, d)
	}
	sortDescriptors(out)
	return out
}

func (r *Registry) Triggers() []Descriptor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Descriptor, 0, len(r.triggers))
	for _, t := range r.triggers {
		d, _ := copyDescriptor(t)
		out = append(out, d)
	}
	sortDescriptors(out)
	return out
}

func sortDescriptors(items []Descriptor) {
	slices.SortFunc(items, func(a, b Descriptor) int {
		if diff := cmp.Compare(a.Type, b.Type); diff != 0 {
			return diff
		}
		return cmp.Compare(a.Version, b.Version)
	})
}

func (a ActionDefinition) ports(cfg Values) (Fields, Fields, error) {
	if a.Ports == nil {
		return a.Inputs, a.Outputs, nil
	}
	in, out := a.Ports(cfg)
	if !validFields(in, 0) || !validFields(out, 0) {
		return nil, nil, fmt.Errorf("invalid dynamic port schema for %s", a.Type)
	}
	return in, out, nil
}

// ExecutionPolicy exposes server-controlled behavior for durable execution.
func (r *Registry) ExecutionPolicy(step Step) (maxAttempts int, safeReplay bool, found bool) {
	a, ok := r.action(Key{step.ActionType, step.ActionVersion})
	if !ok {
		return 1, false, false
	}
	n := max(a.MaxAttempts, 1)
	if step.RetryPolicy != nil && step.RetryPolicy.MaxAttempts > 0 {
		n = min(n, step.RetryPolicy.MaxAttempts)
	}
	return n, a.SafeReplay, true
}

// ConfirmStopped is required for isolated profiles. The supplied ID identifies
// the same attempt passed to the handler; false leaves its resources reserved.
func (r *Registry) ConfirmStopped(ctx context.Context, step Step, id string) (bool, error) {
	a, ok := r.action(Key{step.ActionType, step.ActionVersion})
	if !ok {
		return false, nil
	}
	if a.ConfirmStopped == nil {
		return a.ResourceProfile == "", nil
	}
	return a.ConfirmStopped(ctx, id)
}
