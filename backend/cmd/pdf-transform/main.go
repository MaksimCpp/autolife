package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"autolife/backend/internal/pdfcheck"
	"autolife/backend/internal/pdfproto"
)

type input struct {
	path  string
	pages int
}

func main() {
	if len(os.Args) != 1 {
		os.Exit(1)
	}
	q, err := pdfproto.ReadRequest(os.Stdin)
	if err != nil {
		os.Exit(20)
	}
	inputs, err := readInputs(os.Stdin, q)
	if err != nil {
		os.Exit(20)
	}
	outputs, err := perform(q, inputs)
	if err != nil {
		os.Exit(20)
	}
	if err := writeResults(os.Stdout, outputs); err != nil {
		os.Exit(20)
	}
}

func readInputs(r io.Reader, q pdfproto.Request) ([]input, error) {
	result := make([]input, 0, len(q.Sizes))
	pages := 0
	for i, n := range q.Sizes {
		path := filepath.Join("/work", fmt.Sprintf("input-%02d", i+1))
		if q.Operation != "create" {
			path += ".pdf"
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		_, copyErr := io.CopyN(f, r, n)
		err = f.Close()
		if copyErr != nil || err != nil {
			return nil, pdfproto.ErrInvalid
		}
		count := 1
		if q.Operation != "create" {
			h, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			sig := make([]byte, 5)
			_, err = io.ReadFull(h, sig)
			h.Close()
			if err != nil || string(sig) != "%PDF-" {
				return nil, pdfproto.ErrInvalid
			}
			count = pdfcheck.Pages(path)
			if count == 0 {
				return nil, pdfproto.ErrInvalid
			}
		}
		pages += count
		if pages > pdfproto.MaxPages {
			return nil, pdfproto.ErrInvalid
		}
		result = append(result, input{path, count})
	}
	var trailing [1]byte
	if _, err := io.ReadFull(r, trailing[:]); err != io.EOF {
		return nil, pdfproto.ErrInvalid
	}
	return result, nil
}

func perform(q pdfproto.Request, inputs []input) ([]string, error) {
	if q.Operation == "create" {
		for i, in := range inputs {
			path, err := makePage(q, in.path, i)
			if err != nil {
				return nil, err
			}
			inputs[i] = input{path, 1}
			if err := os.Remove(in.path); err != nil {
				return nil, err
			}
		}
	}
	if q.Operation == "merge" || q.Operation == "create" {
		out := "/work/result-001.pdf"
		if err := selectPages(inputs, nil, out); err != nil {
			return nil, err
		}
		return []string{out}, nil
	}
	groups, err := pdfproto.Ranges(q.Ranges, inputs[0].pages)
	if err != nil {
		return nil, err
	}
	if len(groups) == 0 {
		for i := 1; i <= inputs[0].pages; i++ {
			groups = append(groups, []int{i})
		}
	}
	if q.Operation == "extract" {
		combined := []int{}
		for _, pages := range groups {
			combined = append(combined, pages...)
		}
		groups = [][]int{combined}
	}
	result := make([]string, 0, len(groups))
	var total int64
	for i, pages := range groups {
		path := fmt.Sprintf("/work/result-%03d.pdf", i+1)
		if err := selectPages(inputs, pages, path); err != nil {
			return nil, err
		}
		size, err := pdfSize(path)
		if err != nil {
			return nil, err
		}
		total += size
		if total > pdfproto.MaxBytes {
			return nil, pdfproto.ErrInvalid
		}
		result = append(result, path)
	}
	return result, nil
}

func selectPages(inputs []input, pages []int, out string) error {
	args := []string{"--empty", "--remove-info", "--remove-metadata", "--pages"}
	total := 0
	for _, in := range inputs {
		args = append(args, "--file="+in.path)
		total += in.pages
	}
	if len(pages) > 0 {
		values := make([]string, len(pages))
		for i, n := range pages {
			values[i] = strconv.Itoa(n)
		}
		args = append(args, "--range="+strings.Join(values, ","))
		total = len(pages)
	}
	args = append(args, "--", out)
	if err := exec.Command("qpdf", args...).Run(); err != nil {
		return err
	}
	if pdfcheck.OutputPages(out) != total {
		return pdfproto.ErrInvalid
	}
	return nil
}

func makePage(q pdfproto.Request, path string, index int) (string, error) {
	// Use the existing raster profile binary. Each invocation has its own fixed,
	// writable native working files, removed before the next item starts.
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()
	normalized := fmt.Sprintf("/work/raster-%02d.png", index+1)
	f, err := os.OpenFile(normalized, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	header := make([]byte, 18)
	copy(header, "ALI1")
	header[4], header[5], header[6] = 2, 85, 1
	cmd := exec.Command("/usr/local/bin/autolife-image")
	cmd.Stdin = io.MultiReader(bytes.NewReader(header), in)
	cmd.Stdout = f
	runErr := cmd.Run()
	closeErr := f.Close()
	for _, temp := range []string{"/work/input", "/work/output"} {
		if err := os.Remove(temp); err != nil && !os.IsNotExist(err) {
			return "", err
		}
	}
	if runErr != nil || closeErr != nil {
		return "", pdfproto.ErrInvalid
	}
	png, err := os.Open(normalized)
	if err != nil {
		return "", err
	}
	var imageHeader [24]byte
	_, err = io.ReadFull(png, imageHeader[:])
	png.Close()
	if err != nil {
		return "", err
	}
	width, height := binary.BigEndian.Uint32(imageHeader[16:20]), binary.BigEndian.Uint32(imageHeader[20:24])
	if width == 0 || height == 0 {
		return "", pdfproto.ErrInvalid
	}
	landscape := q.Orientation == "landscape" || q.Orientation == "auto" && width > height
	w, h := float64(width)*.75, float64(height)*.75
	switch q.PageSize {
	case "A4":
		w, h = 595.2756, 841.8898
	case "Letter":
		w, h = 612, 792
	}
	if (w > h) != landscape {
		w, h = h, w
	}
	fit := "into"
	if q.Fit == "shrink" {
		fit = "shrink"
	}
	out := fmt.Sprintf("/work/page-%02d.pdf", index+1)
	args := []string{"-m", "img2pdf", "--engine", "internal", "--nodate", "--rotation", "none",
		"--pagesize", fmt.Sprintf("%.4fptx%.4fpt", max(w, 3), max(h, 3)), "--fit", fit, "--output", out, normalized}
	if err := exec.Command("python3", args...).Run(); err != nil {
		return "", err
	}
	if _, err := pdfSize(out); err != nil {
		return "", err
	}
	if err := os.Remove(normalized); err != nil {
		return "", err
	}
	return out, nil
}

func pdfSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	if info.Size() < 5 || info.Size() > pdfproto.MaxFileBytes {
		return 0, pdfproto.ErrInvalid
	}
	return info.Size(), nil
}

func writeResults(w io.Writer, paths []string) error {
	sizes := make([]int64, len(paths))
	var total int64
	for i, path := range paths {
		n, err := pdfSize(path)
		if err != nil {
			return err
		}
		sizes[i] = n
		total += n
	}
	if total > pdfproto.MaxBytes || len(paths) == 0 || len(paths) > pdfproto.MaxPages {
		return pdfproto.ErrInvalid
	}
	if err := binary.Write(w, binary.BigEndian, uint32(len(paths))); err != nil {
		return err
	}
	for i, path := range paths {
		if err := binary.Write(w, binary.BigEndian, sizes[i]); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.CopyN(w, f, sizes[i])
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
