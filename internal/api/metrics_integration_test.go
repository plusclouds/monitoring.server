package api_test

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/metrics"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func newMaintainer(t *testing.T) *metrics.Maintainer {
	t.Helper()
	m, err := metrics.NewMaintainer(metrics.MaintainerOptions{System: db.System, Config: config.Default().Metrics})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func values(r resp, series int) []float64 {
	s := r.body["series"].([]any)[series].(map[string]any)
	var out []float64
	for _, p := range s["points"].([]any) {
		out = append(out, p.(map[string]any)["v"].(float64))
	}
	return out
}

// tenantKey provisions a tenant through the platform key and returns an
// admin key of it.
func (e *env) tenantKey(external string) string {
	e.must(e.do("PUT", "/v1/tenants/by-external-id/"+external, e.platformKey, map[string]any{"name": external}), 201)
	return e.must(e.do("POST", "/v1/api-keys", e.platformKey, map[string]any{"name": "k", "role": "admin"},
		"X-Tenant-External-ID", external), 201).body["key"].(string)
}

func metricQuery(from, to time.Time, extra string) string {
	return "/v1/metrics/query?from=" + url.QueryEscape(from.Format(time.RFC3339)) +
		"&to=" + url.QueryEscape(to.Format(time.RFC3339)) + extra
}

// F07: results go through the writer into grouped raw samples; queries read
// raw data, 5-minute and hourly rollups, and fill the part the rollups have
// not reached from the finer level.
func TestMetricsStoreAndQuery(t *testing.T) {
	e := setup(t, false)
	k := e.tenantKey("acc")
	ctx := context.Background()
	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "web", "type": "web"}), 201))
	chk := e.id(e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{"name": "home", "plugin": "http"}), 201))
	ping := e.id(e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{"name": "ping", "plugin": "icmp"}), 201))
	tenant := uuid.MustParse(e.must(e.do("GET", "/v1/tenant", k, nil), 200).body["id"].(string))

	w, err := metrics.NewWriter(metrics.WriterOptions{System: db.System})
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	nan := math.NaN()
	for i := range 60 {
		connect := float64(i)
		if i%2 == 1 {
			connect = nan
		}
		w.Add(runner.Result{TenantID: tenant, DeviceID: uuid.MustParse(dev), CheckID: uuid.MustParse(chk), Plugin: "http",
			Result: plugin.Result{Time: base.Add(time.Duration(i) * time.Minute),
				Metrics: []float64{nan, connect, nan, nan, float64(i), 200, 1024}}})
	}
	w.Add(runner.Result{TenantID: tenant, DeviceID: uuid.MustParse(dev), CheckID: uuid.MustParse(ping), Plugin: "icmp",
		Result: plugin.Result{Time: base, Metrics: []float64{1.5, 2, 0}}})
	w.Add(runner.Result{TenantID: tenant, CheckID: uuid.MustParse(chk), Plugin: "http", // all NaN: ignored
		Result: plugin.Result{Time: base, Metrics: []float64{nan, nan, nan, nan, nan, nan, nan}}})
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := db.System.QueryRow(ctx, `SELECT count(*) FROM metric_samples`).Scan(&rows); err != nil || rows != 61 {
		t.Fatalf("raw rows = %d, %v; want 61 (one per result)", rows, err)
	}

	series := e.must(e.do("GET", "/v1/metrics/series?device_id="+dev, k, nil), 200).body["items"].([]any)
	if len(series) != 10 {
		t.Fatalf("series = %d, want 7 http + 3 icmp", len(series))
	}
	for _, s := range series {
		// The tenant may only use standard: icmp's high-frequency falls back.
		if c := s.(map[string]any)["retention_class"]; c != "standard" {
			t.Errorf("class = %v", c)
		}
	}
	e.must(e.do("GET", "/v1/metrics/series", k, nil), 422)

	from, to := base, base.Add(time.Hour)
	sel := "&check_id=" + chk + "&name=total_ms&name=connect_ms"
	r := e.must(e.do("GET", metricQuery(from, to, sel+"&step=300&resolution=raw"), k, nil), 200)
	if r.body["resolution"] != "raw" || len(r.body["series"].([]any)) != 2 {
		t.Fatalf("before rollups: %s", r.raw)
	}
	// Series are ordered by name: connect_ms, then total_ms.
	want := make([]float64, 12)
	for b := range want {
		want[b] = float64(5*b + 2)
	}
	if got := values(r, 1); !slices.Equal(got, want) {
		t.Errorf("raw total_ms = %v, want %v", got, want)
	}
	if got := values(r, 0); len(got) != 12 || got[0] != 2 { // avg of 0, 2, 4
		t.Errorf("raw connect_ms (NaN skipped) = %v", got)
	}
	// No rollups yet: 5-minute and hourly queries read raw data.
	if r := e.must(e.do("GET", metricQuery(from, to, sel+"&step=300"), k, nil), 200); r.body["resolution"] != "5m" ||
		!slices.Equal(values(r, 1), want) {
		t.Errorf("5m before rollups: %s", r.raw)
	}
	r = e.must(e.do("GET", metricQuery(from, to, sel+"&step=3600&agg=max"), k, nil), 200)
	if r.body["resolution"] != "1h" || !slices.Equal(values(r, 1), []float64{59}) {
		t.Errorf("hourly before rollups: %s", r.raw)
	}

	m := newMaintainer(t)
	if n, err := m.Rollup5m(ctx); err != nil || n == 0 {
		t.Fatalf("rollup 5m: %d, %v", n, err)
	}
	r = e.must(e.do("GET", metricQuery(from, to, sel+"&step=300"), k, nil), 200)
	if r.body["resolution"] != "5m" || !slices.Equal(values(r, 1), want) {
		t.Errorf("5m rollups: %s", r.raw)
	}
	if n, err := m.Rollup1h(ctx); err != nil || n == 0 {
		t.Fatalf("rollup 1h: %d, %v", n, err)
	}
	if n, err := m.Rollup5m(ctx); err != nil {
		t.Fatalf("second 5m run: %d, %v", n, err)
	}
	r = e.must(e.do("GET", metricQuery(from, to, sel+"&step=3600"), k, nil), 200)
	if !slices.Equal(values(r, 1), []float64{29.5}) {
		t.Errorf("1h rollup avg: %s", r.raw)
	}
	r = e.must(e.do("GET", metricQuery(from, to, sel+"&step=3600&agg=min&resolution=1h"), k, nil), 200)
	if !slices.Equal(values(r, 1), []float64{0}) {
		t.Errorf("1h rollup min: %s", r.raw)
	}
	// Grafana reads the views, and nothing else.
	graf, err := pgx.Connect(ctx, strings.Replace(db.SystemDSN, "monitor_system:change-me-system", "monitor_grafana:change-me-grafana", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = graf.Close(ctx) }()
	for _, v := range []string{"metrics_raw", "metrics_5m", "metrics_1h", "metric_series_v", "devices_v", "check_states_v"} {
		var n int
		if err := graf.QueryRow(ctx, `SELECT count(*) FROM `+v).Scan(&n); err != nil || n == 0 {
			t.Errorf("grafana %s: %d rows, %v", v, n, err)
		}
	}
	if _, err := graf.Exec(ctx, `SELECT 1 FROM metric_samples LIMIT 1`); err == nil {
		t.Error("grafana reads the base tables")
	}

	e.must(e.do("GET", metricQuery(from, from.Add(48*time.Hour), sel+"&step=1"), k, nil), 422)

	// Another tenant sees nothing of it.
	other := e.tenantKey("other")
	if l := e.must(e.do("GET", "/v1/metrics/series?device_id="+dev, other, nil), 200); len(l.body["items"].([]any)) != 0 {
		t.Errorf("cross-tenant series: %s", l.raw)
	}
	if l := e.must(e.do("GET", metricQuery(from, to, sel), other, nil), 200); len(l.body["series"].([]any)) != 0 {
		t.Errorf("cross-tenant query: %s", l.raw)
	}

	// Raw data kept 3 days: a query starting earlier reads 5-minute rollups.
	e.must(e.do("PUT", "/v1/metrics/retention-classes/standard", e.platformKey, map[string]any{"raw_days": 3}), 200)
	r = e.must(e.do("GET", metricQuery(time.Now().Add(-5*24*time.Hour), time.Now(), sel+"&step=60"), k, nil), 200)
	if r.body["resolution"] != "5m" {
		t.Errorf("past raw retention: %s", r.raw)
	}
}

