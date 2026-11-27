package imageflow

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"autolife/backend/internal/execution"
	"autolife/backend/internal/fileflow/processing"
	"autolife/backend/internal/sandbox"
	"autolife/backend/internal/workflow"
)

type transformer interface {
	Transform(context.Context, string, []byte, io.Reader, int64) (io.ReadCloser, int64, error)
	ConfirmStopped(context.Context, string) (bool, error)
	Available() bool
}

type Actions struct {
	objects processing.Storage
	sandbox transformer
}

var formats = []string{"image/jpeg", "image/png", "image/webp"}

func Register(registry *workflow.Registry, objects processing.Storage, launcher transformer) error {
	actions := Actions{objects, launcher}
	for _, operation := range []string{"convert", "compress", "resize"} {
		config := workflow.Fields{}
		switch operation {
		case "convert":
			config["format"] = workflow.Field{Kind: "string", Enum: []string{"jpeg", "png", "webp"}}
			config["quality"] = workflow.Field{Kind: "integer", Optional: true}
			config["background"] = workflow.Field{Kind: "string", Optional: true}
		case "compress":
			config["mode"] = workflow.Field{Kind: "string", Optional: true, Enum: []string{"auto", "lossy", "lossless"}}
			config["quality"] = workflow.Field{Kind: "integer", Optional: true}
		case "resize":
			config["max_width"] = workflow.Field{Kind: "integer"}
			config["max_height"] = workflow.Field{Kind: "integer"}
			config["upscale"] = workflow.Field{Kind: "boolean", Optional: true}
			config["preserve_aspect_ratio"] = workflow.Field{Kind: "boolean", Optional: true}
		}
		ports := func(config workflow.Values) (workflow.Fields, workflow.Fields) {
			source, target := formats, formats
			switch operation {
			case "convert":
				if f := text(config, "format"); f != "" {
					target = []string{"image/" + f}
				}
			case "compress":
				switch text(config, "mode") {
				case "lossy":
					source = []string{"image/jpeg", "image/webp"}
				case "lossless":
					source = []string{"image/png", "image/webp"}
				}
				target = source
			}
			return filePort(source), outputPort(target)
		}
		pairs := make([]workflow.FormatPair, 0, 9)
		for _, i := range formats {
			for _, o := range formats {
				if operation == "convert" || i == o {
					pairs = append(pairs, workflow.FormatPair{Input: i, Output: o})
				}
			}
		}
		err := registry.RegisterAction(workflow.ActionDefinition{
			Descriptor: workflow.Descriptor{Key: workflow.Key{Type: "file.image." + operation, Version: 1}, Config: config,
				Inputs: filePort(formats), Outputs: outputPort(formats), FormatPairs: pairs, Capability: "image-transform", ResourceProfile: "image-transform", MaxAttempts: 3},
			Available: func() bool { return objects != nil && launcher != nil && launcher.Available() },
			ConfirmStopped: func(ctx context.Context, id string) (bool, error) {
				if launcher == nil {
					return false, execution.ErrTemporary
				}
				return launcher.ConfirmStopped(ctx, id)
			},
			SafeReplay: true,
			ValidateConfig: func(cfg workflow.Values) []workflow.Problem {
				_, issue := parameters(operation, cfg)
				if issue != nil {
					return []workflow.Problem{*issue}
				}
				return nil
			},
			Ports: ports,
			Handler: func(ctx context.Context, cfg, in workflow.Values) (workflow.Values, error) {
				return actions.apply(ctx, operation, cfg, in)
			},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func filePort(mimes []string) workflow.Fields {
	return workflow.Fields{"files": {Kind: "array", MaxItems: 50, Items: &workflow.Field{Kind: "file", Formats: mimes}}}
}
func outputPort(mimes []string) workflow.Fields {
	fields := filePort(mimes)
	fields["sizes"] = workflow.Field{Kind: "array", MaxItems: 50, Items: &workflow.Field{Kind: "object", Properties: workflow.Fields{
		"input_bytes": {Kind: "integer"}, "output_bytes": {Kind: "integer"}, "reduced": {Kind: "boolean"},
	}}}
	return fields
}
func text(cfg workflow.Values, name string) string {
	var value string
	_ = json.Unmarshal(cfg[name], &value)
	return value
}

func parameters(operation string, cfg workflow.Values) ([]byte, *workflow.Problem) {
	header := make([]byte, sandbox.ImageHeaderBytes)
	copy(header, "ALI1")
	header[5] = 85
	header[7], header[8], header[9] = 255, 255, 255
	invalid := func(field string) ([]byte, *workflow.Problem) {
		return nil, &workflow.Problem{Code: "INVALID_VALUE", Path: "/" + field}
	}
	if raw, ok := cfg["quality"]; ok {
		var n int
		if json.Unmarshal(raw, &n) != nil || n < 1 || n > 100 {
			return invalid("quality")
		}
		header[5] = byte(n)
	}
	switch operation {
	case "convert":
		switch text(cfg, "format") {
		case "jpeg":
			header[4] = 1
		case "png":
			header[4] = 2
		case "webp":
			header[4] = 3
		default:
			return invalid("format")
		}
		if raw, ok := cfg["background"]; ok {
			bg := text(cfg, "background")
			if len(raw) == 0 || len(bg) != 7 || bg[0] != '#' || header[4] != 1 {
				return invalid("background")
			}
			rgb, err := hex.DecodeString(bg[1:])
			if err != nil {
				return invalid("background")
			}
			copy(header[7:10], rgb)
		}
	case "compress":
		if text(cfg, "mode") == "lossless" {
			header[6] |= 1
		}
	case "resize":
		header[6] |= 8
		preserve := true
		if raw, ok := cfg["preserve_aspect_ratio"]; ok {
			_ = json.Unmarshal(raw, &preserve)
		}
		if preserve {
			header[6] |= 2
		}
		var upscale bool
		_ = json.Unmarshal(cfg["upscale"], &upscale)
		if upscale {
			header[6] |= 4
		}
		for i, key := range []string{"max_width", "max_height"} {
			var value uint32
			if json.Unmarshal(cfg[key], &value) != nil || value < 1 || value > 40_000_000 {
				return invalid(key)
			}
			binary.BigEndian.PutUint32(header[10+4*i:], value)
		}
	}
	return header, nil
}

type sizes struct {
	Input         int64 `json:"input_bytes"`
	Output        int64 `json:"output_bytes"`
	Reduced       bool  `json:"reduced"`
	input, output byte
}

func (a Actions) apply(ctx context.Context, operation string, cfg, in workflow.Values) (workflow.Values, error) {
	var files []string
	if json.Unmarshal(in["files"], &files) != nil || len(files) < 1 || len(files) > 50 {
		return nil, execution.ErrInputs
	}
	base, problem := parameters(operation, cfg)
	if problem != nil {
		return nil, execution.ErrInputs
	}
	attempt, ok := execution.AttemptID(ctx)
	if !ok || a.objects == nil || a.sandbox == nil {
		return nil, execution.ErrTemporary
	}
	usage := execution.FileUsage{}
	_ = execution.ReportFileUsage(ctx, usage)
	defer func() { _ = execution.ReportFileUsage(ctx, usage) }()
	outputs := make([]string, 0, len(files))
	measured := make([]sizes, 0, len(files))
	for i, id := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		usage.InputCount++
		out, size, err := a.one(ctx, attempt, id, i, base)
		usage.InputBytes += size.Input
		usage.InputFormat = mergeFormat(usage.InputFormat, size.input)
		if err != nil {
			usage.FailedIndex = i + 1
			return nil, err
		}
		usage.OutputCount++
		usage.OutputBytes += size.Output
		usage.OutputFormat = mergeFormat(usage.OutputFormat, size.output)
		outputs = append(outputs, out)
		measured = append(measured, size)
	}
	ids, err := json.Marshal(outputs)
	if err != nil {
		return nil, err
	}
	summary, err := json.Marshal(measured)
	if err != nil {
		return nil, err
	}
	return workflow.Values{"files": ids, "sizes": summary}, nil
}

func (a Actions) one(ctx context.Context, attempt, id string, index int, header []byte) (string, sizes, error) {
	var measured sizes
	key, err := execution.ObjectKey(ctx, id)
	if err != nil {
		return "", measured, err
	}
	body, bytesIn, err := a.objects.Open(ctx, key)
	if err != nil {
		return "", measured, execution.ErrTemporary
	}
	defer body.Close()
	measured.Input = bytesIn
	var prefix [12]byte
	if _, err := io.ReadFull(body, prefix[:]); err != nil {
		return "", measured, execution.ErrInputs
	}
	inputFormat := detect(prefix[:])
	measured.input = inputFormat
	if inputFormat == 0 {
		return "", measured, execution.ErrInputs
	}
	outputFormat := header[4]
	if outputFormat == 0 {
		outputFormat = inputFormat
	}
	result, declared, err := a.sandbox.Transform(ctx, attempt, header, io.MultiReader(bytes.NewReader(prefix[:]), body), bytesIn)
	if err != nil {
		return "", measured, err
	}
	defer result.Close()
	if declared < 12 || declared > 100_000_000 {
		return "", measured, execution.ErrInputs
	}
	if _, err := io.ReadFull(result, prefix[:]); err != nil || detect(prefix[:]) != outputFormat {
		return "", measured, execution.ErrInputs
	}
	name := []string{"", "jpeg", "png", "webp"}[outputFormat]
	output, err := processing.Stage(ctx, a.objects, io.MultiReader(bytes.NewReader(prefix[:]), result), declared,
		fmt.Sprintf("image-%02d.%s", index+1, name), "image/"+name)
	measured.Output, measured.Reduced, measured.output = declared, declared < bytesIn, outputFormat
	return output, measured, err
}

func detect(b []byte) byte {
	if len(b) < 12 {
		return 0
	}
	switch {
	case bytes.HasPrefix(b, []byte{0xff, 0xd8, 0xff}):
		return 1
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return 2
	case string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return 3
	default:
		return 0
	}
}

func mergeFormat(current string, id byte) string {
	if id == 0 {
		return current
	}
	next := []string{"", "jpeg", "png", "webp"}[id]
	if current == "" || current == next {
		return next
	}
	return "mixed"
}
