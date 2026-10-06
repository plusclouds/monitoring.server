package snmp

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
	"github.com/plusclouds/monitoring.server/pkg/plugin/plugintest"
)

// The simulator (snmpsim, as F10 asks) serves each testdata/<name>.snmprec
// file as v2c community <name> and as SNMPv3 context <name>.
const (
	simImage = "tandrup/snmpsim:latest"
	v3User   = "monitor"
	v3Auth   = "auth-secret-1"
	v3Priv   = "priv-secret-1"
)

var (
	simOnce sync.Once
	simAddr string
	simErr  error
)

// agent returns the simulator's address, starting it on first use. Without
// Docker the test is skipped, unless MONITOR_REQUIRE_DB=1 (CI).
func agent(t *testing.T) string {
	t.Helper()
	simOnce.Do(func() { simAddr, simErr = startSim() })
	if simErr != nil {
		if os.Getenv("MONITOR_REQUIRE_DB") == "1" {
			t.Fatalf("snmpsim: %v", simErr)
		}
		t.Skipf("snmpsim not available: %v", simErr)
	}
	return simAddr
}

func startSim() (string, error) {
	ctx := context.Background()
	files, err := filepath.Glob("testdata/*.snmprec")
	if err != nil {
		return "", err
	}
	var copies []testcontainers.ContainerFile
	for _, f := range files {
		copies = append(copies, testcontainers.ContainerFile{
			HostFilePath: f, ContainerFilePath: "/data/" + filepath.Base(f), FileMode: 0o644,
		})
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        simImage,
			ExposedPorts: []string{"161/udp"},
			Files:        copies,
			Cmd: []string{"/bin/sh", "-c", "snmpsimd.py --data-dir=/data --agent-udpv4-endpoint=0.0.0.0:161 " +
				"--process-user=snmpsim --process-group=nogroup --v3-user=" + v3User +
				" --v3-auth-key=" + v3Auth + " --v3-auth-proto=SHA256 --v3-priv-key=" + v3Priv + " --v3-priv-proto=AES"},
			WaitingFor: wait.ForLog("Listening at").WithStartupTimeout(time.Minute),
		},
		Started: true,
	})
	if err != nil {
		return "", err
	}
	// Removed by the testcontainers reaper when the test binary exits.
	host, err := c.Host(ctx)
	if err != nil {
		return "", err
	}
	port, err := c.MappedPort(ctx, "161/udp")
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%s", host, port.Port()), nil
}

func v2c(community string) map[string]plugin.Credential {
	return map[string]plugin.Credential{"auth": {Type: "snmp_v2c", Secret: map[string]plugin.Secret{"community": plugin.Secret(community)}}}
}

func v3cred(context, auth string) map[string]plugin.Credential {
	return map[string]plugin.Credential{"auth": {Type: "snmp_v3",
		Fields: map[string]string{"username": v3User, "security_level": "authPriv", "auth_protocol": "SHA-256",
			"priv_protocol": "AES", "context_name": context},
		Secret: map[string]plugin.Secret{"auth_password": plugin.Secret(auth), "priv_password": v3Priv}}}
}

func run(t *testing.T, c plugin.Check, creds map[string]plugin.Credential, cfg any, state plugin.StateStore) plugin.Result {
	t.Helper()
	return plugintest.Run(t, c, plugintest.Options{Address: agent(t), Config: cfg, Credentials: creds,
		State: state, Timeout: 10 * time.Second})
}

func near(got, want float64) bool { return math.Abs(got-want) < 0.01 }