// F07: retention is set per class through the platform API, and the
// maintenance run drops whole expired partitions.
func TestRetention(t *testing.T) {
	e := setup(t, false)
	ctx := context.Background()
	tenant := e.tenantKey("acc")
	l := e.must(e.do("GET", "/v1/metrics/retention-classes", tenant, nil), 200).body["items"].([]any)
	if len(l) != 3 || l[1].(map[string]any)["name"] != "standard" || l[1].(map[string]any)["raw_days"] != nil {
		t.Fatalf("classes: %v", l)
	}
	keep := map[string]any{"raw_days": 3, "rollup_5m_days": 30, "rollup_1h_days": 365}
	e.must(e.do("PUT", "/v1/metrics/retention-classes/standard", tenant, keep), 403)
	e.must(e.do("PUT", "/v1/metrics/retention-classes/standard", e.platformKey,
		map[string]any{"raw_days": 3, "rollup_5m_days": 2}), 422)
	e.must(e.do("PUT", "/v1/metrics/retention-classes/Bad_Name", e.platformKey, keep), 400)
	r := e.must(e.do("PUT", "/v1/metrics/retention-classes/standard", e.platformKey, keep), 200)
	if r.body["raw_days"] != float64(3) || r.body["rollup_1h_days"] != float64(365) {
		t.Fatalf("put: %s", r.raw)
	}
	e.must(e.do("PUT", "/v1/metrics/retention-classes/standard", e.platformKey, keep), 200)
	if n := auditCount(t, "retention_class.update"); n != 1 {
		t.Errorf("retention_class.update events = %d, want 1 (the repeat changes nothing)", n)
	}
	e.must(e.do("PUT", "/v1/metrics/retention-classes/standard-1y", e.platformKey, map[string]any{"rollup_1h_days": 365}), 201)

	if _, err := db.System.Exec(ctx, `SELECT metrics_ensure_partitions(now() - interval '10 days', now())`); err != nil {
		t.Fatal(err)
	}
	dropped, err := newMaintainer(t).ApplyRetention(ctx)
	if err != nil {
		t.Fatal(err)
	}
	day := func(d int) string { return time.Now().UTC().AddDate(0, 0, -d).Format("20060102") }
	want := fmt.Sprintf("metric_samples_c2_d%s", day(5))
	if !slices.Contains(dropped, want) {
		t.Errorf("dropped %v, want it to include %s", dropped, want)
	}
	for _, name := range dropped {
		if !strings.HasPrefix(name, "metric_samples_c2_") {
			t.Errorf("dropped %s: only standard raw has an expired policy", name)
		}
		if name >= fmt.Sprintf("metric_samples_c2_d%s", day(3)) {
			t.Errorf("dropped %s, which is within 3 days", name)
		}
	}
	if again, err := newMaintainer(t).ApplyRetention(ctx); err != nil || len(again) != 0 {
		t.Errorf("second run dropped %v, %v", again, err)
	}
}
