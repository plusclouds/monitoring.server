package push

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func cfg(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := plugin.ValidateConfig(&HTTP{}, b); err != nil {
		t.Fatalf("config %s: %v", b, err)
	}
	return b
}

func TestSelector(t *testing.T) {
	doc := map[string]any{"a": map[string]any{"b": []any{map[string]any{"c": 1.0}}, "x-y": "z"}}
	for sel, want := range map[string]any{"$.a.b[0].c": 1.0, `$.a["x-y"]`: "z", `$['a']['x-y']`: "z"} {
		s, err := parseSelector(sel)
		if err != nil {
			t.Fatalf("%s: %v", sel, err)
		}
		if got, ok := s.get(doc); !ok || got != want {
			t.Errorf("%s = %v %v", sel, got, ok)
		}
	}
	for _, sel := range []string{"$.a.b[1].c", "$.a.b.c", "$.nope"} {
		s, _ := parseSelector(sel)
		if _, ok := s.get(doc); ok {
			t.Errorf("%s found a value", sel)
		}
	}
	for _, bad := range []string{"a.b", "$.", "$..a", "$[x]", "$[-1]", "$[", `$[""]`, "$a"} {
		if _, err := parseSelector(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestParse(t *testing.T) {
	p := &HTTP{}
	c := cfg(t, HTTPConfig{Metrics: map[string]string{"temperature": "$.sensors.temp", "humidity": "$.sensors.hum", "door": "$.door"},
		Units: map[string]string{"temperature": "celsius"}, Status: "$.state", Output: "$.msg", Timestamp: "$.ts"})
	defs, err := p.MetricsFor(c)
	if err != nil || len(defs) != 3 || defs[0].Name != "door" || defs[1].Name != "humidity" || defs[2].Unit != "celsius" {
		t.Fatalf("layout: %+v %v", defs, err)
	}
	rs, err := p.Parse(c, []byte(`{"sensors":{"temp":21.5,"hum":"40"},"door":true,"state":"warning","msg":"door open","ts":1759820400}`))
	if err != nil || len(rs) != 1 {
		t.Fatalf("parse: %v %v", rs, err)
	}
	r := rs[0]
	if r.Status != plugin.Warning || r.Output != "door open" || r.Metrics[0] != 1 || r.Metrics[1] != 40 || r.Metrics[2] != 21.5 ||
		!r.Time.Equal(time.Unix(1759820400, 0)) {
		t.Errorf("message: %+v", r)
	}

	// A batch, out of order, in milliseconds and RFC 3339; a missing metric is NaN.
	rs, err = p.Parse(c, []byte(`[{"state":0,"ts":"2026-10-07T08:00:00Z","sensors":{"temp":20}},
		{"state":"ok","ts":1759816800000,"sensors":{"temp":19,"hum":"n/a"}}]`))
	if err != nil || len(rs) != 2 || !rs[0].Time.Before(rs[1].Time) || rs[0].Metrics[2] != 19 || !math.IsNaN(rs[0].Metrics[1]) {
		t.Fatalf("batch: %+v %v", rs, err)
	}

	for body, want := range map[string]string{
		`not json`:                         "not JSON",
		`[]`:                               "empty array",
		`42`:                               "object or an array",
		`{"state":"maybe","ts":1}`:         "status maybe",
		`{"ts":1}`:                         "status field is missing",
		`{"state":"ok","ts":"yesterday"}`:  "timestamp yesterday",
		`[{"state":"ok","ts":1}, 5]`:       "message 1",
		`{"state":"ok","ts":1} {"more":1}`: "more than one",
	} {
		if _, err := p.Parse(c, []byte(body)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", body, err, want)
		}
	}

	// Without a status selector every message is OK and the output lists the values.
	if rs, err := p.Parse(c, []byte(`{"state":"ok"}`)); err != nil || !rs[0].Time.IsZero() {
		t.Errorf("no timestamp: %+v %v", rs, err)
	}
	plain := cfg(t, HTTPConfig{Metrics: map[string]string{"v": "$.v"}})
	if rs, err := p.Parse(plain, []byte(`{"v":3}`)); err != nil || rs[0].Status != plugin.OK || rs[0].Output != "pushed: 3" || !rs[0].Time.IsZero() {
		t.Errorf("plain: %+v %v", rs, err)
	}
}

func TestValidate(t *testing.T) {
	for _, raw := range []string{`{}`, `{"metrics":{"Temp":"$.t"}}`, `{"metrics":{"t":"t"}}`, `{"status":"$..x"}`,
		`{"metrics":{"t":"$.t"},"units":{"x":"c"}}`, `{"metrics":{"t":"$.t"},"missed_count":0}`, `{"metrics":{"t":"$.t"},"other":1}`} {
		err := plugin.ValidateConfig(&HTTP{}, json.RawMessage(raw))
		if err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if got := MissedCount(json.RawMessage(`{"metrics":{"t":"$.t"},"missed_count":5}`)); got != 5 {
		t.Errorf("missed count %d", got)
	}
}

// FuzzParse: no body crashes the parser or makes it misbehave (F08).
func FuzzParse(f *testing.F) {
	c := json.RawMessage(`{"metrics":{"a":"$.a","b":"$.x[2].y"},"status":"$.s","output":"$.o","timestamp":"$.t"}`)
	for _, s := range []string{`{"a":1,"s":"ok","t":1}`, `[{"a":"2","s":2,"t":"2026-10-07T00:00:00Z"}]`, `{"x":[1,2,{"y":true}],"s":true,"t":1e12}`, `[`, `null`} {
		f.Add([]byte(s))
	}
	p := &HTTP{}
	f.Fuzz(func(t *testing.T, body []byte) {
		rs, err := p.Parse(c, body)
		if err != nil {
			return
		}
		if len(rs) == 0 || len(rs) > MaxBatch {
			t.Fatalf("%d results", len(rs))
		}
		for _, r := range rs {
			if len(r.Metrics) != 2 || len(r.Output) > maxOutputLen || r.Status < plugin.OK || r.Status > plugin.Unknown {
				t.Fatalf("bad result %+v", r)
			}
		}
	})
}
