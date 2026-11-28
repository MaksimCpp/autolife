package main

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"autolife/backend/internal/tableproto"
	"github.com/xuri/excelize/v2"
)

const (
	maxRows     = 100_000
	maxColumns  = 256
	maxCells    = 1_000_000
	maxExpanded = 96 << 20
)

var invalid = errors.New("invalid bounded table")

type warnings struct{ missing, escaped uint32 }

func main() {
	if run(os.Stdin, os.Stdout) != nil {
		os.Exit(20)
	}
}

func run(source io.Reader, destination io.Writer) error {
	var head [tableproto.HeaderBytes]byte
	if _, err := io.ReadFull(source, head[:1]); err != nil {
		return err
	}
	inspect := head[0] == 'I'
	var options tableproto.Options
	if !inspect {
		if _, err := io.ReadFull(source, head[1:]); err != nil {
			return err
		}
		var ok bool
		options, ok = tableproto.Parse(head)
		if !ok {
			return invalid
		}
	}
	data, err := io.ReadAll(io.LimitReader(source, tableproto.MaxBytes+1))
	if err != nil || len(data) == 0 || len(data) > tableproto.MaxBytes {
		return invalid
	}
	if inspect {
		for _, delim := range []byte{',', ';', '\t'} {
			if readCSV(data, delim, func(_ int, _ []string) error { return nil }) == nil {
				_, err := io.WriteString(destination, `{"mime":"text/csv"}`)
				return err
			}
		}
		return invalid
	}
	output := new(bytes.Buffer)
	bounded := &boundedWriter{out: output, left: tableproto.MaxBytes}
	var stats warnings
	if options.ToXLSX {
		err = toXLSX(data, options, bounded)
		if err == nil {
			err = inspectXLSX(output.Bytes())
		}
	} else {
		stats, err = toCSV(data, options, bounded)
		if err == nil {
			err = readCSV(output.Bytes(), options.Delimiter, func(_ int, _ []string) error { return nil })
		}
	}
	if err != nil || output.Len() == 0 {
		return invalid
	}
	header := binary.BigEndian.AppendUint32(nil, stats.missing)
	header = binary.BigEndian.AppendUint32(header, stats.escaped)
	if _, err = destination.Write(header); err != nil {
		return err
	}
	_, err = output.WriteTo(destination)
	return err
}

type boundedWriter struct {
	out  io.Writer
	left int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, invalid
	}
	n, err := w.out.Write(p)
	w.left -= n
	return n, err
}

func readCSV(data []byte, delimiter byte, row func(int, []string) error) error {
	if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return invalid
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	reader := csv.NewReader(bytes.NewReader(data))
	reader.Comma = rune(delimiter)
	reader.FieldsPerRecord = -1
	cells := 0
	for index := 1; ; index++ {
		values, err := reader.Read()
		if err == io.EOF && index > 1 {
			return nil
		}
		cells += len(values)
		if err != nil || index > maxRows || len(values) > maxColumns || cells > maxCells {
			return invalid
		}
		for _, value := range values {
			if utf8.RuneCountInString(value) > 16383 || strings.ContainsAny(value, "\xfffe\xffff") ||
				strings.IndexFunc(value, func(c rune) bool { return c < 32 && c != '\r' && c != '\n' && c != '\t' }) >= 0 {
				return invalid
			}
		}
		if err := row(index, values); err != nil {
			return err
		}
	}
}

func toXLSX(data []byte, options tableproto.Options, out io.Writer) error {
	book := excelize.NewFile(excelize.Options{TmpDir: "/work"})
	defer book.Close()
	stream, err := book.NewStreamWriter(book.GetSheetName(0))
	if err != nil {
		return err
	}
	if err := readCSV(data, options.Delimiter, func(index int, row []string) error {
		values := make([]any, len(row))
		for i, value := range row {
			values[i] = value
		}
		cell, err := excelize.CoordinatesToCellName(1, index)
		if err != nil {
			return err
		}
		return stream.SetRow(cell, values)
	}); err != nil {
		return err
	}
	if err := stream.Flush(); err != nil {
		return err
	}
	return book.Write(out)
}

