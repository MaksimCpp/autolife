package workflow

import "strings"

// QuickDefinition builds the server-owned one-step definition of an available
// FileFlow operation using its effective input port.
func (r *Registry) QuickDefinition(key Key, cfg Values) (Definition, error) {
	bad := func(code string) (Definition, error) {
		return Definition{}, &Invalid{Fields: []Problem{{Code: code, Path: "/action_type"}}}
	}
	a, ok := r.action(key)
	if !ok || !strings.HasPrefix(key.Type, "file.") || a.ResourceProfile == "" {
		return bad("UNKNOWN_ACTION_VERSION")
	}
	if a.Available != nil && !a.Available() {
		return bad("ACTION_UNAVAILABLE")
	}
	inputs, _, err := a.ports(cfg)
	if err != nil {
		return Definition{}, err
	}
	if len(inputs) != 1 {
		return bad("INVALID_INPUT_BINDING")
	}
	step := Step{ID: "convert", ActionType: key.Type, ActionVersion: key.Version, Config: cfg, InputBindings: map[string]Binding{}}
	def := Definition{SchemaVersion: SchemaVersion, Trigger: Trigger{Type: "core.manual", Version: 1}}
	for name, field := range inputs {
		switch {
		case field.Kind == "array" && field.Items != nil && field.Items.Kind == "file":
			def.UserInputs = Fields{"files": field}
			step.InputBindings[name] = Binding{Source: "user_input", Output: "files"}
		case field.Kind == "file":
			def.UserInputs = Fields{"file": field}
			step.InputBindings[name] = Binding{Source: "user_input", Output: "file"}
		default:
			return bad("INVALID_INPUT_BINDING")
		}
	}
	def.Steps = []Step{step}
	return def, nil
}
