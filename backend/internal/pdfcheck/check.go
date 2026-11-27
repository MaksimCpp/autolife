package pdfcheck

import (
	"bytes"
	"encoding/json"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

func Pages(path string) int { return pages(path, false) }

// OutputPages verifies the static PDF policy and rejects retained private metadata.
func OutputPages(path string) int { return pages(path, true) }

func pages(path string, output bool) int {
	if exec.Command("qpdf", "--check", path).Run() != nil {
		return 0
	}
	out, err := exec.Command("qpdf", "--show-npages", path).Output()
	if err != nil {
		return 0
	}
	pages, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pages < 1 || pages > 500 {
		return 0
	}
	cmd := exec.Command("qpdf", "--json", "--json-key=qpdf", "--json-key=encrypt", "--json-stream-data=none", path)
	pipe, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return 0
	}
	limited := &io.LimitedReader{R: pipe, N: 16 << 20}
	var doc struct {
		Encrypt struct{ Encrypted *bool }
		QPDF    []json.RawMessage
	}
	dec := json.NewDecoder(limited)
	err = dec.Decode(&doc)
	var extra any
	complete := err == nil && dec.Decode(&extra) == io.EOF && limited.N > 0
	_ = pipe.Close()
	if cmd.Wait() != nil || !complete || doc.Encrypt.Encrypted == nil || *doc.Encrypt.Encrypted || len(doc.QPDF) != 2 {
		return 0
	}
	tokens := json.NewDecoder(bytes.NewReader(doc.QPDF[1]))
	depth := 0
	for {
		token, err := tokens.Token()
		if err == io.EOF {
			if depth == 0 {
				return pages
			}
			return 0
		}
		if err != nil {
			return 0
		}
		switch x := token.(type) {
		case json.Delim:
			switch x {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			if depth > 64 {
				return 0
			}
		case string:
			if output && (x == "/Metadata" || x == "/PieceInfo") {
				return 0
			}
			switch x {
			case "/AA", "/A", "/OpenAction", "/JavaScript", "/JS", "/Launch", "/AcroForm", "/XFA", "/EmbeddedFiles", "/EmbeddedFile", "/Filespec", "/RichMedia", "/Movie", "/Sound", "/3D", "/Annots", "/F", "/FFilter", "/FDecodeParms", "/Action", "/URI", "/GoToR", "/GoToE", "/SubmitForm", "/ImportData", "/Rendition", "/Collection", "/EF", "/AF":
				return 0
			}
		}
	}
}
