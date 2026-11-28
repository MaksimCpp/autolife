package svgcheck

import (
	"strings"
	"testing"
)

func TestStaticSubset(t *testing.T) {
	root := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 12 4">`
	for _, value := range []string{
		`<rect width="4" height="4" fill="#123456" transform="scale(1 2)"/>`,
		`<defs><linearGradient id="a"><stop offset="0%" stop-color="rgb(0, 0, 0)"/></linearGradient></defs><path d="M 1 1 Z" fill="url(#a)"/>`,
	} {
		if err := Validate(strings.NewReader(root + value + "</svg>")); err != nil {
			t.Errorf("%s: %v", value, err)
		}
	}
	for _, value := range []string{
		`<script/>`, `<style/>`, `<foreignObject/>`, `<image href="file:///etc/hosts"/>`,
		`<use href="#recursive"/>`, `<rect onload="run()"/>`, `<rect fill="url(https://example.invalid)"/>`,
		`<rect style="fill: red"/>`, `<rect fill="u&#114;l(file:///etc/hosts)"/>`, `<rect width="1" width="2"/>`,
	} {
		if Validate(strings.NewReader(root+value+"</svg>")) == nil {
			t.Errorf("unexpectedly accepted %s", value)
		}
	}
	if Validate(strings.NewReader(`<?xml-stylesheet href="foo"?>`+root+"</svg>")) == nil {
		t.Error("external stylesheet accepted")
	}
}
