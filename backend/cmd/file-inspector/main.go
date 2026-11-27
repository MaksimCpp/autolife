package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"autolife/backend/internal/pdfcheck"
	"autolife/backend/internal/sandbox"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(1)
	}
	mime, err := inspect(os.Args[1], os.Stdin)
	if err != nil {
		os.Exit(1)
	}
	if mime == "" {
		os.Exit(20)
	}
	if json.NewEncoder(os.Stdout).Encode(map[string]string{"mime": mime}) != nil {
		os.Exit(1)
	}
}

func inspect(profile string, src io.Reader) (string, error) {
	header := make([]byte, 512)
	n, err := io.ReadFull(src, header)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return "", err
	}
	header = header[:n]
	mime := http.DetectContentType(header)
	ext := map[string]string{"image/jpeg": "jpg", "image/png": "png", "image/webp": "webp", "application/pdf": "pdf"}[mime]
	if ext == "" || (profile == "pdf" && ext != "pdf") || (profile == "image" && ext == "pdf") {
		return "", nil
	}
	path := filepath.Join("/work", "input."+ext)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	defer os.Remove(path)
	count, err := io.Copy(f, io.LimitReader(io.MultiReader(bytes.NewReader(header), src), sandbox.MaxInputBytes+1))
	closeErr := f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if count > sandbox.MaxInputBytes {
		return "", nil
	}
	if profile == "image" && validImage(path) || profile == "pdf" && pdfcheck.Pages(path) > 0 {
		return mime, nil
	}
	return "", nil
}

func validImage(path string) bool {
	expected := map[string]string{".jpg": "jpegload", ".png": "pngload", ".webp": "webpload"}[filepath.Ext(path)]
	loader, err := exec.Command("vipsheader", "-f", "vips-loader", path).Output()
	if err != nil || strings.TrimSpace(string(loader)) != expected {
		return false
	}

	dimensions := [2]int64{}
	for i, field := range []string{"width", "height"} {
		out, err := exec.Command("vipsheader", "-f", field, path).Output()
		if err != nil {
			return false
		}
		value, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil || value < 1 || value > 40_000_000 {
			return false
		}
		dimensions[i] = value
	}
	if dimensions[0]*dimensions[1] > 40_000_000 {
		return false
	}
	// avg forces libvips to decode the entire raster sequentially.
	return exec.Command("vips", "avg", path+"[access=sequential,fail-on=warning]").Run() == nil
}