func TestSystemProfiles(t *testing.T) {
	for _, tc := range []struct {
		community        string
		uptime, cpu, mem float64
		profile          string
	}{
		{"pub-linux", 86400, 20, 50, "net-snmp"}, // hrSystemUptime; UCD memory beats hrStorage's 90 %
		{"pub-cisco", 5000, 35, 30, "cisco"},     // busiest CPU; memory of the first row
		{"pub-fortinet", 5000, 5, 60, "fortinet"},
		{"pub-bare", 60, math.NaN(), math.NaN(), "host-resources"},
	} {
		t.Run(tc.community, func(t *testing.T) {
			r := run(t, &System{}, v2c(tc.community), nil, nil)
			if r.Status != plugin.OK || !strings.Contains(r.Output, "profile "+tc.profile) {
				t.Fatalf("got %v %q", r.Status, r.Output)
			}
			for i, want := range []float64{tc.uptime, tc.cpu, tc.mem} {
				if got := r.Metrics[i]; !(near(got, want) || math.IsNaN(got) && math.IsNaN(want)) {
					t.Errorf("metric %d = %v, want %v (%q)", i, got, want, r.Output)
				}
			}
		})
	}
}

// SNMPv3 authPriv with SHA-256 and AES, the default security.
func TestSystemV3(t *testing.T) {
	r := run(t, &System{}, v3cred("pub-cisco", v3Auth), nil, nil)
	if r.Status != plugin.OK || !near(r.Metrics[sCPU], 35) {
		t.Fatalf("v3: %v %q", r.Status, r.Output)
	}
	r = run(t, &System{}, v3cred("pub-cisco", "wrong-secret"), SystemConfig{Connection: Connection{TimeoutMS: 1000, Retries: new(0)}}, nil)
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "authentication") {
		t.Fatalf("wrong auth password: %v %q", r.Status, r.Output)
	}
}

func TestSystemRestart(t *testing.T) {
	st := &plugin.MemState{}
	saveSample(st, "uptime", sample{Value: 100000, At: time.Now().Add(-time.Minute)})
	r := run(t, &System{}, v2c("pub-bare"), nil, st)
	if !strings.Contains(r.Output, "restarted") {
		t.Errorf("restart not reported: %q", r.Output)
	}
	r = run(t, &System{}, v2c("pub-bare"), nil, st)
	if strings.Contains(r.Output, "restarted") {
		t.Errorf("restart reported twice: %q", r.Output)
	}
}

func TestNoAnswer(t *testing.T) {
	// snmpsim does not answer an unknown community, like a real agent.
	r := run(t, &System{}, v2c("no-such-community"), SystemConfig{Connection: Connection{TimeoutMS: 300, Retries: new(0)}}, nil)
	if r.Status != plugin.Critical || !strings.Contains(r.Output, "no SNMP response") {
		t.Fatalf("got %v %q", r.Status, r.Output)
	}
}

func TestGet(t *testing.T) {
	r := run(t, &Get{}, v2c("pub-linux"), GetConfig{OID: "1.3.6.1.4.1.2021.10.1.5.1", Scale: 0.01}, nil)
	if r.Status != plugin.OK || !near(r.Metrics[gValue], 0.42) || !math.IsNaN(r.Metrics[gRate]) {
		t.Fatalf("gauge: %v %q %v", r.Status, r.Output, r.Metrics)
	}
	r = run(t, &Get{}, v2c("pub-linux"), GetConfig{OID: ".1.3.6.1.2.1.1.1.0", Expect: "Linux", Match: "contains"}, nil)
	if r.Status != plugin.OK {
		t.Fatalf("contains: %v %q", r.Status, r.Output)
	}
	r = run(t, &Get{}, v2c("pub-linux"), GetConfig{OID: ".1.3.6.1.2.1.1.1.0", Expect: "^FreeBSD", Match: "regex"}, nil)
	if r.Status != plugin.Critical || !strings.Contains(r.Output, "expected regex") {
		t.Fatalf("regex mismatch: %v %q", r.Status, r.Output)
	}
	r = run(t, &Get{}, v2c("pub-linux"), GetConfig{OID: "1.3.6.1.4.1.2021.99.0"}, nil)
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "no object") {
		t.Fatalf("missing OID: %v %q", r.Status, r.Output)
	}
	r = run(t, &Get{}, v2c("pub-linux"), GetConfig{OID: "1.3.6.1.2.1.1.5.0", Kind: "counter"}, nil)
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "not a counter") {
		t.Fatalf("string as counter: %v %q", r.Status, r.Output)
	}
}

