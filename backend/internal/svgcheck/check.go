package svgcheck

import (
	"encoding/xml"
	"errors"
	"io"
	"regexp"
	"strings"
)

const MIME = "image/svg+xml"
const MaxBytes = 2 << 20
const namespace = "http://www.w3.org/2000/svg"

var invalid = errors.New("unsupported static SVG")

var elements = names("svg g defs title desc rect circle ellipse line polyline polygon path linearGradient radialGradient stop text tspan")
var attributes = names("id version width height viewBox preserveAspectRatio x y x1 y1 x2 y2 cx cy r rx ry fx fy fr d points transform fill fill-opacity fill-rule stroke stroke-opacity stroke-width stroke-linecap stroke-linejoin stroke-miterlimit stroke-dasharray stroke-dashoffset opacity color gradientUnits gradientTransform spreadMethod offset stop-color stop-opacity font-family font-size font-weight font-style text-anchor dominant-baseline dx dy rotate")
var fragment = regexp.MustCompile(`^url\(#[A-Za-z_][A-Za-z0-9_.-]{0,127}\)$`)

func names(names string) map[string]bool {
	result := make(map[string]bool)
	for _, name := range strings.Fields(names) {
		result[name] = true
	}
	return result
}

// Validate accepts a static subset with no links, imports, stylesheets, embedding
// or executable elements. Both inspection and rendering apply this check.
func Validate(src io.Reader) error {
	limited := &io.LimitedReader{R: src, N: MaxBytes + 1}
	decoder := xml.NewDecoder(limited)
	depth, count := 0, 0
	closed := false
	for {
		token, err := decoder.Token()
		if err == io.EOF && closed && limited.N > 0 {
			return nil
		}
		if err != nil || limited.N == 0 {
			return invalid
		}
		switch t := token.(type) {
		case xml.StartElement:
			if closed || t.Name.Space != namespace || !elements[t.Name.Local] ||
				(depth == 0) != (t.Name.Local == "svg") || depth >= 32 || count >= 10000 || len(t.Attr) > 48 {
				return invalid
			}
			seen := make(map[xml.Name]bool, len(t.Attr))
			for _, attr := range t.Attr {
				if seen[attr.Name] || !safeAttribute(attr) {
					return invalid
				}
				seen[attr.Name] = true
			}
			depth++
			count++
		case xml.EndElement:
			depth--
			closed = depth == 0
		case xml.Directive:
			return invalid
		case xml.ProcInst:
			if depth != 0 || closed || t.Target != "xml" || decoder.InputOffset() > 256 {
				return invalid
			}
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(t)) != "" {
				return invalid
			}
		}
	}
}

func safeAttribute(attr xml.Attr) bool {
	if attr.Name.Space == "xmlns" || attr.Name.Local == "xmlns" && attr.Name.Space == "" {
		return attr.Value == namespace
	}
	if attr.Name.Space != "" || !attributes[attr.Name.Local] || len(attr.Value) > 65536 {
		return false
	}
	// Prevent CSS escapes and external resources even in presentation attributes.
	value := strings.TrimSpace(attr.Value)
	if (attr.Name.Local == "fill" || attr.Name.Local == "stroke") && fragment.MatchString(value) {
		return true
	}
	return !strings.ContainsAny(value, "\\()/@{}:;") ||
		(attr.Name.Local == "transform" || attr.Name.Local == "gradientTransform" ||
			attr.Name.Local == "fill" || attr.Name.Local == "stroke" || attr.Name.Local == "color" || attr.Name.Local == "stop-color") &&
			safeFunction.MatchString(value)
}

var safeFunction = regexp.MustCompile(`^(?:(?:matrix|translate|scale|rotate|skewX|skewY|rgb|rgba)\([0-9eE+.,% \t-]+\)[ \t,]*)+$`)
