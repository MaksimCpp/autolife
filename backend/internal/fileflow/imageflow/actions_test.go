package imageflow

import (
	"encoding/binary"
	"encoding/json"
	"testing"

	"autolife/backend/internal/workflow"
)

func TestParameters(t *testing.T) {
	v := func(s string) json.RawMessage { return json.RawMessage(s) }
	tests := []struct {
		op   string
		cfg  workflow.Values
		path string
	}{
		{"convert", workflow.Values{"format": v(`"heic"`)}, "/format"},
		{"convert", workflow.Values{"format": v(`"webp"`), "background": v(`"#ff0011"`)}, "/background"},
		{"convert", workflow.Values{"format": v(`"jpeg"`), "background": v(`"#ffoops"`)}, "/background"},
		{"compress", workflow.Values{"quality": v("0")}, "/quality"},
		{"compress", workflow.Values{"quality": v("101")}, "/quality"},
		{"resize", workflow.Values{"max_width": v("0"), "max_height": v("100")}, "/max_width"},
		{"resize", workflow.Values{"max_width": v("100"), "max_height": v("40000001")}, "/max_height"},
	}
	for _, tt := range tests {
		t.Run(tt.op+tt.path, func(t *testing.T) {
			_, issue := parameters(tt.op, tt.cfg)
			if issue == nil || issue.Path != tt.path {
				t.Fatalf("got %+v", issue)
			}
		})
	}
	header, issue := parameters("resize", workflow.Values{"max_width": v("1024"), "max_height": v("768")})
	if issue != nil || header[6] != 10 || binary.BigEndian.Uint32(header[10:14]) != 1024 ||
		binary.BigEndian.Uint32(header[14:18]) != 768 {
		t.Fatalf("defaults: %v %v", header, issue)
	}
	header, issue = parameters("convert", workflow.Values{"format": v(`"jpeg"`), "background": v(`"#ff1122"`), "quality": v("1")})
	if issue != nil || header[4] != 1 || header[5] != 1 || header[7] != 255 || header[8] != 17 || header[9] != 34 {
		t.Fatalf("encode: %v %v", header, issue)
	}
}

func TestUnavailableProfileHidden(t *testing.T) {
	r := workflow.NewRegistry()
	if err := Register(r, nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(r.Actions()) != 0 {
		t.Fatal("capabilities advertised without a launcher")
	}
}
