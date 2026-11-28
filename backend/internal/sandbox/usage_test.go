package sandbox

import (
	"strings"
	"testing"
)

func TestDecodeUsage(t *testing.T) {
	samples := `{"read":"0001-01-01T00:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":999}}}
{"read":"2026-11-27T00:00:00Z","cpu_stats":{"cpu_usage":{"total_usage":12}},"memory_stats":{"usage":800}}
{"read":"2026-11-27T00:00:01Z","cpu_stats":{"cpu_usage":{"total_usage":24}},"memory_stats":{"usage":600,"max_usage":900}}`
	if got := decodeUsage(strings.NewReader(samples)); !got.Sampled || got.CPUNanoseconds != 24 || got.MemoryBytes != 900 {
		t.Fatalf("wrong cumulative counters: %+v", got)
	}
	if got := decodeUsage(strings.NewReader(`{"read":"2026-11-27T00:00:01Z"}`)); got.Sampled {
		t.Fatalf("missing sample reported as zero: %+v", got)
	}
}
