package api_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/engine"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/internal/webhook"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// noise is a tenant with a webhook receiver, an engine to feed results and a
// notifier ticked by hand.
type noise struct {
	*env
	key    string
	tenant uuid.UUID
	rcv    *receiver
	hook   string
	eng    *engine.Engine
	n      *webhook.Notifier
}

func newNoise(t *testing.T, grace time.Duration, tune ...func(*config.Notifier)) *noise {
	t.Helper()
	e := setup(t, true)
	if _, err := db.System.Exec(context.Background(),
		`UPDATE tenants SET allowed_target_networks = '{127.0.0.0/8}' WHERE NOT is_platform`); err != nil {
		t.Fatal(err)
	}
	x := &noise{env: e, key: e.adminKey, rcv: newReceiver(t)}
	x.tenant = uuid.MustParse(e.must(e.do("GET", "/v1/tenant", x.key, nil), 200).body["id"].(string))
	x.hook = e.id(e.must(e.do("POST", "/v1/webhooks", x.key, map[string]any{"name": "h", "url": x.rcv.URL}), 201))
	var err error
	if x.eng, err = engine.New(engine.Options{System: db.System, Logger: slog.New(slog.DiscardHandler)}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Notifier.DependencyGrace = config.Duration(grace)
	for _, f := range tune {
		f(&cfg.Notifier)
	}
	if x.n, err = webhook.New(webhook.Options{System: db.System, Store: &webhook.Store{Keys: sharedKeys},
		Sender: webhook.NewSender(cfg), Config: cfg.Notifier, Logger: slog.New(slog.DiscardHandler),
		Registry: prometheus.NewRegistry()}); err != nil {
		t.Fatal(err)
	}
	return x
}

func (x *noise) device(name string) string {
	return x.id(x.must(x.do("POST", "/v1/devices", x.key, map[string]any{"name": name, "type": "server"}), 201))
}

func (x *noise) check(dev, name, plug string, host bool) string {
	return x.id(x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, map[string]any{
		"name": name, "plugin": plug, "is_host_check": host, "failure_count": 1, "recovery_count": 1}), 201))
}

func (x *noise) result(dev, chk, plug string, st plugin.Status) {
	x.t.Helper()
	if _, err := x.eng.Apply(context.Background(), runner.Result{TenantID: x.tenant, DeviceID: uuid.MustParse(dev),
		CheckID: uuid.MustParse(chk), Plugin: plug, Interval: time.Minute,
		Result: plugin.Result{Status: st, Output: st.String(), Time: time.Now()}}); err != nil {
		x.t.Fatal(err)
	}
}

// tick runs the notifier until nothing is left to send right now.
func (x *noise) tick() {
	for range 3 {
		x.n.Tick(context.Background())
	}
}

func (x *noise) incident(chk string) map[string]any {
	x.t.Helper()
	l := x.must(x.do("GET", "/v1/incidents?status=active&check_id="+chk, x.key, nil), 200).body["items"].([]any)
	if len(l) != 1 {
		x.t.Fatalf("active incidents of %s: %v", chk, l)
	}
	return l[0].(map[string]any)
}

// M4 (F05): a server behind a failed switch is suppressed, also when its own
// check fails first; when the switch recovers, the server's host check is
// notified and its other checks stay suppressed under it.
func TestDependencySuppression(t *testing.T) {
	x := newNoise(t, 300*time.Millisecond)
	x.must(x.do("POST", "/v1/alert-routes", x.key, map[string]any{"name": "all", "endpoint_id": x.hook}), 201)
	sw, srv := x.device("switch"), x.device("server")
	x.must(x.do("POST", "/v1/devices/"+srv+"/dependencies", x.key, map[string]any{"depends_on_id": sw}), 201)
	swPing := x.check(sw, "ping", "icmp", true)
	srvPing := x.check(srv, "ping", "icmp", true)
	srvWeb := x.check(srv, "web", "http", false)

	// The server's web check fails before the switch's ping: it waits.
	x.result(srv, srvWeb, "http", plugin.Critical)
	x.tick()
	if got := x.rcv.types(); len(got) != 0 {
		t.Fatalf("notified before the grace: %v", got)
	}
	x.result(sw, swPing, "icmp", plugin.Critical)
	x.tick()
	x.rcv.wait(t, "monitoring.incident.opened", 2*time.Second)
	time.Sleep(400 * time.Millisecond)
	x.tick()
	x.result(srv, srvPing, "icmp", plugin.Critical) // suppressed when opened
	time.Sleep(400 * time.Millisecond)
	x.tick()
	if got := x.rcv.types(); len(got) != 1 {
		t.Fatalf("want only the switch notified, got %v", got)
	}
	if l := x.must(x.do("GET", "/v1/incidents?suppressed=true", x.key, nil), 200).body["items"].([]any); len(l) != 2 {
		t.Errorf("suppressed filter: %d incidents, want 2", len(l))
	}
	swInc := x.incident(swPing)["id"]
	for _, c := range []string{srvWeb, srvPing} {
		if i := x.incident(c); i["suppressed"] != true || i["root_incident_id"] != swInc || i["root_device_id"] != sw {
			t.Errorf("server incident %s: %v", c, i)
		}
	}

	// The switch recovers; the server is still down.
	x.result(sw, swPing, "icmp", plugin.OK)
	x.tick()
	time.Sleep(400 * time.Millisecond)
	x.tick()
	deadline := time.Now().Add(2 * time.Second)
	for len(x.rcv.types()) < 3 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		x.tick()
	}
	// Deliveries of different incidents may arrive in any order.
	got := x.rcv.types()
	slices.Sort(got)
	if !slices.Equal(got, []string{"monitoring.incident.opened", "monitoring.incident.opened",
		"monitoring.incident.resolved"}) {
		t.Fatalf("after the switch recovered: %v", got)
	}
	x.rcv.mu.Lock()
	var checks []string
	for _, m := range x.rcv.msgs {
		var b struct {
			Data struct{ Check struct{ ID string } } `json:"data"`
		}
		_ = json.Unmarshal(m.body, &b)
		if m.typ == "monitoring.incident.opened" {
			checks = append(checks, b.Data.Check.ID)
		}
	}
	x.rcv.mu.Unlock()
	if !slices.Contains(checks, swPing) || !slices.Contains(checks, srvPing) {
		t.Errorf("opened for %v, want the switch ping and the server ping", checks)
	}
	ping := x.incident(srvPing)
	if ping["suppressed"] != false || ping["root_device_id"] != srv {
		t.Errorf("server ping after release: %v", ping)
	}
	if web := x.incident(srvWeb); web["suppressed"] != true || web["root_incident_id"] != ping["id"] {
		t.Errorf("server web should now hang under the server's ping: %v", web)
	}
}

