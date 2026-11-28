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
	return RegisterOperations(registry, objects, launcher, false)
}

func RegisterOperations(registry *workflow.Registry, objects processing.Storage, launcher transformer, rasterize bool) error {
	a := actions{objects, launcher}
	operations := []string{"merge", "split", "extract", "create"}
	if rasterize {
		operations = append(operations, "rasterize")
	}
	for _, op := range operations {
		inputs, outputs, config := schemas(op)
		pairs := []workflow.FormatPair{{Input: "application/pdf", Output: "application/pdf"}}
		if op == "create" {
			pairs = nil
			for _, m := range []string{"image/jpeg", "image/png", "image/webp"} {
				pairs = append(pairs, workflow.FormatPair{Input: m, Output: "application/pdf"})
			}
		}
		if op == "rasterize" {
			pairs = []workflow.FormatPair{{Input: "application/pdf", Output: "image/png"}, {Input: "application/pdf", Output: "image/jpeg"}}
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
			Ports: func(cfg workflow.Values) (workflow.Fields, workflow.Fields) {
				if op == "rasterize" {
					_, out, _ := schemas(op)
					q := params(op, cfg)
					field := out["files"]
					if q.MaxRasterPages >= 1 && q.MaxRasterPages <= pdfproto.MaxPages {
						field.MaxItems = q.MaxRasterPages
					}
					out["files"] = field
					if q.Format == "png" || q.Format == "jpeg" {
						out["files"].Items.Formats = []string{"image/" + q.Format}
					}
					return inputs, out
				}
				return inputs, outputs
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
	case "rasterize":
		inputs = workflow.Fields{"pdfs": {Kind: "array", Items: &pdf, MaxItems: pdfproto.MaxFiles}}
		outputs = workflow.Fields{"files": {Kind: "array", MaxItems: pdfproto.MaxPages, Items: &workflow.Field{
			Kind: "file", Formats: []string{"image/png", "image/jpeg"},
		}}}
		outputs["pages"] = workflow.Field{Kind: "array", MaxItems: pdfproto.MaxPages, Items: &workflow.Field{
			Kind: "object", Properties: workflow.Fields{
				"input_index": {Kind: "integer"}, "page": {Kind: "integer"},
			},
		}}
		config = workflow.Fields{
			"ranges":     {Kind: "array", Items: &workflow.Field{Kind: "string"}, Optional: true, MaxItems: pdfproto.MaxPages},
			"format":     {Kind: "string", Enum: []string{"png", "jpeg"}},
			"max_pages":  {Kind: "integer", Optional: true},
			"dpi":        {Kind: "integer", Optional: true},
			"quality":    {Kind: "integer", Optional: true},
			"background": {Kind: "string", Optional: true},
			"alpha":      {Kind: "boolean", Optional: true},
		}
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
	case "rasterize":
		q.DPI, q.Quality, q.MaxRasterPages, q.Background = 150, 85, 50, "#ffffff"
		_ = json.Unmarshal(v["format"], &q.Format)
		_ = json.Unmarshal(v["dpi"], &q.DPI)
		_ = json.Unmarshal(v["max_pages"], &q.MaxRasterPages)
		_ = json.Unmarshal(v["quality"], &q.Quality)
		_ = json.Unmarshal(v["background"], &q.Background)
		_ = json.Unmarshal(v["alpha"], &q.Alpha)
		_ = json.Unmarshal(v["ranges"], &q.Ranges)
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
	case "merge", "rasterize":
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
	outputFormat, mime, signature := "pdf", "application/pdf", "%PDF-"
	if op == "rasterize" {
		outputFormat = q.Format
		mime = "image/" + q.Format
		signature = map[string]string{"png": "\x89PNG\r\n\x1a\n", "jpeg": "\xff\xd8\xff"}[q.Format]
	}
	usage := execution.FileUsage{InputCount: len(ids), OutputFormat: outputFormat}
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
	var pages []pdfproto.SourcePage
	if op == "rasterize" {
		var n uint32
		if binary.Read(body, binary.BigEndian, &n) != nil || n < 1 || n > uint32(q.MaxRasterPages) {
			return nil, execution.ErrInputs
		}
		pages = make([]pdfproto.SourcePage, n)
		if binary.Read(body, binary.BigEndian, pages) != nil {
			return nil, execution.ErrInputs
		}
		var previous pdfproto.SourcePage
		for _, page := range pages {
			if page.Document < 1 || int(page.Document) > len(ids) || page.Page < 1 || page.Page > pdfproto.MaxPages ||
				page.Document < previous.Document || page.Document == previous.Document && page.Page <= previous.Page {
				return nil, execution.ErrInputs
			}
			previous = page
		}
	}
	var count uint32
	if err := binary.Read(body, binary.BigEndian, &count); err != nil || count < 1 || count > pdfproto.MaxPages || op == "rasterize" && int(count) != len(pages) || op != "split" && op != "rasterize" && count != 1 {
		return nil, execution.ErrInputs
	}
	outputs := make([]string, 0, count)
	for i := uint32(0); i < count; i++ {
		var n int64
		if err := binary.Read(body, binary.BigEndian, &n); err != nil || n < 5 || n > pdfproto.MaxFileBytes || n > body.N || usage.OutputBytes+n > pdfproto.MaxBytes {
			return nil, execution.ErrInputs
		}
		file := &io.LimitedReader{R: body, N: n}
		prefix := make([]byte, len(signature))
		if _, err := io.ReadFull(file, prefix); err != nil || string(prefix) != signature {
			return nil, execution.ErrInputs
		}
		name := fmt.Sprintf("result-%03d.%s", i+1, outputFormat)
		id, err := processing.Stage(ctx, a.objects, io.MultiReader(bytes.NewReader(prefix[:]), file), n, name, mime)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, id)
		usage.OutputBytes += n
		usage.OutputCount++
		if usage.OutputCount%10 == 0 {
			_ = execution.ReportFileUsage(ctx, usage)
		}
	}
	if body.N != 0 {
		return nil, execution.ErrInputs
	}
	var extra [1]byte
	if _, err := io.ReadFull(result, extra[:]); err != io.EOF {
		return nil, execution.ErrInputs
	}
	if op == "split" || op == "rasterize" {
		raw, err := json.Marshal(outputs)
		values := workflow.Values{"files": raw}
		if err == nil && op == "rasterize" {
			values["pages"], err = json.Marshal(pages)
		}
		return values, err
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
