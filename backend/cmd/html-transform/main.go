package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"autolife/backend/internal/htmlproto"
	"autolife/backend/internal/pdfcheck"
)

func main() {
	os.Exit(run(os.Stdin, os.Stdout))
}

func run(input io.Reader, output io.Writer) int {
	var header [htmlproto.HeaderBytes]byte
	if _, err := io.ReadFull(input, header[:1]); err != nil {
		return 20
	}
	inspect := header[0] == 'I'
	layout := htmlproto.Layout{Margin: 20}
	if !inspect {
		if _, err := io.ReadFull(input, header[1:]); err != nil {
			return 20
		}
		var ok bool
		layout, ok = htmlproto.Parse(header)
		if !ok {
			return 20
		}
	}
	data, err := io.ReadAll(io.LimitReader(input, htmlproto.MaxInput+1))
	if err != nil {
		return 1
	}
	safe, err := sanitize(data, layout)
	if err != nil {
		return 20
	}
	if inspect {
		if _, err := fmt.Fprintln(output, `{"mime":"text/html"}`); err != nil {
			return 1
		}
		return 0
	}
	if err := os.WriteFile("/work/document.html", safe, 0600); err != nil {
		return 1
	}
	cmd := exec.Command("/usr/bin/chromium", "--headless", "--disable-setuid-sandbox",
		"--disable-background-networking", "--disable-component-update", "--disable-sync", "--disable-extensions",
		"--disable-dev-shm-usage", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--password-store=basic", "--blink-settings=scriptEnabled=false", "--no-pdf-header-footer",
		"--user-data-dir=/work/chromium", "--print-to-pdf=/work/raw.pdf", "file:///work/document.html")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/work", "TMPDIR=/work", "XDG_RUNTIME_DIR=/work/runtime"}
	if err := os.Mkdir("/work/runtime", 0700); err != nil || cmd.Run() != nil {
		return 1
	}
	raw, err := os.Stat("/work/raw.pdf")
	if err != nil || raw.Size() < 5 || raw.Size() > htmlproto.MaxOutput {
		return 20
	}
	if exec.Command("qpdf", "--empty", "--pages", "/work/raw.pdf", "--", "/work/output.pdf").Run() != nil {
		return 20
	}
	file, err := os.Open("/work/output.pdf")
	if err != nil {
		return 1
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() < 5 || info.Size() > htmlproto.MaxOutput || pdfcheck.OutputPages(file.Name()) == 0 {
		return 20
	}
	if _, err := io.Copy(output, file); err != nil {
		return 1
	}
	return 0
}

func stylesheet(layout htmlproto.Layout) string {
	page, orientation := "A4", "portrait"
	if layout.Letter {
		page = "Letter"
	}
	if layout.Landscape {
		orientation = "landscape"
	}
	return fmt.Sprintf("@page { size: %s %s; margin: %dmm; }", page, orientation, layout.Margin)
}

func prependPolicy(data []byte, layout htmlproto.Layout) []byte {
	var result bytes.Buffer
	result.WriteString(`<!doctype html><meta charset="utf-8"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'">`)
	result.Write(data)
	result.WriteString("<style>" + strings.TrimSpace(stylesheet(layout)) + "</style>")
	return result.Bytes()
}
