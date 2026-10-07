package push

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Payloads as FixLean ESP32 sensors send them (shape from the collector).
const (
	fixleanStatus = `{"Product":"FL-VIB-3","Version":"2.4.1","RSSI":-61,"Local IP":"10.1.2.3","Network MAC":"24:6F:28:AA:BB:CC",
		"Last Reset Reason":"POWERON","Runtime MS":86400000,"Memory Info":{"Free Heap":120000,"Min Free Heap":98000},
		"ESP-IDF Version":"v5.1","Compile Date":"Sep 1 2026","Compile Time":"10:00:00","Current Running Application":"vib",
		"unixtimestamp":1759820400}`
	fixleanTelemetry = `{"vrms_x":1.25,"vrms_y":0.8,"grms_z":0.31,"temperature":41.5,"note":"ok","unixtimestamp":1759820460,
		"sampling_rate":3200,"sample_size":4096,"measurement_buffer_size":8}`
)

func TestDecodeFixLean(t *testing.T) {
	m, err := Decode(ProfileFixLean, []byte(fixleanStatus))
	if err != nil || !m.Status || m.Info["firmware"] != "2.4.1" || m.Info["product"] != "FL-VIB-3" || m.Info["compiled"] != "Sep 1 2026 10:00:00" ||
		m.Values["rssi_dbm"] != -61 || m.Values["memory_free_bytes"] != 120000 || m.Values["memory_min_free_bytes"] != 98000 ||
		m.Values["runtime_ms"] != 86400000 || !m.Time.Equal(time.Unix(1759820400, 0)) {
		t.Fatalf("status: %+v %v", m, err)
	}
	m, err = Decode(ProfileFixLean, []byte(fixleanTelemetry))
	if err != nil || m.Status || len(m.Values) != 4 || m.Values["vrms_x"] != 1.25 || m.Skipped != 1 {
		t.Fatalf("telemetry: %+v %v", m, err)
	}
	if _, ok := m.Values["sampling_rate"]; ok {
		t.Error("metadata stored as a metric")
	}
	// A status signature wins: never both.
	m, _ = Decode(ProfileFixLean, []byte(`{"status":"online","temperature":20}`))
	if !m.Status || len(m.Values) != 0 {
		t.Errorf("status wins: %+v", m)
	}
}

func TestDecodeJSON(t *testing.T) {
	m, err := Decode(ProfileJSON, []byte(`{"Temp-C":21.5,"env":{"hum":40,"deep":{"x":{"y":1}}},"on":true,"name":"x","1st":2}`))
	if err != nil || m.Values["temp_c"] != 21.5 || m.Values["env_hum"] != 40 || m.Values["on"] != 1 || m.Values["m_1st"] != 2 || m.Skipped != 2 {
		t.Fatalf("json: %+v %v", m, err)
	}
	for _, bad := range []string{`[1]`, `nope`, `{"a":1} {}`} {
		if _, err := Decode(ProfileJSON, []byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	for in, want := range map[string]string{"Vrms X": "vrms_x", "__a--b__": "a_b", "9": "m_9"} {
		if got, ok := MetricName(in); !ok || got != want {
			t.Errorf("%q -> %q", in, got)
		}
	}
	if _, ok := MetricName("--"); ok {
		t.Error("empty name accepted")
	}
}

func TestMQTTParse(t *testing.T) {
	p := &MQTT{}
	cfg := json.RawMessage(`{"profile":"fixlean-esp","fields":["temperature","vrms_x"]}`)
	if err := plugin.ValidateConfig(p, cfg); err != nil {
		t.Fatal(err)
	}
	defs, _ := p.MetricsFor(cfg)
	if len(defs) != 6 || defs[0].Name != "rssi_dbm" || defs[4].Name != "temperature" || defs[5].Name != "vrms_x" {
		t.Fatalf("layout: %+v", defs)
	}
	rs, err := p.Parse(cfg, []byte(fixleanTelemetry))
	if err != nil || rs[0].Metrics[4] != 41.5 || rs[0].Metrics[5] != 1.25 || !math.IsNaN(rs[0].Metrics[0]) ||
		!strings.Contains(rs[0].Output, "2 values") || !strings.Contains(rs[0].Output, "1 non-numeric") {
		t.Fatalf("telemetry: %+v %v", rs, err)
	}
	rs, _ = p.Parse(cfg, []byte(fixleanStatus))
	if rs[0].Metrics[0] != -61 || !strings.Contains(rs[0].Output, "firmware 2.4.1") {
		t.Errorf("status: %+v", rs[0])
	}
	// json with explicit selectors behaves like push.http.
	sel := json.RawMessage(`{"metrics":{"t":"$.a.t"},"status":"$.s"}`)
	if err := plugin.ValidateConfig(p, sel); err != nil {
		t.Fatal(err)
	}
	if rs, err := p.Parse(sel, []byte(`{"a":{"t":5},"s":"warning"}`)); err != nil || rs[0].Status != plugin.Warning || rs[0].Metrics[0] != 5 {
		t.Errorf("selectors: %+v %v", rs, err)
	}
	for _, bad := range []string{`{"profile":"fixlean-esp","metrics":{"a":"$.a"}}`, `{"fields":["A"]}`, `{"fields":["a","a"]}`, `{"profile":"x"}`} {
		if err := plugin.ValidateConfig(p, json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if (&Connection{}).Validate(nil) != nil {
		t.Error("connection config")
	}
}

// FuzzDecode: no payload crashes the profiles (F08, F12), seeded with the
// FixLean shapes.
func FuzzDecode(f *testing.F) {
	for _, s := range []string{fixleanStatus, fixleanTelemetry, `{"a":{"b":{"c":{"d":1}}}}`, `{"Memory Info":5}`, `{`, `[]`} {
		f.Add([]byte(s))
	}
	p := &MQTT{}
	cfgs := []json.RawMessage{json.RawMessage(`{"profile":"fixlean-esp","fields":["vrms_x"]}`), json.RawMessage(`{"fields":["a_b_c"]}`)}
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, profile := range []string{ProfileFixLean, ProfileJSON} {
			m, err := Decode(profile, body)
			if err != nil {
				continue
			}
			for name, v := range m.Values {
				if !metricName.MatchString(name) || math.IsNaN(v) || math.IsInf(v, 0) {
					t.Fatalf("bad value %q=%v", name, v)
				}
			}
		}
		for _, c := range cfgs {
			if rs, err := p.Parse(c, body); err == nil && len(rs) != 1 {
				t.Fatalf("%d results", len(rs))
			}
		}
	})
}
