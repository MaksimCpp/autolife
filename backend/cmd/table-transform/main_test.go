package main

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"autolife/backend/internal/tableproto"
	"github.com/xuri/excelize/v2"
)

func TestRoundTripTextCells(t *testing.T) {
	source := []byte("\xef\xbb\xbf001;Привет;\"строка\nещё\";=1+1\r\n")
	var encoded bytes.Buffer
	toWorkbook := tableproto.Options{ToXLSX: true, Delimiter: ';', Sheet: 1}
	if err := run(bytes.NewReader(append(toWorkbook.Header(), source...)), &encoded); err != nil {
		t.Fatal(err)
	}
	book, err := excelize.OpenReader(bytes.NewReader(encoded.Bytes()[8:]))
	if err != nil {
		t.Fatal(err)
	}
	defer book.Close()
	for cell, want := range map[string]string{"A1": "001", "B1": "Привет", "C1": "строка\nещё", "D1": "=1+1"} {
		got, err := book.GetCellValue("Sheet1", cell)
		if err != nil || got != want {
			t.Fatalf("%s: %q (%v)", cell, got, err)
		}
	}
	if formula, err := book.GetCellFormula("Sheet1", "D1"); err != nil || formula != "" {
		t.Fatalf("imported formula: %q (%v)", formula, err)
	}
	var decoded bytes.Buffer
	export := tableproto.Options{Delimiter: ';', Sheet: 1}
	if err := run(bytes.NewReader(append(export.Header(), encoded.Bytes()[8:]...)), &decoded); err != nil {
		t.Fatal(err)
	}
	if got := decoded.String()[8:]; got != "001;Привет;\"строка\nещё\";'=1+1\n" || binary.BigEndian.Uint32(decoded.Bytes()[4:]) != 1 {
		t.Fatalf("incorrect escaped CSV: %q", got)
	}
}

func TestMissingFormulaAndSheet(t *testing.T) {
	book := excelize.NewFile()
	defer book.Close()
	if _, err := book.NewSheet("Second"); err != nil {
		t.Fatal(err)
	}
	if err := book.SetCellFormula("Second", "A1", "=1+2"); err != nil {
		t.Fatal(err)
	}
	if err := book.SetCellValue("Second", "B1", "ok"); err != nil {
		t.Fatal(err)
	}
	data, err := book.WriteToBuffer()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	stats, err := toCSV(data.Bytes(), tableproto.Options{Delimiter: ',', Sheet: 2}, &output)
	if err != nil || stats.missing != 1 || output.String() != ",ok\n" {
		t.Fatalf("selected sheet: %v %v %q", stats, err, output.String())
	}
}

func TestCSVLimits(t *testing.T) {
	if err := readCSV([]byte(strings.Repeat("x,", maxColumns)+"x"), ',', func(_ int, _ []string) error { return nil }); err == nil {
		t.Fatal("oversized record accepted")
	}
	if ok := formulaPrefix(" \ufeff＋1"); !ok {
		t.Fatal("escaped fullwidth formula prefix not detected")
	}
}
