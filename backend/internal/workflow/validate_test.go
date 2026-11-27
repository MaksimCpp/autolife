package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

const pngID = "11111111-1111-1111-1111-111111111111"
const jpegID = "22222222-2222-2222-2222-222222222222"
const privateID = "33333333-3333-3333-3333-333333333333"

func raw(v string) json.RawMessage { return json.RawMessage(v) }
func fileArray(formats ...string) Field {
	return Field{Kind: "array", Items: &Field{Kind: "file", Formats: formats}}
}
func testLookup(_ context.Context, id string) (string, error) {
	switch id {
	case pngID:
		return "image/png", nil
	case jpegID:
		return "image/jpeg", nil
	default:
		return "", nil
	}
}

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	for _, version := range []int{1, 2} {
		convert := ActionDefinition{
			Descriptor: Descriptor{Key: Key{"fixture.convert", version}, Config: Fields{"format": {Kind: "string", Enum: []string{"png", "jpeg"}}},
				Inputs: Fields{"files": fileArray("image/png", "image/jpeg")}, Outputs: Fields{"files": fileArray("image/png", "image/jpeg")},
				MaxAttempts: 2, ResourceProfile: "image", FormatPairs: []FormatPair{{"image/png", "image/jpeg"}}},
			ConfirmStopped: func(context.Context, string) (bool, error) { return true, nil },
			Ports: func(config Values) (Fields, Fields) {
				var format string
				_ = json.Unmarshal(config["format"], &format)
				return Fields{"files": fileArray("image/png", "image/jpeg")}, Fields{"files": fileArray("image/" + format)}
			},
			Handler: func(context.Context, Values, Values) (Values, error) {
				return Values{"files": raw(`["` + jpegID + `"]`)}, nil
			},
		}
		if err := r.RegisterAction(convert); err != nil {
			t.Fatal(err)
		}
		if err := r.RegisterAction(convert); err == nil {
			t.Fatal("duplicate accepted")
		}
	}
	if err := r.RegisterAction(ActionDefinition{Descriptor: Descriptor{Key: Key{"fixture.consume", 1},
		Inputs: Fields{"files": fileArray("image/jpeg"), "quality": {Kind: "integer"}, "title": {Kind: "string"}}, Outputs: Fields{"ok": {Kind: "boolean"}}},
		Handler: func(context.Context, Values, Values) (Values, error) { return Values{"ok": raw("true")}, nil },
	}); err != nil {
		t.Fatal(err)
	}
	return r
}

func definition() Definition {
	return Definition{SchemaVersion: 1, Trigger: Trigger{Type: "core.manual", Version: 1}, UserInputs: Fields{"title": {Kind: "string"}},
		Steps: []Step{
			{ID: "convert", ActionType: "fixture.convert", ActionVersion: 1, Config: Values{"format": raw(`"jpeg"`)}, InputBindings: map[string]Binding{"files": {Source: "trigger", Output: "files"}}},
			{ID: "consume", ActionType: "fixture.consume", ActionVersion: 1, InputBindings: map[string]Binding{
				"files": {Source: "convert", Output: "files"}, "title": {Source: "user_input", Output: "title"}, "quality": {Source: "constant", Value: raw("90")},
			}},
		}}
}

func TestTypedChain(t *testing.T) {
	r := testRegistry(t)
	d := definition()
	info := BindingData{Trigger: Values{"files": raw(`["` + pngID + `"]`)}, UserInputs: Values{"title": raw(`"report"`)}, Steps: map[string]Values{}}
	for i, step := range d.Steps {
		input, err := r.Resolve(t.Context(), d, i, info, testLookup)
		if err != nil {
			t.Fatal(err)
		}
		out, err := r.Invoke(t.Context(), step, input, testLookup)
		if err != nil {
			t.Fatal(err)
		}
		info.Steps[step.ID] = out
	}
	if got := string(info.Steps["consume"]["ok"]); got != "true" {
		t.Fatal(got)
	}
	if err := r.ValidateOutput(t.Context(), d.Steps[0], Values{"files": raw(`["` + privateID + `"]`)}, testLookup); err == nil {
		t.Fatal("unavailable artifact output accepted")
	}
	if err := r.ValidateOutput(t.Context(), d.Steps[0], Values{"files": raw(`["` + pngID + `"]`)}, testLookup); err == nil {
		t.Fatal("wrong result format accepted")
	}
	if _, err := r.Invoke(t.Context(), d.Steps[0], Values{}, testLookup); err == nil {
		t.Fatal("missing input accepted")
	}
}

