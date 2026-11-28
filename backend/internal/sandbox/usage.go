package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const CPUHeader = "X-Autolife-Cpu-Ns"
const MemoryHeader = "X-Autolife-Memory-Bytes"

type Usage struct {
	CPUNanoseconds, MemoryBytes uint64
	Sampled                     bool
}

type usageKey struct{}

func WithUsage(ctx context.Context, usage *Usage) context.Context {
	return context.WithValue(ctx, usageKey{}, usage)
}

type sampler func(context.Context, string, *Usage) func()

func statsSampler(socket string) sampler {
	dialer := net.Dialer{Timeout: time.Second}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return func(ctx context.Context, name string, usage *Usage) func() {
		ctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+name+"/stats?stream=true", nil)
			if err != nil {
				return
			}
			response, err := client.Do(request)
			if err == nil {
				defer response.Body.Close()
				if response.StatusCode == http.StatusOK {
					*usage = decodeUsage(response.Body)
				}
			}
		}()
		return sync.OnceFunc(func() { cancel(); <-done })
	}
}

func decodeUsage(src io.Reader) Usage {
	decoder := json.NewDecoder(io.LimitReader(src, 4<<20))
	var peak Usage
	for {
		var message struct {
			Read time.Time `json:"read"`
			CPU  struct {
				Usage struct {
					Total uint64 `json:"total_usage"`
				} `json:"cpu_usage"`
			} `json:"cpu_stats"`
			Memory struct {
				Usage    uint64 `json:"usage"`
				MaxUsage uint64 `json:"max_usage"`
			} `json:"memory_stats"`
		}
		if decoder.Decode(&message) != nil {
			return peak
		}
		if !message.Read.IsZero() && message.Memory.Usage > 0 {
			peak.Sampled = true
			peak.CPUNanoseconds = max(peak.CPUNanoseconds, message.CPU.Usage.Total)
			peak.MemoryBytes = max(peak.MemoryBytes, message.Memory.Usage, message.Memory.MaxUsage)
		}
	}
}
