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
	dto "github.com/prometheus/client_model/go"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/ingest"
	"github.com/plusclouds/monitoring.server/internal/metrics"
	"github.com/plusclouds/monitoring.server/internal/runner"
)

func boxBeat(box, state, severity string, xid float64, extra map[string]any) string {
	m := map[string]any{
		"v": 1, "box_id": box, "seq": 1, "ts": time.Now().Unix(), "agent_version": "1.4.0",
		"state": state, "severity": severity, "backend_healthy": true, "router_connected": true,
		"vllm":  map[string]any{"up": true, "restart_count": 0},
		"queue": map[string]any{"running": 1, "waiting": 0},
		"host":  map[string]any{"load": 1.0, "mem_pct": 40, "disk_pct": 50, "oom_kills": 0},
		"gpu": []any{map[string]any{"idx": 0, "temp": 70, "util": 90, "mem_used": 40e9, "mem_total": 80e9,
			"ecc_dbe": 0, "xid_last": xid, "throttle": false}},
	}
	for k, v := range extra {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// box.agent (LLM box heartbeats over HTTPS with their own push token): the
// provisioner creates a device and a check per box and gets the token once;
// heartbeats become host, vllm and GPU objects; boot states are not
// incidents, a hard GPU fault and silence are; a rotated token stops the
// old one; every answer is counted by status code.
func TestBoxAgent(t *testing.T) {
	ctx := context.Background()
	x := newNoise(t, 0)
	const box = "box-7f3a"
	dev := x.id(x.must(x.do("POST", "/v1/devices", x.key, map[string]any{"name": box, "type": "server",
		"external": map[string]any{"source": "greenference", "type": "llmbox", "id": box}}), 201))
	body := map[string]any{"name": "box", "plugin": "box.agent", "is_host_check": true, "interval_seconds": 1,
		"config": map[string]any{"box_id": box},
		"thresholds": []any{map[string]any{"metric": "gpu_temp_c", "object": "*", "warning": map[string]any{"op": ">", "value": 85}}}}

	// The tenant's minimum interval applies: a 1 s check needs it lowered.
	x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, body), 422)
	if _, err := db.System.Exec(ctx, `UPDATE tenants SET min_check_interval_seconds = 1 WHERE id = $1`, x.tenant); err != nil {
		t.Fatal(err)
	}
	created := x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, body), 201)
	chk := x.id(created)
	token, _ := created.body["push_token"].(string)
	if !strings.HasPrefix(token, "mpush_") || created.body["failure_count"] != float64(1) {
		t.Fatalf("created: %s", created.raw)
	}
	if got := x.must(x.do("GET", "/v1/checks/"+chk, x.key, nil), 200); got.body["push_token"] != nil {
		t.Errorf("the token is shown again: %s", got.raw)
	}
	// One check per box on the whole server; a bad box_id is refused.
	other := x.device("box-copy")
	x.must(x.do("POST", "/v1/devices/"+other+"/checks", x.key, map[string]any{"name": "box", "plugin": "box.agent",
		"interval_seconds": 1, "config": map[string]any{"box_id": strings.ToUpper(box)}}), 409)
	x.must(x.do("POST", "/v1/devices/"+other+"/checks", x.key, map[string]any{"name": "box", "plugin": "box.agent",
		"interval_seconds": 1, "config": map[string]any{"box_id": "bad id"}}), 422)

	results := make(chan runner.Result, 100)
	reg := prometheus.NewRegistry()
	in, err := ingest.New(ingest.Options{Config: config.IngestHTTP{MaxBody: 64 << 10, RateLimit: config.Rate{PerSecond: 1000, Burst: 1000}},
		System: db.System, Results: results, Logger: slog.New(slog.DiscardHandler), Registry: reg})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(in.Handler())
	defer srv.Close()
	w, err := metrics.NewWriter(metrics.WriterOptions{System: db.System})
	if err != nil {
		t.Fatal(err)
	}
	post := func(id, tok, payload string) int {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/ingest/v1/"+id, strings.NewReader(payload))
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
	apply := func() int {
		t.Helper()
		n := 0
		for {
			select {
			case r := <-results:
				if _, err := x.eng.Apply(ctx, r); err != nil {
					t.Fatal(err)
				}
				w.Add(r)
				n++
			default:
				if err := w.Flush(ctx); err != nil {
					t.Fatal(err)
				}
				return n
			}
		}
	}
	state := func() map[string]any {
		return x.must(x.do("GET", "/v1/checks/"+chk+"/state", x.key, nil), 200).body
	}

	// Auth and identity: no token, a wrong token, another box's heartbeat.
	if code := post(chk, "", boxBeat(box, "ready", "ok", 0, nil)); code != 401 {
		t.Errorf("no token: %d", code)
	}
	if code := post(chk, "mpush_nope", boxBeat(box, "ready", "ok", 0, nil)); code != 401 {
		t.Errorf("wrong token: %d", code)
	}
	if code := post(chk, token, boxBeat("other-box", "ready", "ok", 0, nil)); code != 400 {
		t.Errorf("another box's heartbeat: %d", code)
	}

	// A healthy beat, with the box's own count of the server's answers.
	if code := post(chk, token, boxBeat(box, "ready", "ok", 0,
		map[string]any{"monitor_responses": map[string]any{"2xx": 10, "429": 2, "5xx": 1}})); code != 202 {
		t.Fatalf("beat: %d", code)
	}
	apply()
	if st := state(); st["phase"] != "OK" {
		t.Fatalf("state after a beat: %v", st)
	}
	objs := x.must(x.do("GET", "/v1/checks/"+chk+"/objects", x.key, nil), 200).body["items"].([]any)
	byKey := map[string]map[string]any{}
	for _, o := range objs {
		m := o.(map[string]any)
		byKey[m["key"].(string)] = m
	}
	if len(byKey) != 3 || byKey["host"] == nil || byKey["vllm"] == nil || byKey["gpu:0"] == nil {
		t.Fatalf("objects: %v", objs)
	}
	if m := byKey["host"]["last_metrics"].(map[string]any); m["monitor_responses_429"] != float64(2) || m["monitor_responses_2xx"] != float64(10) {
		t.Errorf("response counts: %v", m)
	}
	if m := byKey["gpu:0"]["last_metrics"].(map[string]any); m["gpu_mem_usage_pct"] != float64(50) {
		t.Errorf("gpu: %v", m)
	}

	// Boot, download and restart are not incidents, even when the agent says failed.
	for _, s := range []string{"booting", "downloading", "restarting", "stopped"} {
		if code := post(chk, token, boxBeat(box, s, "failed", 0, nil)); code != 202 {
			t.Fatalf("%s: %d", s, code)
		}
		apply()
		if st := state(); st["phase"] != "OK" {
			t.Errorf("%s is an incident: %v", s, st)
		}
	}

	// The GPU gets hot: the threshold opens a warning on that object only.
	if code := post(chk, token, strings.Replace(boxBeat(box, "ready", "ok", 0, nil), `"temp":70`, `"temp":91`, 1)); code != 202 {
		t.Fatal(code)
	}
	apply()
	if inc := x.incident(chk); inc["object_key"] != "gpu:0" || inc["severity"] != "warning" {
		t.Errorf("temperature threshold: %v", inc)
	}

	// A hard GPU fault (XID 79) is CRITICAL at once, whatever the state.
	if code := post(chk, token, boxBeat(box, "ready", "ok", 79, nil)); code != 202 {
		t.Fatal(code)
	}
	apply()
	if st := state(); st["phase"] != "PROBLEM" || !strings.Contains(st["last_output"].(string), "XID 79") {
		t.Errorf("XID 79: %v", st)
	}
	// Recovery.
	if code := post(chk, token, boxBeat(box, "ready", "ok", 0, nil)); code != 202 {
		t.Fatal(code)
	}
	apply()
	if st := state(); st["phase"] != "OK" {
		t.Errorf("after recovery: %v", st)
	}

	// Silence: 5 intervals without a beat is CRITICAL, once per interval.
	if _, err := db.System.Exec(ctx, `UPDATE push_sources SET last_push_at = now() - interval '1 minute',
		created_at = now() - interval '1 hour' WHERE check_id = $1`, chk); err != nil {
		t.Fatal(err)
	}
	if n, err := in.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	apply()
	if st := state(); st["phase"] != "PROBLEM" || !strings.Contains(st["last_output"].(string), "5 intervals") {
		t.Errorf("silent: %v", st)
	}
	if code := post(chk, token, boxBeat(box, "ready", "ok", 0, nil)); code != 202 {
		t.Fatal(code)
	}
	apply()
	if st := state(); st["phase"] != "OK" {
		t.Errorf("after the box came back: %v", st)
	}

	// Rotation: the old token stops at once.
	rot := x.must(x.do("POST", "/v1/checks/"+chk+"/rotate-token", x.key, nil), 200)
	fresh, _ := rot.body["push_token"].(string)
	if fresh == "" || fresh == token {
		t.Fatalf("rotate: %s", rot.raw)
	}
	if code := post(chk, token, boxBeat(box, "ready", "ok", 0, nil)); code != 401 {
		t.Errorf("old token after rotation: %d", code)
	}
	if code := post(chk, fresh, boxBeat(box, "ready", "ok", 0, nil)); code != 202 {
		t.Errorf("new token: %d", code)
	}
	apply()

	// A disabled check (deprovisioned box) refuses data.
	x.must(x.do("PATCH", "/v1/checks/"+chk, x.key, map[string]any{"enabled": false}), 200)
	if code := post(chk, fresh, boxBeat(box, "ready", "ok", 0, nil)); code != 409 {
		t.Errorf("disabled: %d", code)
	}

	// Every answer was counted by status code.
	counted := map[string]float64{}
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() != "ingest_http_responses_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			counted[labelValue(m, "code")] = m.GetCounter().GetValue()
		}
	}
	if counted["401"] != 3 || counted["409"] != 1 || counted["400"] != 1 || counted["202"] < 10 {
		t.Errorf("answers by code: %v", counted)
	}
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}
