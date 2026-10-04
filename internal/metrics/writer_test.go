package metrics

import (
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func TestChooseClass(t *testing.T) {
	for _, c := range []struct {
		want    string
		allowed []string
		got     string
	}{
		{"high-frequency", []string{"standard", "high-frequency"}, "high-frequency"},
		{"high-frequency", []string{"standard"}, "standard"},
		{"capacity", nil, "standard"},
		{"standard", []string{"standard-1y"}, "standard-1y"},
		{"", []string{"standard"}, "standard"},
	} {
		if got := chooseClass(c.want, c.allowed); got != c.got {
			t.Errorf("chooseClass(%q, %v) = %q, want %q", c.want, c.allowed, got, c.got)
		}
	}
}

func TestLayoutVersion(t *testing.T) {
	defs := []plugin.MetricDef{{Name: "a", Unit: "ms"}, {Name: "b", Unit: "ms"}}
	v := layoutVersion("standard", defs, []int{0, 1})
	if v != layoutVersion("standard", defs, []int{0, 1}) {
		t.Fatal("not deterministic")
	}
	renamed := []plugin.MetricDef{{Name: "a", Unit: "ms"}, {Name: "c", Unit: "ms"}}
	for name, other := range map[string]int32{
		"class":   layoutVersion("capacity", defs, []int{0, 1}),
		"slots":   layoutVersion("standard", defs, []int{0}),
		"renamed": layoutVersion("standard", renamed, []int{0, 1}),
	} {
		if other == v {
			t.Errorf("%s change keeps the layout version", name)
		}
	}
}

// Metrics never block the engine: a full queue drops and counts, and a
// failing database costs at most max_buffered of results.
func TestWriterNeverBlocks(t *testing.T) {
	w, err := NewWriter(WriterOptions{Config: config.MetricsWrite{BatchRows: 1, MaxBuffered: config.Duration(time.Minute)}})
	if err != nil {
		t.Fatal(err)
	}
	r := runner.Result{Plugin: "http", Result: plugin.Result{Metrics: []float64{1}}}
	for range 3 {
		w.Add(r)
	}
	w.Add(runner.Result{Result: plugin.Result{Metrics: []float64{math.NaN()}}}) // nothing to store
	if got := testutil.ToFloat64(w.dropped.WithLabelValues("queue_full")); got != 1 {
		t.Errorf("queue_full drops = %v, want 1", got)
	}

	w.drain()
	w.pending[0].added = time.Now().Add(-2 * time.Minute)
	w.expire()
	if len(w.pending) != 1 || testutil.ToFloat64(w.dropped.WithLabelValues("buffer_expired")) != 1 {
		t.Errorf("after expire: %d pending", len(w.pending))
	}
}