func inspectXLSX(data []byte) error {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(archive.File) > 2048 {
		return invalid
	}
	seen := make(map[string]bool)
	var total uint64
	for _, file := range archive.File {
		name := strings.TrimSuffix(file.Name, "/")
		key := strings.ToLower(name)
		size := file.UncompressedSize64
		total += size
		if name == "." || name == "" || !strings.EqualFold(path.Clean(name), name) ||
			strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\:") ||
			seen[key] || file.Flags&1 != 0 || !file.FileInfo().Mode().IsRegular() && !file.FileInfo().IsDir() ||
			size > 24<<20 || total > maxExpanded || size > max(1<<20, 200*min(file.CompressedSize64, tableproto.MaxBytes)) ||
			strings.HasSuffix(key, ".bin") || strings.Contains(key, "externallinks/") || strings.Contains(key, "embeddings/") ||
			strings.Contains(key, "activex/") || strings.Contains(key, "vbaproject") {
			return invalid
		}
		seen[key] = true
		if file.FileInfo().IsDir() {
			continue
		}
		body, err := file.Open()
		if err != nil {
			return err
		}
		var readErr error
		if strings.HasSuffix(key, ".xml") || strings.HasSuffix(key, ".rels") {
			readErr = inspectXML(body)
		} else {
			_, readErr = io.Copy(io.Discard, body)
		}
		_ = body.Close()
		if readErr != nil {
			return readErr
		}
	}
	if !seen["xl/workbook.xml"] || !seen["[content_types].xml"] || !seen["_rels/.rels"] {
		return invalid
	}
	return nil
}

func inspectXML(body io.Reader) error {
	decoder := xml.NewDecoder(body)
	depth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF && depth == 0 {
			return nil
		}
		if err != nil {
			return err
		}
		switch t := token.(type) {
		case xml.Directive:
			return invalid
		case xml.StartElement:
			depth++
			if depth > 64 || len(t.Attr) > 128 {
				return invalid
			}
			for _, attr := range t.Attr {
				if attr.Name.Local == "TargetMode" && !strings.EqualFold(attr.Value, "Internal") ||
					attr.Name.Local == "ContentType" && strings.Contains(strings.ToLower(attr.Value), "macroenabled") {
					return invalid
				}
			}
		case xml.EndElement:
			depth--
		}
	}
}

func toCSV(data []byte, options tableproto.Options, out io.Writer) (warnings, error) {
	var stats warnings
	if err := inspectXLSX(data); err != nil {
		return stats, err
	}
	book, err := excelize.OpenReader(bytes.NewReader(data), excelize.Options{
		RawCellValue: true, TmpDir: "/work", UnzipSizeLimit: maxExpanded, UnzipXMLSizeLimit: 4 << 20,
	})
	if err != nil {
		return stats, err
	}
	defer book.Close()
	sheet := book.GetSheetName(int(options.Sheet) - 1)
	if sheet == "" {
		return stats, invalid
	}
	rows, err := book.Rows(sheet)
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	writer := csv.NewWriter(out)
	writer.Comma = rune(options.Delimiter)
	cells := 0
	for index := 1; rows.Next(); index++ {
		line, err := rows.Columns()
		cells += len(line)
		if err != nil || index > maxRows || len(line) > maxColumns || cells > maxCells {
			return stats, invalid
		}
		if len(line) == 0 {
			line = []string{""}
		}
		for column, value := range line {
			if value == "" {
				cell, err := excelize.CoordinatesToCellName(column+1, index)
				if err != nil {
					return stats, err
				}
				formula, err := book.GetCellFormula(sheet, cell)
				if err != nil {
					return stats, err
				}
				if formula != "" {
					stats.missing++
				}
			}
			if formulaPrefix(value) {
				stats.escaped++
				line[column] = "'" + value
			}
		}
		// encoding/csv emits a blank line for one empty value; keep it a record.
		if len(line) == 1 && line[0] == "" {
			writer.Flush()
			if _, err := io.WriteString(out, "\"\"\n"); err != nil {
				return stats, err
			}
		} else if err := writer.Write(line); err != nil {
			return stats, err
		}
	}
	writer.Flush()
	return stats, errors.Join(rows.Error(), writer.Error())
}

func formulaPrefix(value string) bool {
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") {
		return true
	}
	value = strings.TrimLeftFunc(value, func(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' })
	return value != "" && strings.ContainsRune("=+-@＝＋－＠", []rune(value)[0])
}
