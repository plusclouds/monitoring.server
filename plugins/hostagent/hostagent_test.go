package hostagent

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

const id = "web-1"

func cfg() json.RawMessage { return json.RawMessage(`{"agent_id":"` + id + `"}`) }

func sample(agent string) []byte {
	b, _ := json.Marshal(map[string]any{
		"v": 1, "agent_id": agent, "seq": 7, "ts": 1790000000.5, "agent_version": "0.1.0",
		"objects": []any{
			map[string]any{"key": "host", "kind": "host", "metrics": map[string]any{"cpu_usage_pct": 12.5, "load1": 0.4, "bogus": 1}},
			map[string]any{"key": "service:redis", "kind": "service", "name": "redis", "status": "critical", "output": "refused",
				"metrics": map[string]any{"up": 0}},
		},
	})
	return b
}

func TestParse(t *testing.T) {
	res, err := (&Agent{}).Parse(cfg(), sample(id))
	if err != nil || len(res) != 1 {
		t.Fatalf("parse: %v %v", res, err)
	}
	r := res[0]
	if r.Status != plugin.Critical || !r.Time.Equal(time.Unix(1790000000, 5e8)) || len(r.Objects) != 2 {
		t.Fatalf("result: %+v", r)
	}
	h := r.Objects[0].Metrics
	cpu, _ := Slot("cpu_usage_pct")
	mem, _ := Slot("mem_usage_pct")
	if h[cpu] != 12.5 || !math.IsNaN(h[mem]) || len(h) != len(Metrics()) {
		t.Errorf("host metrics: %v", h)
	}
	if o := r.Objects[1]; o.Name != "redis" || o.Status != plugin.Critical || o.Labels["kind"] != "service" {
		t.Errorf("service: %+v", o)
	}
}

func TestParseRejects(t *testing.T) {
	a := &Agent{}
	for name, body := range map[string][]byte{
		"other agent": sample("db-1"),
		"not json":    []byte("{"),
		"bad status":  []byte(`{"agent_id":"web-1","objects":[{"key":"k","status":"fine"}]}`),
		"no key":      []byte(`{"agent_id":"web-1","objects":[{"kind":"host"}]}`),
		"dup key":     []byte(`{"agent_id":"web-1","objects":[{"key":"k"},{"key":"k"}]}`),
	} {
		if _, err := a.Parse(cfg(), body); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestValidate(t *testing.T) {
	a := &Agent{}
	if err := a.Validate(cfg()); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{`{}`, `{"agent_id":"-x"}`, `{"agent_id":"a","missed_count":0}`} {
		if a.Validate(json.RawMessage(bad)) == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestCatalogUnique(t *testing.T) {
	if len(index) != len(catalog) {
		t.Fatal("duplicate metric name in catalog")
	}
}

func FuzzParse(f *testing.F) {
	f.Add(sample(id))
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = (&Agent{}).Parse(cfg(), b) })
}
