package pdfflow

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"autolife/backend/internal/execution"
	"autolife/backend/internal/fileflow/processing"
	"autolife/backend/internal/pdfproto"
	"autolife/backend/internal/workflow"
)

type Launcher = processing.Launcher

func NewLauncher(socket string) (*Launcher, error) {
	return processing.NewLauncher(socket, "pdf-transform")
}

type transformer interface {
	Available() bool
	ConfirmStopped(context.Context, string) (bool, error)
	Transform(context.Context, string, []byte, io.Reader, int64) (io.ReadCloser, int64, error)
}
type actions struct {
	objects  processing.Storage
	launcher transformer
}

var pdf = workflow.Field{Kind: "file", Formats: []string{"application/pdf"}}

func Register(registry *workflow.Registry, objects processing.Storage, launcher transformer) error {
	a := actions{objects, launcher}
	for _, op := range []string{"merge", "split", "extract", "create"} {
		inputs, outputs, config := schemas(op)
		pairs := []workflow.FormatPair{{Input: "application/pdf", Output: "application/pdf"}}
		if op == "create" {
			pairs = nil
			for _, m := range []string{"image/jpeg", "image/png", "image/webp"} {
				pairs = append(pairs, workflow.FormatPair{Input: m, Output: "application/pdf"})
			}
		}
		err := registry.RegisterAction(workflow.ActionDefinition{
			Descriptor: workflow.Descriptor{Key: workflow.Key{Type: "file.pdf." + op, Version: 1}, Config: config,
				Inputs: inputs, Outputs: outputs, ResourceProfile: "pdf-transform", Capability: "pdf-transform", FormatPairs: pairs, MaxAttempts: 3},
			Available: func() bool { return objects != nil && launcher != nil && launcher.Available() },
			ConfirmStopped: func(ctx context.Context, id string) (bool, error) {
				if launcher == nil {
					return false, execution.ErrTemporary
				}
				return launcher.ConfirmStopped(ctx, id)
			},
			SafeReplay: true,
			ValidateConfig: func(cfg workflow.Values) []workflow.Problem {
				q := params(op, cfg)
				q.Sizes = []int64{1}
				if q.Validate() != nil {
					return []workflow.Problem{{Code: "INVALID_VALUE", Path: "/ranges"}}
				}
				return nil
			},
			Handler: func(ctx context.Context, cfg, inputs workflow.Values) (workflow.Values, error) {
				return a.run(ctx, op, cfg, inputs)
			},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func schemas(op string) (workflow.Fields, workflow.Fields, workflow.Fields) {
	inputs := workflow.Fields{"pdf": pdf}
	outputs := workflow.Fields{"pdf": pdf}
	config := workflow.Fields{}
	switch op {
	case "merge":
		inputs = workflow.Fields{"pdfs": {Kind: "array", Items: &pdf, MaxItems: pdfproto.MaxFiles}}
	case "split":
		config["ranges"] = workflow.Field{Kind: "array", Items: &workflow.Field{Kind: "string"}, Optional: true, MaxItems: pdfproto.MaxPages}
		outputs = workflow.Fields{"files": {Kind: "array", Items: &pdf, MaxItems: pdfproto.MaxPages}}
	case "extract":
		config["ranges"] = workflow.Field{Kind: "array", Items: &workflow.Field{Kind: "string"}, MaxItems: pdfproto.MaxPages}
	case "create":
		inputs = workflow.Fields{"images": {Kind: "array", MaxItems: pdfproto.MaxFiles, Items: &workflow.Field{
			Kind: "file", Formats: []string{"image/jpeg", "image/png", "image/webp"},
		}}}
		config["page_size"] = workflow.Field{Kind: "string", Optional: true, Enum: []string{"A4", "Letter", "auto"}}
		config["orientation"] = workflow.Field{Kind: "string", Optional: true, Enum: []string{"auto", "portrait", "landscape"}}
		config["fit"] = workflow.Field{Kind: "string", Optional: true, Enum: []string{"contain", "shrink"}}
	}
	return inputs, outputs, config
}

func params(op string, v workflow.Values) pdfproto.Request {
	q := pdfproto.Request{Operation: op}
	switch op {
	case "split", "extract":
		_ = json.Unmarshal(v["ranges"], &q.Ranges)
	case "create":
		q.PageSize, q.Orientation, q.Fit = "auto", "auto", "contain"
		_ = json.Unmarshal(v["page_size"], &q.PageSize)
		_ = json.Unmarshal(v["orientation"], &q.Orientation)
		_ = json.Unmarshal(v["fit"], &q.Fit)
	}
	return q
}

func (a actions) run(ctx context.Context, op string, cfg, in workflow.Values) (workflow.Values, error) {
	var ids []string
	switch op {
	case "merge":
		_ = json.Unmarshal(in["pdfs"], &ids)
	case "create":
		_ = json.Unmarshal(in["images"], &ids)
	default:
		var id string
		if json.Unmarshal(in["pdf"], &id) == nil {
			ids = []string{id}
		}
	}
	attempt, ok := execution.AttemptID(ctx)
	if !ok || a.objects == nil || a.launcher == nil {
		return nil, execution.ErrTemporary
	}
	if len(ids) < 1 || len(ids) > pdfproto.MaxFiles {
		return nil, execution.ErrInputs
	}
	q := params(op, cfg)
	usage := execution.FileUsage{InputCount: len(ids), OutputFormat: "pdf"}
	_ = execution.ReportFileUsage(ctx, usage)
	defer func() { _ = execution.ReportFileUsage(ctx, usage) }()
	objects := make([]execution.Object, 0, len(ids))
	for i, id := range ids {
		meta, err := execution.ObjectMetadata(ctx, id)
		if err != nil {
			usage.FailedIndex = i + 1
			return nil, err
		}
		q.Sizes = append(q.Sizes, meta.Bytes)
		usage.InputBytes += meta.Bytes
		format := map[string]string{"application/pdf": "pdf", "image/jpeg": "jpeg", "image/png": "png", "image/webp": "webp"}[meta.MIME]
		if usage.InputFormat == "" {
			usage.InputFormat = format
		} else if usage.InputFormat != format {
			usage.InputFormat = "mixed"
		}
		objects = append(objects, meta)
	}
	header, err := pdfproto.Header(q)
	if err != nil {
		return nil, execution.ErrInputs
	}
	sourceCtx, cancel := context.WithCancel(ctx)
	stream := &objectStream{ctx: sourceCtx, cancel: cancel, objects: a.objects, remaining: objects}
	defer stream.Close()
	result, declared, err := a.launcher.Transform(ctx, attempt, header, stream, usage.InputBytes)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	body := &io.LimitedReader{R: result, N: declared}
	var count uint32
	if err := binary.Read(body, binary.BigEndian, &count); err != nil || count < 1 || count > pdfproto.MaxPages || op != "split" && count != 1 {
		return nil, execution.ErrInputs
	}
	outputs := make([]string, 0, count)
	for i := uint32(0); i < count; i++ {
		var n int64
		if err := binary.Read(body, binary.BigEndian, &n); err != nil || n < 5 || n > pdfproto.MaxFileBytes || n > body.N || usage.OutputBytes+n > pdfproto.MaxBytes {
			return nil, execution.ErrInputs
		}
		file := &io.LimitedReader{R: body, N: n}
		var prefix [5]byte
		if _, err := io.ReadFull(file, prefix[:]); err != nil || string(prefix[:]) != "%PDF-" {
			return nil, execution.ErrInputs
		}
		name := fmt.Sprintf("result-%03d.pdf", i+1)
		id, err := processing.Stage(ctx, a.objects, io.MultiReader(bytes.NewReader(prefix[:]), file), n, name, "application/pdf")
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, id)
		usage.OutputBytes += n
		usage.OutputCount++
	}
	if body.N != 0 {
		return nil, execution.ErrInputs
	}
	var extra [1]byte
	if _, err := io.ReadFull(result, extra[:]); err != io.EOF {
		return nil, execution.ErrInputs
	}
	if op == "split" {
		raw, err := json.Marshal(outputs)
		return workflow.Values{"files": raw}, err
	}
	raw, err := json.Marshal(outputs[0])
	return workflow.Values{"pdf": raw}, err
}

// Open and stream one owner-scoped source at a time.
type objectStream struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	ctx       context.Context
	objects   processing.Storage
	remaining []execution.Object
	current   io.ReadCloser
	left      int64
}

func (s *objectStream) Close() error {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeCurrent()
}

func (s *objectStream) closeCurrent() error {
	if s.current == nil {
		return nil
	}
	err := s.current.Close()
	s.current = nil
	return err
}
func (s *objectStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	if s.current == nil {
		if len(s.remaining) == 0 {
			return 0, io.EOF
		}
		meta := s.remaining[0]
		s.remaining = s.remaining[1:]
		body, n, err := s.objects.Open(s.ctx, meta.Key)
		if err != nil {
			return 0, err
		}
		if n != meta.Bytes {
			body.Close()
			return 0, execution.ErrInputs
		}
		s.current, s.left = body, n
	}
	n, err := s.current.Read(p[:min(int64(len(p)), s.left)])
	s.left -= int64(n)
	if err != nil && err != io.EOF {
		return n, err
	}
	if err == io.EOF && s.left != 0 {
		return n, io.ErrUnexpectedEOF
	}
	if s.left == 0 {
		return n, s.closeCurrent()
	}
	return n, nil
}
