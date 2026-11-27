package main

import (
	"os"
	"path/filepath"
	"testing"

	"autolife/backend/internal/pdfproto"
)

func TestPDFResultBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "result.pdf")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(pdfproto.MaxFileBytes); err != nil {
		t.Fatal(err)
	}
	if n, err := pdfSize(path); err != nil || n != pdfproto.MaxFileBytes {
		t.Fatalf("inclusive 100 MB: %d %v", n, err)
	}
	if err := f.Truncate(pdfproto.MaxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if _, err := pdfSize(path); err == nil {
		t.Fatal("accepted oversized output")
	}
}
