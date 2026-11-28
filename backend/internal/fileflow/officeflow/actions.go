package officeflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"autolife/backend/internal/execution"
	"autolife/backend/internal/fileflow/processing"
	"autolife/backend/internal/htmlproto"
	"autolife/backend/internal/officeproto"
	"autolife/backend/internal/workflow"
	"slices"
)

const Profile = "office-transform"
const pdfMIME = "application/pdf"

type transformer interface {
	Available() bool
	ConfirmStopped(context.Context, string) (bool, error)
	Transform(context.Context, string, []byte, io.Reader, int64) (io.ReadCloser, int64, error)
}

type actions struct {
	objects  processing.Storage
	launcher transformer
}

func Register(registry *workflow.Registry, objects processing.Storage, launcher transformer) error {
	a := actions{objects, launcher}
	formats := officeproto.MIMEs()
	if err := a.register(registry, "file.document.to_pdf", Profile, formats[:len(formats)-1], workflow.Fields{}); err != nil {
		return err
	}
	return a.register(registry, "file.text.to_pdf", Profile, []string{"text/plain"}, layoutFields())
}

func RegisterHTML(registry *workflow.Registry, objects processing.Storage, launcher transformer) error {
	a := actions{objects, launcher}
	return a.register(registry, "file.html.to_pdf", htmlproto.ConvertProfile, []string{htmlproto.MIME}, layoutFields())
}

func layoutFields() workflow.Fields {
	return workflow.Fields{
		"page_size":   {Kind: "string", Optional: true, Enum: []string{"A4", "Letter"}},
		"orientation": {Kind: "string", Optional: true, Enum: []string{"portrait", "landscape"}},
		"margin_mm":   {Kind: "integer", Optional: true},
	}
}

func (a actions) register(registry *workflow.Registry, action, profile string, formats []string, config workflow.Fields) error {
	pairs := make([]workflow.FormatPair, len(formats))
	for i, mime := range formats {
		pairs[i] = workflow.FormatPair{Input: mime, Output: pdfMIME}
	}
	return registry.RegisterAction(workflow.ActionDefinition{
		Descriptor: workflow.Descriptor{
			Key:        workflow.Key{Type: action, Version: 1},
			Config:     config,
			Inputs:     workflow.Fields{"files": {Kind: "array", MaxItems: officeproto.MaxBatch, Items: &workflow.Field{Kind: "file", Formats: formats}}},
			Outputs:    workflow.Fields{"files": {Kind: "array", MaxItems: officeproto.MaxBatch, Items: &workflow.Field{Kind: "file", Formats: []string{pdfMIME}}}},
			Capability: profile, ResourceProfile: profile, FormatPairs: pairs, MaxAttempts: 3,
		},
		Available: func() bool { return a.objects != nil && a.launcher != nil && a.launcher.Available() },
		ConfirmStopped: func(ctx context.Context, id string) (bool, error) {
			if a.launcher == nil {
				return false, execution.ErrTemporary
			}
			return a.launcher.ConfirmStopped(ctx, id)
		},
		ValidateConfig: func(config workflow.Values) []workflow.Problem {
			if _, ok := header(config); !ok {
				return []workflow.Problem{{Code: "INVALID_VALUE", Path: "/margin_mm"}}
			}
			return nil
		},
		SafeReplay: true,
		Handler: func(ctx context.Context, cfg, in workflow.Values) (workflow.Values, error) {
			return a.run(ctx, cfg, in, formats, profile == htmlproto.ConvertProfile)
		},
	})
}

func header(config workflow.Values) ([]byte, bool) {
	result := []byte{'C', 0, 0, 0}
	var page, orientation string
	_ = json.Unmarshal(config["page_size"], &page)
	_ = json.Unmarshal(config["orientation"], &orientation)
	if page == "Letter" {
		result[1] = 1
	}
	if orientation == "landscape" {
		result[2] = 1
	}
	if raw, ok := config["margin_mm"]; ok {
		var value int
		if json.Unmarshal(raw, &value) != nil || value < 5 || value > 30 {
			return nil, false
		}
		result[3] = byte(value)
	}
	return result, true
}

func (a actions) run(ctx context.Context, cfg, in workflow.Values, formats []string, html bool) (workflow.Values, error) {
	var sources []string
	if json.Unmarshal(in["files"], &sources) != nil || len(sources) < 1 || len(sources) > officeproto.MaxBatch {
		return nil, execution.ErrInputs
	}
	params, valid := header(cfg)
	if !valid {
		return nil, execution.ErrInputs
	}
	maxInput, maxOutput := officeproto.MaxInput, officeproto.MaxOutput
	if html {
		maxInput, maxOutput = htmlproto.MaxInput, htmlproto.MaxOutput
		params[0] = 'H'
		if params[3] == 0 {
			params[3] = 20
		}
	}
	attempt, ok := execution.AttemptID(ctx)
	if !ok || a.objects == nil || a.launcher == nil {
		return nil, execution.ErrTemporary
	}
	usage := execution.FileUsage{OutputFormat: "pdf"}
	defer func() { _ = execution.ReportFileUsage(ctx, usage) }()
	results := make([]string, 0, len(sources))
	for index, id := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		usage.FailedIndex = index + 1
		meta, err := execution.ObjectMetadata(ctx, id)
		if err != nil {
			return nil, err
		}
		format := officeproto.Extension(meta.MIME)
		if html {
			format = "html"
		}
		if !slices.Contains(formats, meta.MIME) || meta.Bytes < 1 || meta.Bytes > int64(maxInput) {
			return nil, execution.ErrInputs
		}
		if usage.InputCount == 0 {
			usage.InputFormat = format
		} else if usage.InputFormat != format {
			usage.InputFormat = "mixed"
		}
		usage.InputCount++
		usage.InputBytes += meta.Bytes
		result, size, err := a.convert(ctx, attempt, meta, index, params, int64(maxOutput))
		if err != nil {
			return nil, err
		}
		results = append(results, result)
		usage.OutputCount++
		usage.OutputBytes += size
		_ = execution.ReportFileUsage(ctx, usage)
	}
	usage.FailedIndex = 0
	raw, err := json.Marshal(results)
	return workflow.Values{"files": raw}, err
}

func (a actions) convert(ctx context.Context, attempt string, meta execution.Object, index int, params []byte, maxOutput int64) (string, int64, error) {
	source, size, err := a.objects.Open(ctx, meta.Key)
	if err != nil {
		return "", 0, execution.ErrTemporary
	}
	defer source.Close()
	if size != meta.Bytes {
		return "", 0, execution.ErrInputs
	}
	result, length, err := a.launcher.Transform(ctx, attempt, params, source, size)
	if err != nil {
		return "", 0, err
	}
	defer result.Close()
	var prefix [5]byte
	if length < int64(len(prefix)) || length > maxOutput {
		return "", 0, execution.ErrInputs
	}
	if _, err := io.ReadFull(result, prefix[:]); err != nil || string(prefix[:]) != "%PDF-" {
		return "", 0, execution.ErrInputs
	}
	name := fmt.Sprintf("result-%03d.pdf", index+1)
	id, err := processing.Stage(ctx, a.objects, io.MultiReader(bytes.NewReader(prefix[:]), result), length, name, pdfMIME)
	return id, length, err
}
