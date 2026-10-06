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

// MaxObjects is the most objects one collector run keeps; MaxObjectOutput
// the longest object output.
const (
	MaxObjects      = 5000
	MaxObjectOutput = 1 << 10
	maxObjectKey    = 200
)

// SafeCollect runs a collector like SafeRun runs a check and returns its
// batch as a Result with Objects. A failed collection (error, panic,
// timeout, or a batch without objects) has nil Objects.
func SafeCollect(ctx context.Context, c Collector, t Target) Result {
	start := time.Now()
	type outcome struct {
		b   Batch
		inv Inventory
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if v := recover(); v != nil {
				done <- outcome{err: fmt.Errorf("plugin panicked: %v\n%s", v, debug.Stack())}
			}
		}()
		b, inv, err := c.Collect(ctx, t)
		done <- outcome{b, inv, err}
	}()
	var o outcome
	select {
	case o = <-done:
	case <-ctx.Done():
		select {
		case o = <-done:
		case <-time.After(100 * time.Millisecond):
			o.err = fmt.Errorf("plugin did not stop at its timeout (%w)", ctx.Err())
		}
	}
	m := c.Manifest()
	if o.err != nil {
		return normalize(Result{Status: Unknown, Output: o.err.Error()}, Manifest{}, start)
	}
	r := Result{Status: o.b.Status, Output: o.b.Output}
	if o.b.Objects != nil {
		r.Objects = normalizeObjects(o.b.Objects, len(m.Metrics))
		if o.inv.Children != nil || o.inv.Device != nil {
			inv := normalizeInventory(o.inv)
			r.Inventory = &inv
		}
		known := map[string]bool{}
		if r.Inventory != nil {
			for _, c := range r.Inventory.Children {
				known[c.Key] = true
			}
		}
		for i := range r.Objects {
			if !known[r.Objects[i].Device] {
				r.Objects[i].Device = "" // unknown child: the collector's own device
			}
		}
	}
	// A collector has no device-level metrics: its layout is per object.
	return normalize(r, Manifest{}, start)
}

// normalizeObjects drops objects without a key or with a duplicate key,
// caps their number and output, and aligns their metrics with the layout.
func normalizeObjects(in []Object, slots int) []Object {
	out := make([]Object, 0, min(len(in), MaxObjects))
	seen := make(map[string]bool, len(in))
	for _, o := range in {
		if o.Key == "" || len(o.Key) > maxObjectKey || seen[o.Key] || len(out) == MaxObjects {
			continue
		}
		seen[o.Key] = true
		if len(o.Output) > MaxObjectOutput {
			o.Output = o.Output[:MaxObjectOutput]
		}
		if o.Name == "" {
			o.Name = o.Key
		}
		if len(o.Metrics) != slots {
			fixed := NaNs(slots)
			copy(fixed, o.Metrics)
			o.Metrics = fixed
		}
		out = append(out, o)
	}
	return out
}

// MaxChildren is the most child devices one collection keeps.
const MaxChildren = 5000

// normalizeInventory drops children without a key, with a duplicate key
// or with a parent that is not in the list, and caps their number.
func normalizeInventory(in Inventory) Inventory {
	out := Inventory{Device: in.Device}
	if in.Children == nil {
		return out
	}
	keys := map[string]bool{}
	for _, c := range in.Children {
		if c.Key != "" && len(c.Key) <= maxObjectKey && !keys[c.Key] {
			keys[c.Key] = true
		}
	}
	seen := map[string]bool{}
	out.Children = make([]ChildDevice, 0, len(in.Children))
	for _, c := range in.Children {
		if c.Key == "" || len(c.Key) > maxObjectKey || seen[c.Key] || len(out.Children) == MaxChildren {
			continue
		}
		seen[c.Key] = true
		if c.ParentKey == c.Key || (c.ParentKey != "" && !keys[c.ParentKey]) {
			c.ParentKey = ""
		}
		if c.Name == "" {
			c.Name = c.Key
		}
		out.Children = append(out.Children, c)
	}
	return out
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
