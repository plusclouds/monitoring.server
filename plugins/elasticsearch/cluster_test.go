package elasticsearch

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
	"github.com/plusclouds/monitoring.server/pkg/plugin/plugintest"
)

// fakeCluster serves the Elasticsearch endpoints elasticsearch.cluster reads,
// in their wire formats (_cat/indices values are strings, null when closed).
type fakeCluster struct {
	*httptest.Server
	status  string
	gcMS    float64
	reject  float64
	auth    func(r *http.Request) bool
	catPath string
}

func newFakeCluster(t *testing.T, tls bool) *fakeCluster {
	f := &fakeCluster{status: "green", gcMS: 100, reject: 10}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if f.auth != nil && !f.auth(r) {
			http.Error(w, `{"error":"security_exception"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/":
			_, _ = w.Write([]byte(`{"version":{"number":"8.15.2"}}`))
		case r.URL.Path == "/_cluster/health":
			if f.status == "nomaster" {
				http.Error(w, `{"error":"master_not_discovered_exception"}`, http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"cluster_name":"prod","status":"` + f.status + `","timed_out":false,"number_of_nodes":2,"number_of_data_nodes":2,
			  "active_primary_shards":10,"active_shards":18,"relocating_shards":1,"initializing_shards":0,"unassigned_shards":2,"number_of_pending_tasks":3}`))
		case strings.HasPrefix(r.URL.Path, "/_nodes/stats"):
			_, _ = w.Write([]byte(`{"nodes":{
			  "n1":{"name":"es-01","host":"10.0.0.1","ip":"10.0.0.1","roles":["master","data"],
			    "jvm":{"mem":{"heap_used_percent":71},"gc":{"collectors":{"young":{"collection_time_in_millis":5},"old":{"collection_time_in_millis":` + ftoa(f.gcMS) + `}}}},
			    "os":{"cpu":{"percent":35}},"process":{"cpu":{"percent":30}},
			    "fs":{"total":{"total_in_bytes":1000,"available_in_bytes":250}},
			    "thread_pool":{"search":{"rejected":` + ftoa(f.reject) + `},"write":{"rejected":0}}},
			  "n2":{"name":"es-02","host":"10.0.0.2","ip":"10.0.0.2","roles":["data"],
			    "jvm":{"mem":{"heap_used_percent":55}},"process":{"cpu":{"percent":12}},"fs":{"total":{"total_in_bytes":0,"available_in_bytes":0}}}}}`))
		case strings.HasPrefix(r.URL.Path, "/_cat/indices"):
			f.catPath = r.URL.EscapedPath()
			if r.URL.Query().Get("bytes") != "b" || r.URL.Query().Get("format") != "json" {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`[
			  {"index":"logs-2026.10.01","health":"green","status":"open","docs.count":"1000000","store.size":"5000000000"},
			  {"index":"logs-2026.10.02","health":"yellow","status":"open","docs.count":"2000","store.size":"4000000"},
			  {"index":"small","health":"red","status":"open","docs.count":"5","store.size":"1000"},
			  {"index":"closed-one","health":null,"status":"close","docs.count":null,"store.size":null},
			  {"index":".kibana_1","health":"green","status":"open","docs.count":"10","store.size":"100000"}]`))
		default:
			http.NotFound(w, r)
		}
	})
	if tls {
		f.Server = httptest.NewTLSServer(h)
	} else {
		f.Server = httptest.NewServer(h)
	}
	t.Cleanup(f.Close)
	return f
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

func byKey(r plugin.Result) map[string]plugin.Object {
	m := map[string]plugin.Object{}
	for _, o := range r.Objects {
		m[o.Key] = o
	}
	return m
}

func slot(t *testing.T, name string) int {
	t.Helper()
	for i, m := range (&Cluster{}).Manifest().Metrics {
		if m.Name == name {
			return i
		}
	}
	t.Fatalf("no metric %s", name)
	return -1
}

