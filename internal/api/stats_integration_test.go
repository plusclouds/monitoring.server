package api_test

import (
	"context"
	"math"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/internal/metrics"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func approx(got any, want float64) bool {
	v, ok := got.(float64)
	return ok && math.Abs(v-want) < 1e-6
}

// Percentiles, standard deviations and moving averages come from raw
// samples: exact, per bucket or over the whole window.
func TestMetricStatistics(t *testing.T) {
	e := setup(t, false)
	k := e.tenantKey("acc")
	ctx := context.Background()
	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "web", "type": "web"}), 201))
	chk := e.id(e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{"name": "home", "plugin": "http"}), 201))
	tenant := uuid.MustParse(e.must(e.do("GET", "/v1/tenant", k, nil), 200).body["id"].(string))
	w, err := metrics.NewWriter(metrics.WriterOptions{System: db.System})
	if err != nil {
		t.Fatal(err)
	}
	// total_ms = 1, 2, ..., 100, one a minute, ending a minute ago.
	now := time.Now().UTC().Truncate(time.Minute)
	base := now.Add(-100 * time.Minute)
	nan := math.NaN()
	for i := range 100 {
		w.Add(runner.Result{TenantID: tenant, DeviceID: uuid.MustParse(dev), CheckID: uuid.MustParse(chk), Plugin: "http",
			Result: plugin.Result{Time: base.Add(time.Duration(i) * time.Minute),
				Metrics: []float64{nan, nan, nan, nan, float64(i + 1), 200, 1024}}})
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	q := func(path string, from, to time.Time, extra string) string {
		return path + "?from=" + url.QueryEscape(from.Format(time.RFC3339)) + "&to=" + url.QueryEscape(to.Format(time.RFC3339)) +
			"&check_id=" + chk + "&name=total_ms" + extra
	}

	s := e.must(e.do("GET", q("/v1/metrics/summary", base, now, "&window=7&percentile=p90&percentile=p99.9"), k, nil), 200)
	if s.body["exact"] != true || s.body["resolution"] != "raw" {
		t.Fatalf("summary: %s", s.raw)
	}
	m := s.body["series"].([]any)[0].(map[string]any)
	for name, want := range map[string]float64{"count": 100, "min": 1, "max": 100, "avg": 50.5, "stddev": 29.011491975882016,
		"p50": 50.5, "p95": 95.05, "p99": 99.01, "last": 100} {
		if !approx(m[name], want) {
			t.Errorf("summary %s = %v, want %v", name, m[name], want)
		}
	}
	pct := m["percentiles"].(map[string]any)
	if !approx(pct["p90"], 90.1) || !approx(pct["p99.9"], 99.901) {
		t.Errorf("percentiles: %v", pct)
	}
	mv := m["moving"].(map[string]any)
	if mv["window"] != float64(7) || mv["count"] != float64(7) || !approx(mv["avg"], 97) || !approx(mv["stddev"], math.Sqrt(28.0/6)) {
		t.Errorf("moving: %v", mv)
	}

	// Per bucket: 10-minute buckets of ten consecutive values.
	r := e.must(e.do("GET", q("/v1/metrics/query", base, now, "&step=600&agg=p95"), k, nil), 200)
	if r.body["resolution"] != "raw" {
		t.Fatalf("p95 query: %s", r.raw)
	}
	pts := r.body["series"].([]any)[0].(map[string]any)["points"].([]any)
	var total int
	for _, p := range pts {
		v := p.(map[string]any)["v"].(float64)
		if v < 1 || v > 100 {
			t.Errorf("p95 point %v", v)
		}
		total++
	}
	if total < 10 || total > 11 {
		t.Errorf("p95 buckets: %d", total)
	}
	r = e.must(e.do("GET", q("/v1/metrics/query", base, now, "&step=60&agg=stddev"), k, nil), 200)
	if pts := r.body["series"].([]any)[0].(map[string]any)["points"].([]any); len(pts) != 0 {
		t.Errorf("one sample per bucket has no deviation: %v", pts)
	}
	r = e.must(e.do("GET", q("/v1/metrics/query", base, now, "&step=60&moving_window=7"), k, nil), 200)
	pts = r.body["series"].([]any)[0].(map[string]any)["points"].([]any)
	if last := pts[len(pts)-1].(map[string]any)["v"]; !approx(last, 97) {
		t.Errorf("moving average of the last 7 points = %v, want 97", last)
	}
	if first := pts[0].(map[string]any)["v"]; !approx(first, 1) {
		t.Errorf("first moving point = %v, want 1", first)
	}

	e.must(e.do("GET", q("/v1/metrics/query", base, now, "&agg=p100"), k, nil), 400)
	e.must(e.do("GET", q("/v1/metrics/query", base, now, "&agg=p0"), k, nil), 422)
	e.must(e.do("GET", q("/v1/metrics/query", base, now, "&agg=p95&resolution=5m"), k, nil), 422)
	e.must(e.do("GET", q("/v1/metrics/summary", base, now, "&percentile=x"), k, nil), 400)

	// Raw samples kept for a day: older windows are refused, not approximated.
	if _, err := db.System.Exec(ctx, `INSERT INTO retention_policies (class_id, level, keep_days)
		SELECT id, 'raw', 1 FROM retention_classes WHERE name = 'standard'`); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-72 * time.Hour)
	if r := e.must(e.do("GET", q("/v1/metrics/summary", old, now, ""), k, nil), 422); r.body["errors"] == nil && r.body["detail"] == nil {
		t.Errorf("too old: %s", r.raw)
	}
	e.must(e.do("GET", q("/v1/metrics/query", old, now, "&agg=p99&step=3600"), k, nil), 422)
	e.must(e.do("GET", q("/v1/metrics/query", old, now, "&agg=avg&step=3600"), k, nil), 200)
}
