package main

import (
	"strings"
	"testing"

	"autolife/backend/internal/htmlproto"
)

func TestStaticHTML(t *testing.T) {
	layout := htmlproto.Layout{Letter: true, Landscape: true, Margin: 15}
	input := `<!doctype html><html><head><style>@media print { .test {color: red} }</style></head><body><h1 class="test">Привет &amp; мир</h1></body></html>`
	data, err := sanitize([]byte(input), layout)
	if err != nil || !strings.Contains(string(data), "Letter landscape") || !strings.Contains(string(data), "Привет &amp; мир") ||
		!strings.Contains(string(data), "default-src 'none'") {
		t.Fatalf("safe print document: %s %v", data, err)
	}
	for _, html := range []string{
		`<script>1</script>`, `<h1 onclick="execute()">test</h1>`,
		`<iframe src="file:///etc/hosts"></iframe>`, `<img src="https://example.invalid/a.png">`,
		`<img src="data:image/svg+xml;base64,AAAA">`,
		`<style>@import "style.css";</style>`, `<p style="background: u\72l(local.jpg)">x</p>`,
		`<style>p { background: image-set("local.png" 1x) }</style>`,
		`<meta http-equiv="refresh" content="1;url=/etc/hosts">`,
	} {
		if _, err := sanitize([]byte(html), layout); err == nil {
			t.Errorf("accepted active HTML: %s", html)
		}
	}
}