func TestCluster(t *testing.T) {
	f := newFakeCluster(t, false)
	state := &plugin.MemState{}
	opts := plugintest.Options{Address: f.URL, Config: map[string]any{"scheme": "http"}, State: state}

	r := plugintest.Collect(t, &Cluster{}, opts)
	if r.Status != plugin.OK {
		t.Fatalf("status = %v: %s", r.Status, r.Output)
	}
	o := byKey(r)
	for _, k := range []string{"cluster", "node:n1", "node:n2", "index:logs-2026.10.01", "index:logs-2026.10.02", "index:small", "index:closed-one"} {
		if _, ok := o[k]; !ok {
			t.Errorf("no object %s in %v", k, r.Objects)
		}
	}
	if _, ok := o["index:.kibana_1"]; ok {
		t.Error("system index reported without include_system_indices")
	}
	c := o["cluster"]
	if c.Name != "prod" || c.Metrics[slot(t, "nodes")] != 2 || c.Metrics[slot(t, "unassigned_shards")] != 2 ||
		c.Metrics[slot(t, "pending_tasks")] != 3 || c.Metrics[slot(t, "health_code")] != 0 {
		t.Errorf("cluster: %+v", c)
	}
	n1 := o["node:n1"]
	if n1.Name != "es-01" || n1.Metrics[slot(t, "heap_used_percent")] != 71 || n1.Metrics[slot(t, "cpu_percent")] != 35 ||
		n1.Metrics[slot(t, "disk_used_percent")] != 75 {
		t.Errorf("node n1: %+v", n1)
	}
	if !math.IsNaN(n1.Metrics[slot(t, "gc_old_ms_per_s")]) {
		t.Error("a rate needs two runs")
	}
	if n2 := o["node:n2"]; n2.Metrics[slot(t, "cpu_percent")] != 12 || !math.IsNaN(n2.Metrics[slot(t, "disk_used_percent")]) {
		t.Errorf("node n2 falls back to process CPU and has no disk: %+v", n2)
	}
	big, closed, red := o["index:logs-2026.10.01"], o["index:closed-one"], o["index:small"]
	if big.Metrics[slot(t, "docs")] != 1e6 || big.Metrics[slot(t, "store_bytes")] != 5e9 || big.Status != plugin.OK {
		t.Errorf("index: %+v", big)
	}
	if o["index:logs-2026.10.02"].Status != plugin.Warning || red.Status != plugin.Critical {
		t.Error("index health maps to status")
	}
	if closed.Output != "closed" || !math.IsNaN(closed.Metrics[slot(t, "docs")]) {
		t.Errorf("closed index: %+v", closed)
	}
	if r.Inventory == nil || r.Inventory.Device == nil || r.Inventory.Device.Firmware != "8.15.2" {
		t.Errorf("inventory = %+v", r.Inventory)
	}

	// A second run turns the counters into rates; a restarted node's lower
	// counters give none.
	f.gcMS, f.reject = 100, 10
	time2 := plugintest.Collect(t, &Cluster{}, opts)
	if v := byKey(time2)["node:n1"].Metrics[slot(t, "gc_old_ms_per_s")]; v != 0 {
		t.Errorf("gc rate with no new GC time = %v", v)
	}
	f.gcMS, f.reject = 50, 5
	time3 := plugintest.Collect(t, &Cluster{}, opts)
	if v := byKey(time3)["node:n1"].Metrics[slot(t, "rejected_per_s")]; !math.IsNaN(v) {
		t.Errorf("rate after a counter reset = %v", v)
	}
}

func TestClusterStatus(t *testing.T) {
	for status, want := range map[string]plugin.Status{"green": plugin.OK, "yellow": plugin.Warning, "red": plugin.Critical} {
		f := newFakeCluster(t, false)
		f.status = status
		r := plugintest.Collect(t, &Cluster{}, plugintest.Options{Address: f.URL, Config: map[string]any{"scheme": "http"}})
		if r.Status != want {
			t.Errorf("%s: status = %v, want %v (%s)", status, r.Status, want, r.Output)
		}
	}
}

func TestClusterIndexOptions(t *testing.T) {
	f := newFakeCluster(t, false)
	r := plugintest.Collect(t, &Cluster{}, plugintest.Options{Address: f.URL,
		Config: map[string]any{"scheme": "http", "max_indices": 2, "index_pattern": "logs-*,small", "include_system_indices": true}})
	o := byKey(r)
	n := 0
	for k := range o {
		if strings.HasPrefix(k, "index:") {
			n++
		}
	}
	if n != 2 || o["index:logs-2026.10.01"].Key == "" || o["index:logs-2026.10.02"].Key == "" {
		t.Errorf("max_indices keeps the 2 largest: %v", r.Objects)
	}
	if !strings.Contains(r.Output, "2 of 5 indices") {
		t.Errorf("output does not say indices were cut: %s", r.Output)
	}
	if f.catPath != "/_cat/indices/logs-%2A,small" && f.catPath != "/_cat/indices/logs-*,small" && f.catPath != "/_cat/indices/logs-%2A%2Csmall" {
		t.Errorf("index pattern sent as %q", f.catPath)
	}

	all := plugintest.Collect(t, &Cluster{}, plugintest.Options{Address: f.URL, Config: map[string]any{"scheme": "http", "include_system_indices": true}})
	if _, ok := byKey(all)["index:.kibana_1"]; !ok {
		t.Error("system index missing with include_system_indices")
	}

	none := plugintest.Collect(t, &Cluster{}, plugintest.Options{Address: f.URL, Config: map[string]any{"scheme": "http", "max_indices": 0}})
	if len(none.Objects) != 3 {
		t.Errorf("max_indices 0 reports the cluster and nodes only: %v", none.Objects)
	}
}

