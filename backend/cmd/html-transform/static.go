package main

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"autolife/backend/internal/htmlproto"
	"golang.org/x/net/html"
)

var rejected = errors.New("unsupported static HTML")
var safeTags = names("html head body title style div span main section article header footer nav aside h1 h2 h3 h4 h5 h6 p br hr pre code blockquote em strong b i u s small sub sup table thead tbody tfoot tr th td caption col colgroup ul ol li dl dt dd figure figcaption img")
var plainAttrs = names("class id title lang dir width height colspan rowspan scope alt")
var illegalCSS = regexp.MustCompile(`(?i)(?:url|image(?:-set)?|-webkit-image-set|src|paint|attr|var|env|element|expression)[ \t\r\n]*\(`)

func names(value string) map[string]bool {
	result := map[string]bool{}
	for _, name := range strings.Fields(value) {
		result[name] = true
	}
	return result
}

func safeCSS(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "://") || strings.Contains(lower, "data:") || strings.Contains(lower, "file:") || len(value) > 64<<10 || strings.ContainsAny(value, "\\<>\x00\x0c") || illegalCSS.MatchString(value) {
		return false
	}
	for _, at := range strings.Split(lower, "@")[1:] {
		if !(strings.HasPrefix(at, "media ") || strings.HasPrefix(at, "page ") || strings.HasPrefix(at, "page{")) {
			return false
		}
	}
	return true
}

func rasterData(value string) (int, bool) {
	mime, data, ok := strings.Cut(value, ",")
	if !ok || (mime != "data:image/png;base64" && mime != "data:image/jpeg;base64") || len(data) > 2<<20 {
		return 0, false
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(data)
	if err != nil {
		return 0, false
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	pixels := int64(cfg.Width) * int64(cfg.Height)
	if err != nil || "data:image/"+format+";base64" != mime || pixels < 1 || pixels > 40_000_000 {
		return 0, false
	}
	_, _, err = image.Decode(bytes.NewReader(raw))
	return int(pixels), err == nil
}

func sanitize(data []byte, layout htmlproto.Layout) ([]byte, error) {
	if len(data) == 0 || len(data) > htmlproto.MaxInput || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return nil, rejected
	}
	reader := html.NewTokenizer(bytes.NewReader(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
	reader.SetMaxBuf(htmlproto.MaxInput)
	var content bytes.Buffer
	count, pixels := 0, 0
	inStyle := false
	found := false
	for kind := reader.Next(); kind != html.ErrorToken; kind = reader.Next() {
		count++
		if count > 40000 {
			return nil, rejected
		}
		token := reader.Token()
		switch kind {
		case html.DoctypeToken, html.CommentToken:
			continue
		case html.TextToken:
			if inStyle {
				if !safeCSS(token.Data) {
					return nil, rejected
				}
				content.WriteString(token.Data)
			} else {
				content.WriteString(html.EscapeString(token.Data))
			}
		case html.StartTagToken, html.SelfClosingTagToken, html.EndTagToken:
			if token.Data == "meta" && len(token.Attr) == 1 &&
				token.Attr[0].Key == "charset" && strings.EqualFold(token.Attr[0].Val, "utf-8") {
				continue
			}
			if !safeTags[token.Data] || (inStyle && (kind != html.EndTagToken || token.Data != "style")) {
				return nil, rejected
			}
			found = true
			inStyle = token.Data == "style" && kind == html.StartTagToken
			seen := map[string]bool{}
			for _, attr := range token.Attr {
				if seen[attr.Key] || attr.Namespace != "" {
					return nil, rejected
				}
				seen[attr.Key] = true
				switch {
				case attr.Key == "src" && token.Data == "img":
					size, ok := rasterData(attr.Val)
					pixels += size
					if !ok || pixels > 120_000_000 {
						return nil, rejected
					}
				case attr.Key == "style":
					if !safeCSS(attr.Val) {
						return nil, rejected
					}
				case plainAttrs[attr.Key] && len(attr.Val) <= 4096:
				default:
					return nil, rejected
				}
			}
			if token.Data == "img" && kind != html.EndTagToken && !seen["src"] {
				return nil, rejected
			}
			content.WriteString(token.String())
		}
	}
	if !found || inStyle || reader.Err() != io.EOF {
		return nil, rejected
	}
	return prependPolicy(content.Bytes(), layout), nil
}