// M4 (F06): a grouping route sends many incidents as one event, and a
// lone incident as the plain event.
func TestRouteGrouping(t *testing.T) {
	x := newNoise(t, 0)
	r := x.must(x.do("POST", "/v1/alert-routes", x.key, map[string]any{"name": "grouped", "endpoint_id": x.hook,
		"group_by": []string{"severity"}, "group_wait_seconds": 0}), 201)
	if gb := r.body["group_by"].([]any); len(gb) != 1 || r.body["group_wait_seconds"] != float64(0) {
		t.Fatalf("route: %s", r.raw)
	}
	for _, name := range []string{"a", "b", "c"} {
		d := x.device(name)
		x.result(d, x.check(d, "web", "http", false), "http", plugin.Critical)
	}
	x.tick()
	m := x.rcv.wait(t, "monitoring.incident.opened", 2*time.Second)
	var body struct {
		Data struct {
			Object  any               `json:"object"`
			Objects []json.RawMessage `json:"objects"`
			Group   struct {
				Count int            `json:"count"`
				Key   map[string]any `json:"key"`
			} `json:"group"`
			Route struct{ Name string } `json:"route"`
		} `json:"data"`
	}
	if err := json.Unmarshal(m.body, &body); err != nil {
		t.Fatal(err)
	}
	if len(x.rcv.types()) != 1 || len(body.Data.Objects) != 3 || body.Data.Group.Count != 3 ||
		body.Data.Group.Key["severity"] != "critical" || body.Data.Object != nil || body.Data.Route.Name != "grouped" {
		t.Fatalf("grouped event: %s (received %v)", m.body, x.rcv.types())
	}

	d := x.device("d")
	x.result(d, x.check(d, "web", "http", false), "http", plugin.Warning)
	x.tick()
	deadline := time.Now().Add(2 * time.Second)
	for len(x.rcv.types()) < 2 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		x.tick()
	}
	x.rcv.mu.Lock()
	last := x.rcv.msgs[len(x.rcv.msgs)-1].body
	x.rcv.mu.Unlock()
	var single struct {
		Data struct {
			Object  any               `json:"object"`
			Objects []json.RawMessage `json:"objects"`
		} `json:"data"`
	}
	if err := json.Unmarshal(last, &single); err != nil {
		t.Fatal(err)
	}
	if single.Data.Object == nil || single.Data.Objects != nil {
		t.Errorf("a group of one should be the plain event: %s", last)
	}
}

// M4 (F06): open, unacknowledged incidents are re-sent every
// repeat_interval; acknowledging stops it.
func TestRepeatInterval(t *testing.T) {
	x := newNoise(t, 0)
	x.must(x.do("POST", "/v1/alert-routes", x.key, map[string]any{"name": "repeat", "endpoint_id": x.hook,
		"repeat_interval_seconds": 300}), 201)
	x.must(x.do("POST", "/v1/alert-routes", x.key, map[string]any{"name": "bad", "endpoint_id": x.hook,
		"repeat_interval_seconds": 10}), 400)
	d := x.device("web")
	chk := x.check(d, "web", "http", false)
	x.result(d, chk, "http", plugin.Critical)
	x.tick()
	x.rcv.wait(t, "monitoring.incident.opened", 2*time.Second)
	x.tick()
	if got := x.rcv.types(); len(got) != 1 {
		t.Fatalf("repeated too early: %v", got)
	}
	age := func() {
		if _, err := db.System.Exec(context.Background(),
			`UPDATE route_notifications SET last_sent_at = now() - interval '10 minutes'`); err != nil {
			t.Fatal(err)
		}
	}
	age()
	x.tick()
	x.rcv.wait(t, "monitoring.incident.renotify", 2*time.Second)

	x.must(x.do("POST", "/v1/incidents/"+x.incident(chk)["id"].(string)+"/ack", x.key, nil), 200)
	x.tick()
	n := len(x.rcv.types())
	age()
	x.tick()
	time.Sleep(200 * time.Millisecond)
	x.tick()
	if got := x.rcv.types(); len(got) != n {
		t.Errorf("acknowledged incident repeated: %v", got)
	}
}