func TestClusterAuth(t *testing.T) {
	f := newFakeCluster(t, false)
	f.auth = func(r *http.Request) bool {
		u, p, ok := r.BasicAuth()
		return (ok && u == "monitor" && p == "es-secret") || r.Header.Get("Authorization") == "Bearer es-token"
	}
	cfg := map[string]any{"scheme": "http"}
	basic := plugin.Credential{Type: "http_basic", Fields: map[string]string{"username": "monitor"}, Secret: map[string]plugin.Secret{"password": "es-secret"}}
	bearer := plugin.Credential{Type: "http_bearer", Secret: map[string]plugin.Secret{"token": "es-token"}}
	for name, cred := range map[string]plugin.Credential{"basic": basic, "bearer": bearer} {
		r := plugintest.Collect(t, &Cluster{}, plugintest.Options{Address: f.URL, Config: cfg, Credentials: map[string]plugin.Credential{"auth": cred}})
		if r.Status != plugin.OK {
			t.Errorf("%s: %v %s", name, r.Status, r.Output)
		}
	}
	bad := basic
	bad.Secret = map[string]plugin.Secret{"password": "wrong"}
	if r := plugintest.Collect(t, &Cluster{}, plugintest.Options{Address: f.URL, Config: cfg, Credentials: map[string]plugin.Credential{"auth": bad}}); r.Status != plugin.Unknown ||
		!strings.Contains(r.Output, "rejected the login") {
		t.Errorf("wrong password: %v %s", r.Status, r.Output)
	}
	if r := plugintest.Collect(t, &Cluster{}, plugintest.Options{Address: f.URL, Config: cfg}); r.Status != plugin.Unknown {
		t.Errorf("no credential on a secured cluster: %v %s", r.Status, r.Output)
	}
}

func TestClusterTLS(t *testing.T) {
	f := newFakeCluster(t, true)
	addr := strings.TrimPrefix(f.URL, "https://")
	r := plugintest.Collect(t, &Cluster{}, plugintest.Options{Address: addr})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "certificate verification failed") {
		t.Errorf("self-signed certificate verified: %v %s", r.Status, r.Output)
	}
	r = plugintest.Collect(t, &Cluster{}, plugintest.Options{Address: addr, Config: map[string]any{"verify_certificate": false}})
	if r.Status != plugin.OK {
		t.Errorf("verify_certificate=false: %v %s", r.Status, r.Output)
	}
	r = plugintest.Collect(t, &Cluster{}, plugintest.Options{Address: f.URL, Config: map[string]any{"verify_certificate": false}})
	if r.Status != plugin.OK {
		t.Errorf("https URL as address: %v %s", r.Status, r.Output)
	}
}

func TestClusterFailures(t *testing.T) {
	f := newFakeCluster(t, false)
	f.status = "nomaster"
	cfg := map[string]any{"scheme": "http"}
	if r := plugintest.Collect(t, &Cluster{}, plugintest.Options{Address: f.URL, Config: cfg}); r.Status != plugin.Critical || r.Objects != nil {
		t.Errorf("no master: %v %s", r.Status, r.Output)
	}
	addr := strings.TrimPrefix(f.URL, "http://")
	f.Close()
	if r := plugintest.Collect(t, &Cluster{}, plugintest.Options{Address: addr, Config: cfg}); r.Status != plugin.Critical {
		t.Errorf("unreachable: %v %s", r.Status, r.Output)
	}
	if r := plugintest.Collect(t, &Cluster{}, plugintest.Options{Config: cfg}); r.Status != plugin.Unknown {
		t.Errorf("no address: %v %s", r.Status, r.Output)
	}
	g := newFakeCluster(t, false)
	if r := plugintest.Collect(t, &Cluster{}, plugintest.Options{Address: g.URL, Config: cfg, Network: &plugin.NetPolicy{DenyPrivate: true}}); r.Status != plugin.Unknown {
		t.Errorf("SSRF policy: %v %s", r.Status, r.Output)
	}
}

func TestValidate(t *testing.T) {
	c := &Cluster{}
	for _, bad := range []string{`{"scheme":"ftp"}`, `{"port":70000}`, `{"max_indices":1001}`, `{"max_indices":-1}`, `{"nope":1}`} {
		if err := c.Validate([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if err := c.Validate(nil); err != nil {
		t.Errorf("empty config: %v", err)
	}
}

func TestBaseURL(t *testing.T) {
	cfg := defaults
	for in, want := range map[string]string{
		"es.example.com":         "https://es.example.com:9200",
		"es.example.com:9243":    "https://es.example.com:9243",
		"http://10.0.0.1":        "http://10.0.0.1:9200",
		"https://es.example:443": "https://es.example:443",
		"::1":                    "https://[::1]:9200",
		"[2001:db8::1]:9200":     "https://[2001:db8::1]:9200",
	} {
		got, err := baseURL(in, cfg)
		if err != nil || got != want {
			t.Errorf("baseURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := baseURL("ftp://x", cfg); err == nil {
		t.Error("ftp URL accepted")
	}
}