func TestInvalidGraphs(t *testing.T) {
	r := testRegistry(t)
	tests := []struct {
		code   string
		mutate func(*Definition)
	}{
		{"UNSUPPORTED_SCHEMA_VERSION", func(d *Definition) { d.SchemaVersion = 2 }},
		{"UNKNOWN_ACTION_VERSION", func(d *Definition) { d.Steps[0].ActionVersion = 3 }},
		{"UNKNOWN_TRIGGER_VERSION", func(d *Definition) { d.Trigger.Version = 2 }},
		{"INVALID_STEP_REFERENCE", func(d *Definition) { d.Steps[0].InputBindings["files"] = Binding{Source: "consume", Output: "ok"} }},
		{"INVALID_STEP_REFERENCE", func(d *Definition) { d.Steps[0].InputBindings["files"] = Binding{Source: "convert", Output: "files"} }},
		{"INVALID_STEP_ID", func(d *Definition) { d.Steps[1].ID = d.Steps[0].ID }},
		{"INVALID_VALUE", func(d *Definition) { d.Steps[0].Config["format"] = raw(`"pdf"`) }},
		{"TYPE_MISMATCH", func(d *Definition) { d.Steps[0].Config["format"] = raw(`"png"`) }},
		{"TYPE_MISMATCH", func(d *Definition) {
			d.Steps[1].InputBindings["quality"] = Binding{Source: "constant", Value: raw(`"90"`)}
		}},
		{"FILE_UNAVAILABLE", func(d *Definition) {
			d.Steps[0].InputBindings["files"] = Binding{Source: "constant", Value: raw(`["` + privateID + `"]`)}
		}},
		{"STEP_LIMIT", func(d *Definition) { d.Steps = make([]Step, 21) }},
		{"REQUIRED", func(d *Definition) { delete(d.Steps[1].InputBindings, "files") }},
	}
	for _, tc := range tests {
		t.Run(tc.code, func(t *testing.T) {
			d := definition()
			tc.mutate(&d)
			issues, err := r.Validate(t.Context(), d, testLookup)
			if err != nil || !slices.ContainsFunc(issues, func(p Problem) bool { return p.Code == tc.code && p.Path != "" }) {
				t.Fatalf("expected %s, got %+v %v", tc.code, issues, err)
			}
		})
	}
}

func TestRegistryAvailabilityAndImmutability(t *testing.T) {
	r := NewRegistry()
	available := false
	fields := Fields{"format": {Kind: "string", Enum: []string{"png"}}}
	a := ActionDefinition{Descriptor: Descriptor{Key: Key{"fixture.disabled", 1}, Config: fields}, Available: func() bool { return available },
		Handler: func(context.Context, Values, Values) (Values, error) { return nil, nil }}
	if err := r.RegisterAction(a); err != nil {
		t.Fatal(err)
	}
	fields["format"].Enum[0] = "pdf"
	step := Step{ID: "a", ActionType: a.Type, ActionVersion: 1, Config: Values{"format": raw(`"png"`)}}
	if err := r.ValidateOutput(t.Context(), step, nil, nil); err == nil {
		t.Fatal("disabled action accepted")
	}
	if len(r.Actions()) != 0 {
		t.Fatal("disabled action advertised")
	}
	available = true
	catalog := r.Actions()
	catalog[0].Config["format"].Enum[0] = "jpeg"
	if err := r.ValidateOutput(t.Context(), step, nil, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.Invoke(ctx, step, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled handler: %v", err)
	}
}

func TestDraftCanBeIncomplete(t *testing.T) {
	if !ValidDraft(raw(`{"steps":[{}]}`)) {
		t.Fatal("partial draft cannot be saved")
	}
	if ValidDraft(raw(`[]`)) {
		t.Fatal("invalid draft shape accepted")
	}
	if _, err := DecodeDefinition(raw(`{"arbitrary_code":"unsupported"}`)); err == nil {
		t.Fatal("unknown structure accepted")
	}
}
