package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/liveness"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/internal/selfmon"
	"github.com/plusclouds/monitoring.server/internal/webhook"
)

// M5 (F11): the engine reports its own health on the platform tenant's
// monitor device, its thresholds alert, and the heartbeat goes out only
// while the engine applies those results.
func TestSelfMonitoring(t *testing.T) {
	ctx := context.Background()
	x := newNoise(t, 0)
	_, device, check, err := selfmon.Ensure(ctx, db.System)
	if err != nil {
		t.Fatal(err)
	}
	if _, d2, c2, err := selfmon.Ensure(ctx, db.System); err != nil || d2 != device || c2 != check {
		t.Fatalf("not idempotent: %v", err)
	}
	// The monitor device lives in the platform tenant, which this test
	// environment has no API key for: read it from the database.
	var plugin string
	var host bool
	var rules int
	if err := db.System.QueryRow(ctx, `SELECT c.plugin, c.is_host_check, jsonb_array_length(c.thresholds)
		FROM checks c JOIN tenants t ON t.id = c.tenant_id WHERE c.id = $1 AND t.is_platform`, check).Scan(&plugin, &host, &rules); err != nil ||
		plugin != "monitor.self" || !host || rules != len(selfmon.DefaultRules()) {
		t.Fatalf("monitor check: %s %v %d %v", plugin, host, rules, err)
	}

	reg := prometheus.NewRegistry()
	mqttFails := prometheus.NewCounter(prometheus.CounterOpts{Name: "mqtt_auth_failures_total"})
	lag := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "runner_check_lag_seconds", Buckets: []float64{0.1, 1, 10}})
	reg.MustRegister(mqttFails, lag)
	results := make(chan runner.Result, 10)
	col := &selfmon.Collector{System: db.System, Gatherer: reg, Results: results, Logger: slog.New(slog.DiscardHandler)}
	tick := func() runner.Result {
		t.Helper()
		if err := col.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		r := <-results
		if _, err := x.eng.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := tick()
	if r.CheckID != check || r.Status.String() != "OK" || r.Metrics[2] != 0 {
		t.Fatalf("first report: %+v", r)
	}
	// A login storm on the broker and a slow runner within five minutes.
	mqttFails.Add(150)
	for range 100 {
		lag.Observe(0.05)
	}
	lag.Observe(8)
	tick()
	var m map[string]any
	if err := db.System.QueryRow(ctx, `SELECT last_metrics FROM check_state WHERE check_id = $1`, check).Scan(&m); err != nil {
		t.Fatal(err)
	}
	if m["mqtt_auth_failures_5m"] != float64(150) || m["check_lag_p99_seconds"] == nil {
		t.Errorf("measured: %v", m)
	}
	var severity string
	if err := db.System.QueryRow(ctx, `SELECT severity FROM incidents WHERE check_id = $1 AND status <> 'resolved'`, check).Scan(&severity); err != nil ||
		severity != "warning" {
		t.Errorf("auth failure alert: %q %v", severity, err)
	}

	// A deleted check comes back with the default rules.
	if _, err := db.System.Exec(ctx, `DELETE FROM checks WHERE id = $1`, check); err != nil {
		t.Fatal(err)
	}
	if r := tick(); r.CheckID == check {
		t.Error("deleted check reused")
	}

	// The heartbeat: signed, with the token, only while healthy.
	var mu sync.Mutex
	var got []*http.Request
	var bodies [][]byte
	rcv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got, bodies = append(got, r), append(bodies, b)
		mu.Unlock()
	}))
	defer rcv.Close()
	secret, _ := webhook.NewSecret()
	hb, err := selfmon.NewHeartbeat(config.SelfMonitoring{StoreAsMetrics: true}, config.Default().TLS, db.System, "node-a", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	hb.Targets = []selfmon.Target{{URL: rcv.URL + "/ping/abc", Token: "hb-token", Secret: secret}}
	liveness.Reset()
	liveness.Beat("notifier")
	if err := hb.Send(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(got) != 1 || got[0].Header.Get("Authorization") != "Bearer hb-token" {
		t.Fatalf("heartbeat: %d %v", len(got), got)
	}
	ts, _ := strconv.ParseInt(got[0].Header.Get("webhook-timestamp"), 10, 64)
	if !webhook.Verify(secret, got[0].Header.Get("webhook-id"), ts, bodies[0], got[0].Header.Get("webhook-signature")) {
		t.Error("bad signature")
	}
	var ev map[string]any
	_ = json.Unmarshal(bodies[0], &ev)
	if ev["type"] != "monitoring.heartbeat" || ev["data"].(map[string]any)["node_id"] != "node-a" {
		t.Errorf("event: %s", bodies[0])
	}
	mu.Unlock()

	// The engine stopped applying its own results: no heartbeat.
	if _, err := db.System.Exec(ctx, `UPDATE check_state SET last_result_at = now() - interval '10 minutes'
		WHERE check_id IN (SELECT id FROM checks WHERE plugin = 'monitor.self')`); err != nil {
		t.Fatal(err)
	}
	if err := hb.Send(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(got) != 1 {
		t.Errorf("sent while unhealthy: %d", len(got))
	}
	mu.Unlock()
	liveness.Reset()
}