func TestGetCounterRate(t *testing.T) {
	st := &plugin.MemState{}
	cfg := GetConfig{OID: "1.3.6.1.2.1.2.2.1.10.1", Kind: "counter", Scale: 8}
	r := run(t, &Get{}, v2c("pub-linux"), cfg, st)
	if r.Status != plugin.OK || !math.IsNaN(r.Metrics[gRate]) || !strings.Contains(r.Output, "first sample") {
		t.Fatalf("first run: %v %q %v", r.Status, r.Output, r.Metrics)
	}
	time.Sleep(1500 * time.Millisecond)
	r = run(t, &Get{}, v2c("pub-linux"), cfg, st)
	// The simulated counter grows by 1,000 per second; scale 8 turns octets into bits.
	if r.Status != plugin.OK || r.Metrics[gRate] < 4000 || r.Metrics[gRate] > 12000 {
		t.Fatalf("second run: %v %q %v", r.Status, r.Output, r.Metrics)
	}
}

func TestUPS(t *testing.T) {
	r := run(t, &UPS{}, v2c("pub-ups"), nil, nil)
	want := []float64{95, 42, 37, 230, 229, 1, 1}
	if r.Status != plugin.Warning || !strings.Contains(r.Output, "on battery") ||
		!strings.Contains(r.Output, "battery needs replacing") || !strings.Contains(r.Output, "rfc1628") {
		t.Fatalf("rfc1628: %v %q", r.Status, r.Output)
	}
	for i, w := range want {
		if !near(r.Metrics[i], w) {
			t.Errorf("rfc1628 metric %d = %v, want %v", i, r.Metrics[i], w)
		}
	}

	r = run(t, &UPS{}, v2c("pub-apc"), nil, nil)
	want = []float64{12, 6, 50, 0, 230, 1, 0}
	if r.Status != plugin.Critical || !strings.Contains(r.Output, "battery low") || !strings.Contains(r.Output, "(apc)") {
		t.Fatalf("apc: %v %q", r.Status, r.Output)
	}
	for i, w := range want {
		if !near(r.Metrics[i], w) {
			t.Errorf("apc metric %d = %v, want %v", i, r.Metrics[i], w)
		}
	}

	r = run(t, &UPS{}, v2c("pub-bare"), nil, nil)
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "no rfc1628 UPS objects") {
		t.Fatalf("not a UPS: %v %q", r.Status, r.Output)
	}
}

func TestPolicyAndCredential(t *testing.T) {
	r := plugintest.Run(t, &System{}, plugintest.Options{Address: "127.0.0.1", Credentials: v2c("x"),
		Network: &plugin.NetPolicy{DenyPrivate: true}})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "private or local") {
		t.Fatalf("policy: %v %q", r.Status, r.Output)
	}
	r = plugintest.Run(t, &Get{}, plugintest.Options{Address: "127.0.0.1", Config: GetConfig{OID: "1.3.6.1.2.1.1.3.0"}})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "no SNMP credential") {
		t.Fatalf("no credential: %v %q", r.Status, r.Output)
	}
}

