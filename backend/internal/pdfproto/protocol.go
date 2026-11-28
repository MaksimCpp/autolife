package pdfproto

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

const (
	MaxFiles           = 50
	MaxPages           = 500
	MaxBytes     int64 = 250_000_000
	MaxFileBytes int64 = 100_000_000
	MaxHeader          = 16384
)
const MaxRequest = MaxBytes + MaxHeader + 4
const MaxResponse = MaxBytes + 8 + MaxPages*12

type SourcePage struct {
	Document uint16 `json:"input_index"`
	Page     uint16 `json:"page"`
}

var ErrInvalid = errors.New("invalid PDF operation or frame")

type Request struct {
	Operation      string   `json:"operation"`
	Ranges         []string `json:"ranges,omitempty"`
	PageSize       string   `json:"page_size,omitempty"`
	Orientation    string   `json:"orientation,omitempty"`
	Fit            string   `json:"fit,omitempty"`
	Format         string   `json:"format,omitempty"`
	DPI            int      `json:"dpi,omitempty"`
	MaxRasterPages int      `json:"max_pages,omitempty"`
	Quality        int      `json:"quality,omitempty"`
	Background     string   `json:"background,omitempty"`
	Alpha          bool     `json:"alpha,omitempty"`
	Sizes          []int64  `json:"sizes"`
}

func (q Request) Validate() error {
	if len(q.Sizes) < 1 || len(q.Sizes) > MaxFiles {
		return ErrInvalid
	}
	var total int64
	for _, n := range q.Sizes {
		if n < 1 || n > MaxFileBytes {
			return ErrInvalid
		}
		total += n
	}
	if total > MaxBytes {
		return ErrInvalid
	}
	if q.Operation == "rasterize" {
		if q.PageSize != "" || q.Orientation != "" || q.Fit != "" || q.DPI < 72 || q.DPI > 300 || q.MaxRasterPages < 1 || q.MaxRasterPages > MaxPages ||
			(q.Format != "png" && q.Format != "jpeg") || q.Quality < 1 || q.Quality > 100 ||
			q.Alpha && q.Format != "png" || len(q.Background) != 7 || q.Background[0] != '#' {
			return ErrInvalid
		}
		if _, err := hex.DecodeString(q.Background[1:]); err != nil {
			return ErrInvalid
		}
		_, err := Ranges(q.Ranges, MaxPages)
		return err
	}
	if q.Format != "" || q.DPI != 0 || q.MaxRasterPages != 0 || q.Quality != 0 || q.Background != "" || q.Alpha {
		return ErrInvalid
	}
	if q.Operation == "create" {
		if len(q.Ranges) != 0 || !(q.PageSize == "auto" || q.PageSize == "A4" || q.PageSize == "Letter") ||
			!(q.Orientation == "auto" || q.Orientation == "portrait" || q.Orientation == "landscape") || !(q.Fit == "contain" || q.Fit == "shrink") {
			return ErrInvalid
		}
		return nil
	}
	if q.PageSize != "" || q.Orientation != "" || q.Fit != "" {
		return ErrInvalid
	}
	switch q.Operation {
	case "merge":
		if len(q.Ranges) != 0 {
			return ErrInvalid
		}
	case "split", "extract":
		if len(q.Sizes) != 1 || q.Operation == "extract" && len(q.Ranges) == 0 {
			return ErrInvalid
		}
		if _, err := Ranges(q.Ranges, MaxPages); err != nil {
			return err
		}
	default:
		return ErrInvalid
	}
	return nil
}

func Ranges(groups []string, maxPage int) ([][]int, error) {
	if maxPage < 1 || maxPage > MaxPages || len(groups) > MaxPages {
		return nil, ErrInvalid
	}
	seen := make([]bool, maxPage+1)
	result := make([][]int, 0, len(groups))
	for _, group := range groups {
		a, b, rangeSet := strings.Cut(group, "-")
		first, err := page(a)
		if err != nil {
			return nil, err
		}
		last := first
		if rangeSet {
			last, err = page(b)
		}
		if err != nil || last < first || last > maxPage {
			return nil, ErrInvalid
		}
		leaves := make([]int, 0, last-first+1)
		for n := first; n <= last; n++ {
			if seen[n] {
				return nil, ErrInvalid
			}
			seen[n] = true
			leaves = append(leaves, n)
		}
		result = append(result, leaves)
	}
	return result, nil
}
func page(raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > MaxPages || strconv.Itoa(n) != raw {
		return 0, ErrInvalid
	}
	return n, nil
}

func Header(q Request) ([]byte, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(q)
	if err != nil || len(raw) > MaxHeader {
		return nil, ErrInvalid
	}
	header := binary.BigEndian.AppendUint32(nil, uint32(len(raw)))
	return append(header, raw...), nil
}

func ReadRequest(r io.Reader) (Request, error) {
	var q Request
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil || n == 0 || n > MaxHeader {
		return q, ErrInvalid
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return q, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil || dec.Decode(new(any)) != io.EOF {
		return q, ErrInvalid
	}
	return q, q.Validate()
}
