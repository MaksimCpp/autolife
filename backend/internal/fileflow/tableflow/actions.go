package tableflow

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"

	"autolife/backend/internal/execution"
	"autolife/backend/internal/fileflow/processing"
	"autolife/backend/internal/tableproto"
	"autolife/backend/internal/workflow"
)

type transformer interface {
	Available() bool
	ConfirmStopped(context.Context, string) (bool, error)
	Transform(context.Context, string, []byte, io.Reader, int64) (io.ReadCloser, int64, error)
}

type action struct {
	store    processing.Storage
	launcher transformer
}

func files(formats ...string) workflow.Fields {
	return workflow.Fields{"files": {Kind: "array", MaxItems: tableproto.Batch, Items: &workflow.Field{Kind: "file", Formats: formats}}}
}

func outputs(formats ...string) workflow.Fields {
	fields := files(formats...)
	fields["missing_formula_values"] = workflow.Field{Kind: "integer"}
	fields["escaped_values"] = workflow.Field{Kind: "integer"}
	return fields
}

func Register(registry *workflow.Registry, store processing.Storage, launcher transformer) error {
	a := action{store, launcher}
	return registry.RegisterAction(workflow.ActionDefinition{
		Descriptor: workflow.Descriptor{
			Key: workflow.Key{Type: "file.table.convert", Version: 1},
			Config: workflow.Fields{
				"format":    {Kind: "string", Enum: []string{"csv", "xlsx"}},
				"delimiter": {Kind: "string", Optional: true, Enum: []string{",", ";", "\t"}},
				"sheet":     {Kind: "integer", Optional: true},
			},
			Inputs: files(tableproto.CSV, tableproto.XLSX), Outputs: outputs(tableproto.CSV, tableproto.XLSX),
			FormatPairs:     []workflow.FormatPair{{Input: tableproto.CSV, Output: tableproto.XLSX}, {Input: tableproto.XLSX, Output: tableproto.CSV}},
			ResourceProfile: tableproto.ConvertProfile, Capability: tableproto.ConvertProfile, MaxAttempts: 3,
		},
		Available: func() bool { return store != nil && launcher != nil && launcher.Available() },
		ConfirmStopped: func(ctx context.Context, id string) (bool, error) {
			if launcher == nil {
				return false, execution.ErrTemporary
			}
			return launcher.ConfirmStopped(ctx, id)
		},
		Ports: func(cfg workflow.Values) (workflow.Fields, workflow.Fields) {
			o, ok := options(cfg)
			if !ok {
				return files(tableproto.CSV, tableproto.XLSX), outputs(tableproto.CSV, tableproto.XLSX)
			}
			if o.ToXLSX {
				return files(tableproto.CSV), outputs(tableproto.XLSX)
			}
			return files(tableproto.XLSX), outputs(tableproto.CSV)
		},
		ValidateConfig: func(cfg workflow.Values) []workflow.Problem {
			if _, ok := options(cfg); !ok {
				return []workflow.Problem{{Code: "INVALID_VALUE", Path: "/sheet"}}
			}
			return nil
		},
		SafeReplay: true,
		Handler:    a.run,
	})
}

func options(cfg workflow.Values) (tableproto.Options, bool) {
	var format, delimiter string
	var sheet uint16 = 1
	_ = json.Unmarshal(cfg["format"], &format)
	delimiter = ","
	if raw := cfg["delimiter"]; raw != nil && json.Unmarshal(raw, &delimiter) != nil {
		return tableproto.Options{}, false
	}
	if raw := cfg["sheet"]; raw != nil && json.Unmarshal(raw, &sheet) != nil {
		return tableproto.Options{}, false
	}
	if len(delimiter) != 1 || format != "csv" && format != "xlsx" {
		return tableproto.Options{}, false
	}
	o := tableproto.Options{ToXLSX: format == "xlsx", Delimiter: delimiter[0], Sheet: sheet}
	return o, o.Valid()
}

func (a action) run(ctx context.Context, cfg, in workflow.Values) (workflow.Values, error) {
	o, valid := options(cfg)
	var ids []string
	if !valid || json.Unmarshal(in["files"], &ids) != nil || len(ids) < 1 || len(ids) > tableproto.Batch {
		return nil, execution.ErrInputs
	}
	attempt, active := execution.AttemptID(ctx)
	if !active || a.store == nil || a.launcher == nil {
		return nil, execution.ErrTemporary
	}
	input, output, inputFormat, extension := tableproto.XLSX, tableproto.CSV, "xlsx", "csv"
	if o.ToXLSX {
		input, output, inputFormat, extension = tableproto.CSV, tableproto.XLSX, "csv", "xlsx"
	}
	usage := execution.FileUsage{InputFormat: inputFormat, OutputFormat: extension}
	defer func() { _ = execution.ReportFileUsage(ctx, usage) }()
	result := make([]string, 0, len(ids))
	var missing, escaped uint64
	for i, id := range ids {
		usage.FailedIndex = i + 1
		meta, err := execution.ObjectMetadata(ctx, id)
		if err != nil {
			return nil, err
		}
		if meta.MIME != input || meta.Bytes < 1 || meta.Bytes > tableproto.MaxBytes {
			return nil, execution.ErrInputs
		}
		usage.InputBytes += meta.Bytes
		usage.InputCount++
		out, length, stats, err := a.one(ctx, attempt, o, meta, output, fmt.Sprintf("result-%02d.%s", i+1, extension))
		if err != nil {
			return nil, err
		}
		result = append(result, out)
		usage.OutputBytes += length
		usage.OutputCount++
		missing += uint64(stats[0])
		escaped += uint64(stats[1])
		_ = execution.ReportFileUsage(ctx, usage)
	}
	usage.FailedIndex = 0
	raw, err := json.Marshal(result)
	return workflow.Values{"files": raw, "missing_formula_values": json.RawMessage(fmt.Sprint(missing)), "escaped_values": json.RawMessage(fmt.Sprint(escaped))}, err
}

func (a action) one(ctx context.Context, attempt string, o tableproto.Options, meta execution.Object, mime, name string) (string, int64, [2]uint32, error) {
	var stats [2]uint32
	in, size, err := a.store.Open(ctx, meta.Key)
	if err != nil {
		return "", 0, stats, execution.ErrTemporary
	}
	defer in.Close()
	if size != meta.Bytes {
		return "", 0, stats, execution.ErrInputs
	}
	out, size, err := a.launcher.Transform(ctx, attempt, o.Header(), in, size)
	if err != nil {
		return "", 0, stats, err
	}
	defer out.Close()
	if size <= 8 || size > tableproto.MaxBytes+8 || binary.Read(out, binary.BigEndian, &stats) != nil ||
		uint64(stats[0])+uint64(stats[1]) > 1_000_000 {
		return "", 0, stats, execution.ErrInputs
	}
	var body io.Reader = out
	if o.ToXLSX {
		var magic [4]byte
		if _, err := io.ReadFull(out, magic[:]); err != nil || string(magic[:]) != "PK\x03\x04" {
			return "", 0, stats, execution.ErrInputs
		}
		body = io.MultiReader(bytes.NewReader(magic[:]), out)
	}
	id, err := processing.Stage(ctx, a.store, body, size-8, name, mime)
	return id, size - 8, stats, err
}