func TestValidate(t *testing.T) {
	for _, raw := range []string{`{"oid":"abc"}`, `{"oid":"1.3.6.1","match":"regex","expect":"("}`, `{"oid":"1.3.6.1","bogus":1}`} {
		if err := (&Get{}).Validate([]byte(raw)); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
	if err := (&Get{}).Validate([]byte(`{"oid":".1.3.6.1.2.1.1.3.0"}`)); err != nil {
		t.Error(err)
	}
	if err := (&System{}).Validate([]byte(`{"profile":"cisco","port":1161}`)); err != nil {
		t.Error(err)
	}
}

func TestPickProfile(t *testing.T) {
	for oid, want := range map[string]string{
		".1.3.6.1.4.1.9.1.1208":    "cisco",
		".1.3.6.1.4.1.99.1":        "host-resources", // 99 is not Cisco's 9
		".1.3.6.1.4.1.11.2.3.7.11": "hpe",
		".1.3.6.1.4.1.8072.3.2.10": "net-snmp",
		".1.3.6.1.4.1.14988.1":     "mikrotik",
		"":                         "host-resources",
	} {
		if got := pickProfile("auto", oid).name; got != want {
			t.Errorf("%s: %s, want %s", oid, got, want)
		}
	}
	if got := pickProfile("juniper", ".1.3.6.1.4.1.9.1").name; got != "juniper" {
		t.Errorf("explicit profile ignored: %s", got)
	}
}

func TestRate(t *testing.T) {
	t0 := time.Now()
	at := func(s float64) time.Time { return t0.Add(time.Duration(s * float64(time.Second))) }
	wrap := math.Exp2(32)
	for _, tc := range []struct {
		name      string
		prev, cur sample
		max       float64
		want      float64
		ok        bool
	}{
		{"plain", sample{Value: 1000, Bits: 64, At: at(0)}, sample{Value: 7000, Bits: 64, At: at(60)}, 0, 100, true},
		{"32-bit wrap", sample{Value: wrap - 600, Bits: 32, At: at(0)}, sample{Value: 5400, Bits: 32, At: at(60)}, 0, 100, true},
		{"64-bit going back is a reset", sample{Value: 7000, Bits: 64, At: at(0)}, sample{Value: 10, Bits: 64, At: at(60)}, 0, 0, false},
		{"uptime went back", sample{Value: 1000, Bits: 32, At: at(0), Uptime: 5000}, sample{Value: 2000, Bits: 32, At: at(60), Uptime: 30}, 0, 0, false},
		{"wrap implausible above speed", sample{Value: 100, Bits: 32, At: at(0)}, sample{Value: 50, Bits: 32, At: at(60)}, 1000, 0, false},
		{"glitch", sample{Value: 0, Bits: 64, At: at(0)}, sample{Value: 1e9, Bits: 64, At: at(1)}, 1e6, 0, false},
		{"no time passed", sample{Value: 0, Bits: 64, At: at(0)}, sample{Value: 10, Bits: 64, At: at(0)}, 0, 0, false},
	} {
		got, ok := rate(tc.prev, tc.cur, tc.max)
		if ok != tc.ok || (ok && !near(got, tc.want)) {
			t.Errorf("%s: %v %v, want %v %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestRestartedWrap(t *testing.T) {
	if restarted(100, 160, 60) {
		t.Error("growing uptime is not a restart")
	}
	if !restarted(100000, 50, 60) {
		t.Error("uptime from 100000 s to 50 s is a restart")
	}
	// TimeTicks wrap after 497 days: 30 s before the wrap, 60 s later.
	if restarted(timeTicksWrap-30, 30, 60) {
		t.Error("TimeTicks wrap taken for a restart")
	}
}

func TestSplitAddress(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.0.1": "10.0.0.1:161", "10.0.0.1:1161": "10.0.0.1:1161", "sw1.example.com": "sw1.example.com:161",
		"2001:db8::1": "2001:db8::1:161", "[2001:db8::1]:162": "2001:db8::1:162",
	} {
		h, p, err := splitAddress(in, 0)
		if err != nil || fmt.Sprintf("%s:%d", h, p) != want {
			t.Errorf("%s: %s:%d %v, want %s", in, h, p, err, want)
		}
	}
}

func collect(t *testing.T, creds map[string]plugin.Credential, cfg any, state plugin.StateStore) plugin.Result {
	t.Helper()
	return plugintest.Collect(t, &Interfaces{}, plugintest.Options{Address: agent(t), Config: cfg, Credentials: creds,
		State: state, Timeout: 10 * time.Second})
}

func objectsByKey(r plugin.Result) map[string]plugin.Object {
	out := map[string]plugin.Object{}
	for _, o := range r.Objects {
		out[o.Key] = o
	}
	return out
}

func TestInterfaces(t *testing.T) {
	st := &plugin.MemState{}
	r := collect(t, v2c("pub-switch"), nil, st)
	objs := objectsByKey(r)
	if r.Status != plugin.OK || len(r.Objects) != 3 || r.Output != "3 interfaces: 2 up, 1 down (Gi1/0/2)" {
		t.Fatalf("first run: %v %q %v", r.Status, r.Output, objs)
	}
	up, down, vlan := objs["Gi1/0/1"], objs["Gi1/0/2"], objs["Vlan1"]
	if up.Status != plugin.OK || up.Labels["alias"] != "uplink" || up.Metrics[iSpeed] != 1e9 || up.Metrics[iOperStatus] != 1 ||
		!math.IsNaN(up.Metrics[iInBps]) {
		t.Errorf("Gi1/0/1: %+v", up)
	}
	if down.Status != plugin.Critical || down.Metrics[iOperStatus] != 2 || !strings.Contains(down.Output, "down") {
		t.Errorf("Gi1/0/2: %+v", down)
	}
	if vlan.Metrics[iSpeed] != 1e8 || vlan.Labels["if_type"] != "53" {
		t.Errorf("Vlan1: %+v", vlan)
	}

	time.Sleep(1500 * time.Millisecond)
	r = collect(t, v2c("pub-switch"), nil, st)
	objs = objectsByKey(r)
	// Simulated counters: Gi1/0/1 125,000 and 250,000 octets/s (64-bit),
	// Vlan1 1,000 and 2,000 octets/s (32-bit only).
	for _, c := range []struct {
		key  string
		slot int
		want float64
	}{{"Gi1/0/1", iInBps, 1e6}, {"Gi1/0/1", iOutBps, 2e6}, {"Vlan1", iInBps, 8000}, {"Vlan1", iOutBps, 16000}} {
		if got := objs[c.key].Metrics[c.slot]; !(got > c.want/2 && got < c.want*2) {
			t.Errorf("%s slot %d = %v, want about %v", c.key, c.slot, got, c.want)
		}
	}
	if got := objs["Gi1/0/1"].Metrics[iInErrors]; got != 0 {
		t.Errorf("in errors rate %v, want 0", got)
	}

	r = collect(t, v2c("pub-switch"), InterfacesConfig{AdminUpOnly: new(false), Exclude: "^Vlan"}, nil)
	objs = objectsByKey(r)
	if _, ok := objs["Vlan1"]; ok || len(objs) != 3 || objs["Gi1/0/3"].Status != plugin.OK ||
		objs["Gi1/0/3"].Output != "administratively down" {
		t.Errorf("filters: %v", objs)
	}
	r = collect(t, v2c("pub-switch"), InterfacesConfig{Types: []int{53}}, nil)
	if len(r.Objects) != 1 || r.Objects[0].Key != "Vlan1" {
		t.Errorf("type filter: %v", r.Objects)
	}

	r = collect(t, v2c("no-such-community"), InterfacesConfig{Connection: Connection{TimeoutMS: 300, Retries: new(0)}}, nil)
	if r.Status != plugin.Critical || r.Objects != nil {
		t.Errorf("unreachable: %v %q %v", r.Status, r.Output, r.Objects)
	}
}

func TestInterfacesV3(t *testing.T) {
	r := collect(t, v3cred("pub-switch", v3Auth), nil, nil)
	if r.Status != plugin.OK || len(r.Objects) != 3 {
		t.Fatalf("v3: %v %q", r.Status, r.Output)
	}
}

func TestOIDAfter(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{".1.3.6.10", ".1.3.6.9", true}, {".1.3.6.9", ".1.3.6.10", false}, {".1.3.6.1.1", ".1.3.6.1", true}, {".1.3", ".1.3", false}} {
		if got := oidAfter(c.a, c.b); got != c.want {
			t.Errorf("oidAfter(%s, %s) = %v", c.a, c.b, got)
		}
	}
}
