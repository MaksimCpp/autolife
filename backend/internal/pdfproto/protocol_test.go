package pdfproto

import (
	"bytes"
	"reflect"
	"testing"
)

func TestRanges(t *testing.T) {
	got, err := Ranges([]string{"4", "1-2"}, 4)
	if err != nil || !reflect.DeepEqual(got, [][]int{{4}, {1, 2}}) {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range [][]string{{"01"}, {"0"}, {"1-3", "2"}, {"3-2"}, {"5"}, {"-1"}, {"1--2"}, {"--file=foo"}, {"1,2"}} {
		if _, err := Ranges(bad, 4); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}
func TestHeader(t *testing.T) {
	q := Request{Operation: "merge", Sizes: []int64{100_000_000, 100_000_000, 50_000_000}}
	raw, err := Header(q)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := ReadRequest(bytes.NewReader(raw))
	if err != nil || !reflect.DeepEqual(actual, q) {
		t.Fatalf("%+v %v", actual, err)
	}
	q.Sizes[2]++
	if _, err := Header(q); err == nil {
		t.Fatal("accepted aggregate overflow")
	}
	if _, err := ReadRequest(bytes.NewReader(raw[:6])); err == nil {
		t.Fatal("accepted truncated header")
	}
	if _, err := Header(Request{Operation: "extract", Sizes: []int64{50}}); err == nil {
		t.Fatal("accepted missing selection")
	}
}
