package plugin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"runtime/debug"
	"sync"
	"time"
)

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

// SafeRun runs a check with its timeout in ctx and always returns a result:
// a panic, an error or a plugin that ignores cancellation becomes UNKNOWN
// with a clear output, and never stalls the caller (F03).
func SafeRun(ctx context.Context, c Check, t Target) Result {
	start := time.Now()
	type outcome struct {
		r   Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				done <- outcome{err: fmt.Errorf("plugin panicked: %v\n%s", v, debug.Stack())}
			}
		}()
		r, err := c.Run(ctx, t)
		done <- outcome{r, err}
	}()

	var o outcome
	select {
	case o = <-done:
	case <-ctx.Done():
		// Give a well-behaved plugin a moment to return its own timeout result.
		select {
		case o = <-done:
		case <-time.After(100 * time.Millisecond):
			o.err = fmt.Errorf("plugin did not stop at its timeout (%w)", ctx.Err())
		}
	}

	r := o.r
	if o.err != nil {
		r = Result{Status: Unknown, Output: o.err.Error()}
	}
	return normalize(r, c.Manifest(), start)
}

func normalize(r Result, m Manifest, start time.Time) Result {
	if r.Time.IsZero() {
		r.Time = start.UTC()
	}
	if r.Duration == 0 {
		r.Duration = time.Since(start)
	}
	if len(r.Output) > MaxOutput {
		r.Output = r.Output[:MaxOutput]
	}
	if len(r.Metrics) != len(m.Metrics) {
		fixed := make([]float64, len(m.Metrics))
		for i := range fixed {
			fixed[i] = math.NaN()
			if i < len(r.Metrics) {
				fixed[i] = r.Metrics[i]
			}
		}
		r.Metrics = fixed
	}
	return r
}

// NaNs returns n NaN values, the "not collected" marker for metric slots.
func NaNs(n int) []float64 {
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	return out
}

// MemState is an in-memory StateStore, used by tests and as the runner's
// default until state is persisted.
type MemState struct {
	mu   sync.Mutex
	data map[string][]byte
	size int
}

// MaxStateSize is the StateStore limit per check.
const MaxStateSize = 64 << 10

func (m *MemState) Get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	return v, ok
}

func (m *MemState) Set(key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	size := m.size - len(m.data[key]) + len(value)
	if size > MaxStateSize {
		return fmt.Errorf("state store full: %d of %d bytes", size, MaxStateSize)
	}
	m.data[key] = append([]byte(nil), value...)
	m.size = size
	return nil
}
