package officeflow

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"autolife/backend/internal/workflow"
)

type readyLauncher bool

func (l readyLauncher) Available() bool                                    { return bool(l) }
func (readyLauncher) ConfirmStopped(context.Context, string) (bool, error) { return true, nil }
func (readyLauncher) Transform(context.Context, string, []byte, io.Reader, int64) (io.ReadCloser, int64, error) {
	panic("catalog should not invoke transform")
}

func TestOfficeCatalog(t *testing.T) {
	registry := workflow.NewRegistry()
	if err := Register(registry, nil, readyLauncher(true)); err != nil {
		t.Fatal(err)
	}
	if n := len(registry.Actions()); n != 0 {
		t.Fatalf("no storage, but %d actions visible", n)
	}
}

func TestTextPrintOptions(t *testing.T) {
	params, ok := header(workflow.Values{
		"page_size":   json.RawMessage(`"Letter"`),
		"orientation": json.RawMessage(`"landscape"`),
		"margin_mm":   json.RawMessage("15"),
	})
	if !ok || string(params) != "C\x01\x01\x0f" {
		t.Fatalf("print options sent as %v", params)
	}
	for _, value := range []string{"0", "31", "1.5", "null", `"5"`} {
		if _, ok := header(workflow.Values{"margin_mm": json.RawMessage(value)}); ok {
			t.Fatalf("accepted margin %s", value)
		}
	}
}
