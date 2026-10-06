package xapi

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
	"github.com/plusclouds/monitoring.server/pkg/plugin/plugintest"
)

// fakePool serves the XAPI JSON-RPC calls xapi.pool makes and rrd_updates,
// in the wire formats of the XAPI documentation (JSON-RPC 2.0, ints as
// strings or numbers, rrd_updates JSON with string values). The master and
// the member are two TLS servers; host records point at them.
type fakePool struct {
	master, member *httptest.Server
	password       string
	logouts        int
}

func newFakePool(t *testing.T) *fakePool {
	f := &fakePool{password: "pool-secret"}
	f.master = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(w, r, true) }))
	f.member = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { f.serve(w, r, false) }))
	t.Cleanup(f.master.Close)
	t.Cleanup(f.member.Close)
	return f
}

func addr(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "https://") }

func (f *fakePool) serve(w http.ResponseWriter, r *http.Request, master bool) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/rrd_updates" {
		if r.URL.Query().Get("session_id") != "OpaqueRef:session-1" || r.URL.Query().Get("json") != "true" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(f.rrd(master)))
		return
	}
	var req struct {
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
		ID     any               `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	reply := func(result any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "result": result, "id": req.ID})
	}
	fail := func(code string, data ...string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
			"error": map[string]any{"code": 1, "message": code, "data": data}})
	}
	if !master && req.Method == "session.login_with_password" {
		fail("HOST_IS_SLAVE", addr(f.master))
		return
	}
	switch req.Method {
	case "session.login_with_password":
		var pass string
		_ = json.Unmarshal(req.Params[1], &pass)
		if pass != f.password {
			fail("SESSION_AUTHENTICATION_FAILED", "root", "Authentication failure")
			return
		}
		reply("OpaqueRef:session-1")
	case "session.logout":
		f.logouts++
		reply("")
	case "pool.get_all_records":
		reply(map[string]any{"OpaqueRef:pool": map[string]any{"name_label": "dc1", "master": "OpaqueRef:h1", "ha_enabled": true}})
	case "host.get_all_records":
		reply(map[string]any{
			"OpaqueRef:h1": map[string]any{"uuid": "11111111-aaaa-4000-8000-000000000001", "name_label": "xcp-01", "address": addr(f.master),
				"enabled": true, "metrics": "OpaqueRef:hm1", "software_version": map[string]string{"product_brand": "XCP-ng", "product_version": "8.3.0"}},
			"OpaqueRef:h2": map[string]any{"uuid": "22222222-aaaa-4000-8000-000000000002", "name_label": "xcp-02", "address": addr(f.member),
				"enabled": false, "metrics": "OpaqueRef:hm2", "software_version": map[string]string{"product_brand": "XCP-ng", "product_version": "8.3.0"}},
		})
	case "host_metrics.get_all_records":
		reply(map[string]any{
			"OpaqueRef:hm1": map[string]any{"live": true, "memory_total": "68719476736", "memory_free": "17179869184"}, // ints as strings
			"OpaqueRef:hm2": map[string]any{"live": true, "memory_total": 68719476736, "memory_free": 34359738368},     // and as numbers
		})
	case "VM.get_all_records":
		snap := time.Now().Add(-72 * time.Hour).UTC().Format("20060102T15:04:05Z")
		reply(map[string]any{
			"OpaqueRef:dom0": map[string]any{"uuid": "dom0", "name_label": "Control domain", "is_control_domain": true, "power_state": "Running"},
			"OpaqueRef:tmpl": map[string]any{"uuid": "tmpl", "name_label": "Debian 12", "is_a_template": true, "power_state": "Halted"},
			"OpaqueRef:web": map[string]any{"uuid": "33333333-bbbb-4000-8000-000000000003", "name_label": "web", "power_state": "Running",
				"resident_on": "OpaqueRef:h1", "guest_metrics": "OpaqueRef:gm-web", "snapshots": []string{"OpaqueRef:web-snap"},
				"VCPUs_max": "2", "memory_static_max": "4294967296"},
			"OpaqueRef:web-snap": map[string]any{"uuid": "snap", "name_label": "web before upgrade", "is_a_snapshot": true,
				"power_state": "Halted", "snapshot_time": snap},
			"OpaqueRef:db": map[string]any{"uuid": "44444444-bbbb-4000-8000-000000000004", "name_label": "db", "power_state": "Running",
				"resident_on": "OpaqueRef:h2", "guest_metrics": "OpaqueRef:NULL", "snapshots": []string{}, "VCPUs_max": 4},
			"OpaqueRef:old": map[string]any{"uuid": "55555555-bbbb-4000-8000-000000000005", "name_label": "old", "power_state": "Halted",
				"resident_on": "OpaqueRef:NULL", "guest_metrics": "OpaqueRef:NULL", "snapshots": []string{}},
		})
	case "VM_guest_metrics.get_all_records":
		reply(map[string]any{"OpaqueRef:gm-web": map[string]any{"os_version": map[string]string{"name": "Debian GNU/Linux 12"},
			"PV_drivers_detected": true}})
	case "SR.get_all_records":
		reply(map[string]any{
			"OpaqueRef:sr1": map[string]any{"uuid": "66666666-cccc-4000-8000-000000000006", "name_label": "Local storage", "type": "ext",
				"shared": false, "physical_size": "100000000000", "physical_utilisation": "40000000000"},
			"OpaqueRef:iso": map[string]any{"uuid": "iso", "name_label": "ISOs", "type": "iso", "physical_size": "0"},
		})
	default:
		fail("MESSAGE_METHOD_UNKNOWN", req.Method)
	}
}

// rrd: two rows; the newest has NaN for a source, which falls back to the
// older row.
func (f *fakePool) rrd(master bool) string {
	now := time.Now().Unix()
	if master {
		return `{"meta":{"start":` + itoa(now-120) + `,"step":60,"end":` + itoa(now) + `,"rows":2,"columns":10,
		  "legend":["AVERAGE:host:11111111-aaaa-4000-8000-000000000001:cpu_avg","AVERAGE:host:11111111-aaaa-4000-8000-000000000001:pif_eth0_rx",
		    "AVERAGE:host:11111111-aaaa-4000-8000-000000000001:pif_eth0_tx","AVERAGE:vm:33333333-bbbb-4000-8000-000000000003:cpu0",
		    "AVERAGE:vm:33333333-bbbb-4000-8000-000000000003:cpu1","AVERAGE:vm:33333333-bbbb-4000-8000-000000000003:memory",
		    "AVERAGE:vm:33333333-bbbb-4000-8000-000000000003:memory_internal_free","AVERAGE:vm:33333333-bbbb-4000-8000-000000000003:vif_0_rx",
		    "AVERAGE:vm:33333333-bbbb-4000-8000-000000000003:vbd_xvda_read","AVERAGE:vm:33333333-bbbb-4000-8000-000000000003:vbd_xvda_iops_total"],
		  "data":[{"t":` + itoa(now) + `,"values":["2.5000E-01","1000","2000","NaN","1.0000E-01","4294967296","1048576","100","500","7"]},
		          {"t":` + itoa(now-60) + `,"values":["0.9","9","9","0.5","0.9","1","1","1","1","1"]}]}}`
	}
	return `{"meta":{"start":0,"step":60,"end":0,"rows":1,"columns":2,
	  "legend":["AVERAGE:host:22222222-aaaa-4000-8000-000000000002:cpu_avg","AVERAGE:vm:44444444-bbbb-4000-8000-000000000004:cpu0"],
	  "data":[{"t":` + itoa(now) + `,"values":["0.1","0.3"]}]}}`
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func creds(pass string) map[string]plugin.Credential {
	return map[string]plugin.Credential{"auth": {Type: "xapi", Fields: map[string]string{"username": "root"},
		Secret: map[string]plugin.Secret{"password": plugin.Secret(pass)}}}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestPool(t *testing.T) {
	f := newFakePool(t)
	r := plugintest.Collect(t, &Pool{}, plugintest.Options{Address: addr(f.master), Credentials: creds(f.password)})
	if r.Status != plugin.OK || !strings.HasPrefix(r.Output, "pool dc1: 2 hosts, 3 VMs (2 running), HA on") {
		t.Fatalf("collect: %v %q", r.Status, r.Output)
	}
	objs := map[string]plugin.Object{}
	for _, o := range r.Objects {
		objs[o.Key] = o
	}
	h1, h2 := objs["host:11111111-aaaa-4000-8000-000000000001"], objs["host:22222222-aaaa-4000-8000-000000000002"]
	if h1.Status != plugin.OK || h1.Labels["master"] != "true" || !near(h1.Metrics[mCPU], 25) || !near(h1.Metrics[mMemory], 75) ||
		h1.Metrics[mNetRx] != 8000 || h1.Metrics[mNetTx] != 16000 || h1.Device != h1.Key {
		t.Errorf("master host: %+v", h1)
	}
	if h2.Status != plugin.Warning || !strings.Contains(h2.Output, "maintenance") || !near(h2.Metrics[mMemory], 50) || !near(h2.Metrics[mCPU], 10) {
		t.Errorf("disabled host: %+v", h2)
	}
	web := objs["vm:33333333-bbbb-4000-8000-000000000003"]
	// cpu0 is NaN in the newest row: the older row's 0.5 is used; mean (0.5 + 0.1) / 2.
	if web.Status != plugin.OK || !near(web.Metrics[mCPU], 30) || !near(web.Metrics[mMemory], 75) || web.Metrics[mNetRx] != 800 ||
		web.Metrics[mDiskRead] != 500 || web.Metrics[mDiskIOPS] != 7 || web.Metrics[mSnapshots] != 1 ||
		web.Metrics[mSnapshotAge] < 2.9 || web.Metrics[mSnapshotAge] > 3.1 || web.Labels["guest_tools"] != "true" {
		t.Errorf("web VM: %+v", web)
	}
	db := objs["vm:44444444-bbbb-4000-8000-000000000004"]
	if !strings.Contains(db.Output, "no guest tools") || !near(db.Metrics[mCPU], 30) || db.Labels["vcpus"] != "4" {
		t.Errorf("db VM: %+v", db)
	}
	if old := objs["vm:55555555-bbbb-4000-8000-000000000005"]; old.Status != plugin.OK || old.Output != "halted" {
		t.Errorf("halted VM: %+v", old)
	}
	sr := objs["sr:66666666-cccc-4000-8000-000000000006"]
	if sr.Metrics[mUsed] != 40 || sr.Metrics[mSize] != 1e11 || sr.Device != "" {
		t.Errorf("SR: %+v", sr)
	}
	if _, ok := objs["sr:iso"]; ok || len(objs) != 6 {
		t.Errorf("objects: %v", objs)
	}

	inv := r.Inventory
	if inv == nil || len(inv.Children) != 5 || inv.Device == nil || inv.Device.Vendor != "XCP-ng" {
		t.Fatalf("inventory: %+v", inv)
	}
	parent := map[string]string{}
	for _, c := range inv.Children {
		parent[c.Key] = c.ParentKey
	}
	if parent["vm:33333333-bbbb-4000-8000-000000000003"] != h1.Key || parent["vm:44444444-bbbb-4000-8000-000000000004"] != h2.Key ||
		parent["vm:55555555-bbbb-4000-8000-000000000005"] != "" || parent[h1.Key] != "" {
		t.Errorf("parents: %v", parent)
	}
	if f.logouts != 1 {
		t.Errorf("logouts = %d", f.logouts)
	}
}

func TestPoolFollowsMaster(t *testing.T) {
	f := newFakePool(t)
	r := plugintest.Collect(t, &Pool{}, plugintest.Options{Address: addr(f.member), Credentials: creds(f.password)})
	if r.Status != plugin.OK || len(r.Objects) != 6 {
		t.Fatalf("via a member: %v %q", r.Status, r.Output)
	}
}

func TestPoolFailures(t *testing.T) {
	f := newFakePool(t)
	r := plugintest.Collect(t, &Pool{}, plugintest.Options{Address: addr(f.master), Credentials: creds("wrong-password")})
	if r.Status != plugin.Unknown || r.Objects != nil || !strings.Contains(r.Output, "rejected the login") {
		t.Errorf("wrong password: %v %q", r.Status, r.Output)
	}
	r = plugintest.Collect(t, &Pool{}, plugintest.Options{Address: "127.0.0.1:1", Credentials: creds("x"), Timeout: 3 * time.Second})
	if r.Status != plugin.Critical || r.Objects != nil {
		t.Errorf("unreachable: %v %q", r.Status, r.Output)
	}
	r = plugintest.Collect(t, &Pool{}, plugintest.Options{Address: addr(f.master)})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "no XAPI credential") {
		t.Errorf("no credential: %v %q", r.Status, r.Output)
	}
	r = plugintest.Collect(t, &Pool{}, plugintest.Options{Address: addr(f.master), Credentials: creds(f.password),
		Network: &plugin.NetPolicy{DenyPrivate: true}})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "private or local") {
		t.Errorf("policy: %v %q", r.Status, r.Output)
	}
	r = plugintest.Collect(t, &Pool{}, plugintest.Options{Address: addr(f.master), Credentials: creds(f.password),
		Config: Config{VerifyCertificate: true}})
	if r.Status == plugin.OK {
		t.Errorf("self-signed certificate accepted with verify_certificate: %q", r.Output)
	}
}
