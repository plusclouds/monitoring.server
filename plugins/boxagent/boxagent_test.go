package boxagent

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

const box = "box-7f3a"

// Sample is a heartbeat as box.agent encodes it.
func Sample(id, state, severity string, xid float64) []byte {
	b, _ := json.Marshal(map[string]any{
		"v": 1, "box_id": id, "seq": 42, "ts": 1790000000.5, "agent_version": "1.4.0",
		"state": state, "severity": severity, "backend_healthy": true, "router_connected": true,
		"vllm":  map[string]any{"up": true, "restart_count": 1, "last_restart_reason": "watchdog"},
		"queue": map[string]any{"running": 3, "waiting": 7},
		"host":  map[string]any{"load": 1.5, "mem_pct": 40.2, "disk_pct": 61, "oom_kills": 0},
		"gpu": []any{
			map[string]any{"idx": 0, "temp": 71, "util": 98, "mem_used": 40e9, "mem_total": 80e9, "ecc_dbe": 0, "xid_last": 0, "throttle": false},
			map[string]any{"idx": 1, "temp": 69, "util": 97, "mem_used": 41e9, "mem_total": 80e9, "ecc_dbe": 0, "xid_last": xid, "throttle": true},
		},
	})
	return b
}

func cfg() json.RawMessage { return json.RawMessage(`{"box_id":"` + box + `"}`) }

func TestParseOK(t *testing.T) {
	res, err := (&Agent{}).Parse(cfg(), Sample(box, "ready", "ok", 0))
	if err != nil || len(res) != 1 {
		t.Fatalf("parse: %v %v", res, err)
	}
	r := res[0]
	if r.Status != plugin.OK || !r.Time.Equal(time.Unix(1790000000, 5e8)) {
		t.Errorf("result: %+v", r)
	}
	if len(r.Objects) != 4 || r.Objects[0].Key != "host" || r.Objects[1].Key != "vllm" || r.Objects[3].Key != "gpu:1" {
		t.Fatalf("objects: %+v", r.Objects)
	}
	if h := r.Objects[0].Metrics; h[Load1] != 1.5 || h[MemUsage] != 40.2 || h[RouterConnected] != 1 || !math.IsNaN(h[GPUTemp]) {
		t.Errorf("host: %v", h)
	}
	if v := r.Objects[1].Metrics; v[VLLMUp] != 1 || v[QueueWaiting] != 7 || v[VLLMRestarts] != 1 {
		t.Errorf("vllm: %v", v)
	}
	if g := r.Objects[2].Metrics; g[GPUTemp] != 71 || g[GPUMemUsage] != 50 || g[GPUThrottle] != 0 {
		t.Errorf("gpu0: %v", g)
	}
	if len(r.Objects[0].Metrics) != len(Metrics()) {
		t.Error("object metrics do not match the layout")
	}
}

func TestStatus(t *testing.T) {
	for _, c := range []struct {
		name            string
		state, severity string
		xid             float64
		want            plugin.Status
	}{
		{"ready", "ready", "ok", 0, plugin.OK},
		{"degraded", "ready", "degraded", 0, plugin.Warning},
		{"failed", "ready", "failed", 0, plugin.Critical},
		{"unhealthy", "unhealthy", "", 0, plugin.Critical},
		{"booting is exempt", "booting", "failed", 0, plugin.OK},
		{"downloading is exempt", "downloading", "failed", 0, plugin.OK},
		{"restarting is exempt", "restarting", "failed", 0, plugin.OK},
		{"stopped is exempt", "stopped", "failed", 0, plugin.OK},
		{"xid 79 is immediate", "booting", "ok", 79, plugin.Critical},
		{"xid 31 is not a hard fault", "ready", "ok", 31, plugin.OK},
	} {
		res, err := (&Agent{}).Parse(cfg(), Sample(box, c.state, c.severity, c.xid))
		if err != nil || res[0].Status != c.want {
			t.Errorf("%s: %v %v", c.name, res, err)
		}
	}
	res, _ := (&Agent{}).Parse(cfg(), Sample(box, "ready", "ok", 79))
	if g := res[0].Objects[3]; g.Status != plugin.Critical || g.Output != "XID 79" {
		t.Errorf("gpu object: %+v", g)
	}
}

func TestMonitorResponses(t *testing.T) {
	var m map[string]any
	_ = json.Unmarshal(Sample(box, "ready", "ok", 0), &m)
	m["monitor_responses"] = map[string]any{"2xx": 100, "429": 3, "5xx": 2, "errors": 1}
	b, _ := json.Marshal(m)
	res, err := (&Agent{}).Parse(cfg(), b)
	if err != nil {
		t.Fatal(err)
	}
	h := res[0].Objects[0].Metrics
	if h[Resp2xx] != 100 || h[Resp429] != 3 || h[Resp5xx] != 2 || h[RespErrors] != 1 || !math.IsNaN(h[Resp4xx]) {
		t.Errorf("responses: %v", h)
	}
}

func TestRouterDown(t *testing.T) {
	var m map[string]any
	_ = json.Unmarshal(Sample(box, "ready", "ok", 0), &m)
	m["router_connected"] = false
	b, _ := json.Marshal(m)
	res, err := (&Agent{}).Parse(cfg(), b)
	if err != nil || res[0].Status != plugin.Warning {
		t.Errorf("router down: %v %v", res, err)
	}
}

func TestParseErrors(t *testing.T) {
	a := &Agent{}
	if _, err := a.Parse(cfg(), Sample("other-box", "ready", "ok", 0)); err == nil {
		t.Error("a token must not report for another box")
	}
	for _, body := range []string{``, `{`, `[]`, `{"box_id":""}`, `{"box_id":"a b"}`, `{"box_id":"x","severity":"bad"}`} {
		if _, err := a.Parse(json.RawMessage(`{"box_id":"x"}`), []byte(body)); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
}

func TestValidate(t *testing.T) {
	a := &Agent{}
	for raw, ok := range map[string]bool{
		`{"box_id":"box-1"}`:                    true,
		`{"box_id":"box-1","missed_count":10}`:  true,
		`{}`:                                    false,
		`{"box_id":"bad id"}`:                   false,
		`{"box_id":"box-1","missed_count":0}`:   false,
		`{"box_id":"box-1","missed_count":101}`: false,
		`{"box_id":"box-1","extra":1}`:          false,
	} {
		if err := a.Validate(json.RawMessage(raw)); (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

func TestMissedCount(t *testing.T) {
	if MissedCount(cfg()) != 3 || MissedCount(json.RawMessage(`{"box_id":"x","missed_count":8}`)) != 8 {
		t.Error("missed_count")
	}
}

func FuzzDecode(f *testing.F) {
	f.Add(Sample(box, "ready", "ok", 0))
	f.Add([]byte(`{"box_id":"x","gpu":[{}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		m, err := Decode(body)
		if err != nil {
			return
		}
		for _, o := range Objects(m) {
			if len(o.Metrics) != NumMetrics {
				t.Fatalf("object %s has %d metrics", o.Key, len(o.Metrics))
			}
		}
		Status(m)
	})
}
