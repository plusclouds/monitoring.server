package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/ingest"
	"github.com/plusclouds/monitoring.server/internal/metrics"
	"github.com/plusclouds/monitoring.server/internal/runner"
)

func agentReport(id string, redisStatus string, cpu float64) string {
	b, _ := json.Marshal(map[string]any{
		"v": 1, "agent_id": id, "seq": 1, "ts": time.Now().Unix(), "agent_version": "0.1.0",
		"objects": []any{
			map[string]any{"key": "host", "kind": "host", "metrics": map[string]any{"cpu_usage_pct": cpu, "mem_usage_pct": 40}},
			map[string]any{"key": "service:redis", "kind": "service", "name": "redis", "status": redisStatus,
				"metrics": map[string]any{"up": 1}},
		},
	})
	return string(b)
}

// host.agent (the monitoring agent in a Docker image, own push token): one
// check per agent; reports become host and service objects; a service the
// agent calls critical is an incident, a threshold on the host's CPU opens
// one on that object, silence is CRITICAL, a rotated token stops the old one.
func TestHostAgent(t *testing.T) {
	ctx := context.Background()
	x := newNoise(t, 0)
	const agent = "web-1"
	dev := x.device(agent)
	body := map[string]any{"name": "agent", "plugin": "host.agent", "is_host_check": true, "interval_seconds": 30,
		"config":     map[string]any{"agent_id": agent},
		"thresholds": []any{map[string]any{"metric": "cpu_usage_pct", "object": "host", "warning": map[string]any{"op": ">", "value": 80}}}}
	created := x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, body), 201)
	chk := x.id(created)
	token, _ := created.body["push_token"].(string)
	if !strings.HasPrefix(token, "mpush_") {
		t.Fatalf("created: %s", created.raw)
	}
	other := x.device("web-copy")
	x.must(x.do("POST", "/v1/devices/"+other+"/checks", x.key, map[string]any{"name": "agent", "plugin": "host.agent",
		"interval_seconds": 30, "config": map[string]any{"agent_id": strings.ToUpper(agent)}}), 409)
	x.must(x.do("POST", "/v1/devices/"+other+"/checks", x.key, map[string]any{"name": "agent", "plugin": "host.agent",
		"interval_seconds": 30, "config": map[string]any{"agent_id": "bad id"}}), 422)

	results := make(chan runner.Result, 100)
	in, err := ingest.New(ingest.Options{Config: config.IngestHTTP{MaxBody: 64 << 10, RateLimit: config.Rate{PerSecond: 1000, Burst: 1000}},
		System: db.System, Results: results, Logger: slog.New(slog.DiscardHandler), Registry: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(in.Handler())
	defer srv.Close()
	w, err := metrics.NewWriter(metrics.WriterOptions{System: db.System})
	if err != nil {
		t.Fatal(err)
	}
	post := func(tok, payload string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/ingest/v1/"+chk, strings.NewReader(payload))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		_, _ = io.Copy(io.Discard, res.Body)
		return res.StatusCode
	}
	apply := func() {
		t.Helper()
		for {
			select {
			case r := <-results:
				if _, err := x.eng.Apply(ctx, r); err != nil {
					t.Fatal(err)
				}
				w.Add(r)
			default:
				if err := w.Flush(ctx); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
	}
	state := func() map[string]any {
		return x.must(x.do("GET", "/v1/checks/"+chk+"/state", x.key, nil), 200).body
	}

	if code := post("", agentReport(agent, "ok", 10)); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code := post(token, agentReport("other", "ok", 10)); code != 400 {
		t.Errorf("another agent's report: %d", code)
	}

	if code := post(token, agentReport(agent, "ok", 10)); code != 202 {
		t.Fatalf("report: %d", code)
	}
	apply()
	if st := state(); st["phase"] != "OK" {
		t.Fatalf("state: %v", st)
	}
	objs := x.must(x.do("GET", "/v1/checks/"+chk+"/objects", x.key, nil), 200).body["items"].([]any)
	byKey := map[string]map[string]any{}
	for _, o := range objs {
		m := o.(map[string]any)
		byKey[m["key"].(string)] = m
	}
	if len(byKey) != 2 || byKey["host"] == nil || byKey["service:redis"] == nil {
		t.Fatalf("objects: %v", objs)
	}
	if m := byKey["host"]["last_metrics"].(map[string]any); m["cpu_usage_pct"] != float64(10) {
		t.Errorf("host metrics: %v", m)
	}

	// CPU over the threshold: a warning on the host object.
	if code := post(token, agentReport(agent, "ok", 95)); code != 202 {
		t.Fatal(code)
	}
	apply()
	if inc := x.incident(chk); inc["object_key"] != "host" || inc["severity"] != "warning" {
		t.Errorf("cpu threshold: %v", inc)
	}

	// The agent calls a service critical: the check is CRITICAL.
	if code := post(token, agentReport(agent, "critical", 10)); code != 202 {
		t.Fatal(code)
	}
	apply()
	if st := state(); st["phase"] != "PROBLEM" {
		t.Errorf("critical service: %v", st)
	}
	if code := post(token, agentReport(agent, "ok", 10)); code != 202 {
		t.Fatal(code)
	}
	apply()
	if st := state(); st["phase"] != "OK" {
		t.Errorf("recovery: %v", st)
	}

	// Silence: 3 intervals without a report is CRITICAL.
	if _, err := db.System.Exec(ctx, `UPDATE push_sources SET last_push_at = now() - interval '5 minutes',
		created_at = now() - interval '1 hour' WHERE check_id = $1`, chk); err != nil {
		t.Fatal(err)
	}
	if n, err := in.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	apply()
	if st := state(); st["phase"] != "PROBLEM" || !strings.Contains(st["last_output"].(string), "3 intervals") {
		t.Errorf("silent: %v", st)
	}

	// Rotation: the old token stops at once.
	fresh, _ := x.must(x.do("POST", "/v1/checks/"+chk+"/rotate-token", x.key, nil), 200).body["push_token"].(string)
	if fresh == "" || post(token, agentReport(agent, "ok", 10)) != 401 || post(fresh, agentReport(agent, "ok", 10)) != 202 {
		t.Error("token rotation")
	}
}
