package api_test

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/internal/engine"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// F06: a device lists the routes that take its incidents, whether their
// webhooks work, and the deliveries about it.
func TestDeviceNotifications(t *testing.T) {
	e := setup(t, true)
	allowLoopback(t)
	k := e.adminKey
	rcv := newReceiver(t)

	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "web-1", "type": "server",
		"tags": map[string]string{"rack": "a12"}}), 201))
	chk := e.id(e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k,
		map[string]any{"name": "c", "plugin": "http", "failure_count": 1}), 201))
	tenant := e.must(e.do("GET", "/v1/tenant", k, nil), 200).body["id"].(string)
	eng, err := engine.New(engine.Options{System: db.System, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	feed := func(st plugin.Status) {
		t.Helper()
		if _, err := eng.Apply(context.Background(), runner.Result{
			TenantID: uuid.MustParse(tenant), DeviceID: uuid.MustParse(dev), CheckID: uuid.MustParse(chk), Plugin: "http",
			Interval: time.Minute, Result: plugin.Result{Status: st, Output: "x", Time: time.Now()},
		}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(query string) map[string]any {
		t.Helper()
		return e.must(e.do("GET", "/v1/devices/"+dev+"/notifications"+query, k, nil), 200).body
	}
	route := func(body map[string]any, name string) map[string]any {
		t.Helper()
		for _, r := range body["routes"].([]any) {
			if m := r.(map[string]any); m["name"] == name {
				return m
			}
		}
		t.Fatalf("no route %q in %v", name, body["routes"])
		return nil
	}

	// The catalog lists the notifications with example payloads.
	cat := e.must(e.do("GET", "/v1/notification-catalog", k, nil), 200).body
	var types []string
	for _, ev := range cat["events"].([]any) {
		m := ev.(map[string]any)
		types = append(types, m["type"].(string))
		if m["example"].(map[string]any)["type"] != m["type"] {
			t.Errorf("example of %v has type %v", m["type"], m["example"].(map[string]any)["type"])
		}
	}
	for _, want := range []string{"monitoring.incident.opened", "monitoring.incident.resolved", "monitoring.incident.escalated",
		"monitoring.tenant.device_limit_reached", "monitoring.webhook.test"} {
		if !slices.Contains(types, want) {
			t.Errorf("catalog lacks %s: %v", want, types)
		}
	}

	// No routes yet.
	if b := get(""); len(b["routes"].([]any)) != 0 || len(b["items"].([]any)) != 0 {
		t.Fatalf("empty setup: %v", b)
	}
	e.must(e.do("GET", "/v1/devices/"+uuid.NewString()+"/notifications", k, nil), 404)

	hook := e.must(e.do("POST", "/v1/webhooks", k, map[string]any{"name": "h", "url": rcv.URL,
		"external": map[string]any{"source": "plusclouds", "type": `PlusClouds\Monitoring\Channel`, "id": "ch-1"}}), 201).body["id"]
	off := e.must(e.do("POST", "/v1/webhooks", k, map[string]any{"name": "off", "url": rcv.URL, "enabled": false}), 201).body["id"]
	e.must(e.do("POST", "/v1/alert-routes", k, map[string]any{"name": "critical", "endpoint_id": hook,
		"match": map[string]any{"severity": []string{"critical"}, "tags": map[string]string{"rack": "a12"}}, "continue": true,
		"external": map[string]any{"source": "plusclouds", "type": `PlusClouds\Monitoring\ChannelRoute`, "id": "ch-1"}}), 201)
	e.must(e.do("POST", "/v1/alert-routes", k, map[string]any{"name": "paused", "endpoint_id": hook, "enabled": false}), 201)
	e.must(e.do("POST", "/v1/alert-routes", k, map[string]any{"name": "network", "endpoint_id": hook,
		"match": map[string]any{"device_types": []string{"network"}}}), 201)
	e.must(e.do("POST", "/v1/alert-routes", k, map[string]any{"name": "to-off", "endpoint_id": off}), 201)

	b := get("")
	r := route(b, "critical")
	if r["would_fire"] != true || r["reason"] != nil {
		t.Errorf("critical route: %v", r)
	}
	because := r["matches_because"].([]any)
	if !slices.Contains(because, any("severity:critical")) || !slices.Contains(because, any("tag:rack=a12")) {
		t.Errorf("matches_because = %v", because)
	}
	if x, _ := r["route_external"].(map[string]any); x["type"] != `PlusClouds\Monitoring\ChannelRoute` || x["id"] != "ch-1" {
		t.Errorf("route_external = %v", r["route_external"])
	}
	if x, _ := r["endpoint"].(map[string]any)["external"].(map[string]any); x["type"] != `PlusClouds\Monitoring\Channel` {
		t.Errorf("endpoint external = %v", r["endpoint"])
	}
	if m := route(b, "network"); m["route_external"] != nil || m["endpoint"].(map[string]any)["external"] == nil {
		t.Errorf("route without a reference: %v", m)
	}
	mc := r["matched_checks"].([]any)
	if len(mc) != 1 || mc[0].(map[string]any)["check_id"] != chk ||
		len(mc[0].(map[string]any)["severities"].([]any)) != 1 {
		t.Errorf("matched_checks = %v", mc)
	}
	for name, reason := range map[string]string{"paused": "route_disabled", "network": "no_match", "to-off": "endpoint_disabled"} {
		if m := route(b, name); m["would_fire"] != false || m["reason"] != reason {
			t.Errorf("route %s: would_fire=%v reason=%v, want %s", name, m["would_fire"], m["reason"], reason)
		}
	}

	// An incident is delivered and shows up under the device.
	feed(plugin.Critical)
	n := newNotifier(t)
	n.Tick(context.Background())
	b = get("")
	items := b["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items: %v", items)
	}
	d := items[0].(map[string]any)
	if d["event_type"] != "monitoring.incident.opened" || d["status"] != "delivered" || d["check_id"] != chk ||
		d["incident_id"] == nil || d["route_name"] != "critical" {
		t.Errorf("delivery: %v", d)
	}
	if x, _ := d["route_external"].(map[string]any); x["id"] != "ch-1" {
		t.Errorf("delivery route_external = %v", d["route_external"])
	}
	if x, _ := d["endpoint_external"].(map[string]any); x["type"] != `PlusClouds\Monitoring\Channel` {
		t.Errorf("delivery endpoint_external = %v", d["endpoint_external"])
	}
	data := d["payload"].(map[string]any)["data"].(map[string]any)
	if data["device"].(map[string]any)["name"] != "web-1" || data["route"].(map[string]any)["name"] != "critical" {
		t.Errorf("payload: %v", d["payload"])
	}
	if h := route(b, "critical")["health"].(map[string]any); h["last_status"] != "delivered" || h["failing"] != false {
		t.Errorf("health: %v", h)
	}

	// Filters and paging.
	feed(plugin.OK)
	n.Tick(context.Background())
	if got := get("?event_type=monitoring.incident.resolved")["items"].([]any); len(got) != 1 {
		t.Errorf("event_type filter: %v", got)
	}
	if got := get("?status=failed")["items"].([]any); len(got) != 0 {
		t.Errorf("status filter: %v", got)
	}
	p1 := get("?limit=1")
	if len(p1["items"].([]any)) != 1 || p1["next_cursor"] == nil {
		t.Fatalf("page 1: %v", p1)
	}
	p2 := get("?limit=1&cursor=" + p1["next_cursor"].(string))
	if len(p2["items"].([]any)) != 1 || len(p2["routes"].([]any)) != 0 ||
		p2["items"].([]any)[0].(map[string]any)["id"] == p1["items"].([]any)[0].(map[string]any)["id"] {
		t.Errorf("page 2: %v", p2)
	}
	if p2["next_cursor"] != nil {
		t.Errorf("page 2 has a next cursor: %v", p2["next_cursor"])
	}

	// A failing receiver turns the route off.
	rcv.code.Store(500)
	feed(plugin.Critical)
	n.Tick(context.Background())
	r = route(get(""), "critical")
	if h := r["health"].(map[string]any); h["failing"] != true || h["last_error"] == nil {
		t.Errorf("health after failure: %v", h)
	}
	if r["would_fire"] != false || r["reason"] != "endpoint_failing" {
		t.Errorf("route with a failing webhook: %v", r)
	}
}

// A route limited to devices (match.device_ids) takes only their incidents,
// in the device view and at delivery, and updating the list takes effect.
func TestRouteDeviceIDs(t *testing.T) {
	e := setup(t, true)
	allowLoopback(t)
	k := e.adminKey
	rcv := newReceiver(t)
	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "web-1", "type": "server"}), 201))
	chk := e.id(e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k,
		map[string]any{"name": "c", "plugin": "http", "failure_count": 1}), 201))
	tenant := e.must(e.do("GET", "/v1/tenant", k, nil), 200).body["id"].(string)
	eng, err := engine.New(engine.Options{System: db.System, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	hook := e.must(e.do("POST", "/v1/webhooks", k, map[string]any{"name": "h", "url": rcv.URL}), 201).body["id"]
	other := uuid.NewString()
	ext := map[string]any{"source": "plusclouds", "type": "t", "id": "r1"}
	view := func() map[string]any {
		t.Helper()
		for _, r := range e.must(e.do("GET", "/v1/devices/"+dev+"/notifications", k, nil), 200).body["routes"].([]any) {
			return r.(map[string]any)
		}
		t.Fatal("no routes")
		return nil
	}
	fire := func(st plugin.Status) {
		t.Helper()
		if _, err := eng.Apply(context.Background(), runner.Result{
			TenantID: uuid.MustParse(tenant), DeviceID: uuid.MustParse(dev), CheckID: uuid.MustParse(chk), Plugin: "http",
			Interval: time.Minute, Result: plugin.Result{Status: st, Output: "x", Time: time.Now()},
		}); err != nil {
			t.Fatal(err)
		}
	}
	save := func(ids []string) {
		t.Helper()
		body := map[string]any{"name": "only", "endpoint_id": hook, "external": ext,
			"match": map[string]any{"severity": []string{"critical"}, "device_ids": ids}}
		r := e.do("PUT", "/v1/alert-routes/by-external-id/r1?source=plusclouds&type=t", k, body)
		if r.status != 200 && r.status != 201 {
			t.Fatalf("save: %d %s", r.status, r.raw)
		}
	}

	save([]string{other})
	if r := view(); r["would_fire"] != false || r["reason"] != "no_match" {
		t.Errorf("route for another device: %v", r)
	}
	fire(plugin.Critical)
	n := newNotifier(t)
	n.Tick(context.Background())
	if got := rcv.types(); len(got) != 0 {
		t.Errorf("delivered for a device outside device_ids: %v", got)
	}
	fire(plugin.OK)
	n.Tick(context.Background()) // events are routed when the notifier ticks, with the routes of that moment

	save([]string{dev})
	r := view()
	if r["would_fire"] != true || !slices.Contains(r["matches_because"].([]any), any("device_id:"+dev)) {
		t.Errorf("route for this device: %v", r)
	}
	fire(plugin.Critical)
	n.Tick(context.Background())
	if got := rcv.types(); len(got) != 1 {
		t.Errorf("deliveries for a device in device_ids: %v", got)
	}

	save([]string{other})
	if r := view(); r["would_fire"] != false || r["reason"] != "no_match" {
		t.Errorf("route moved to another device: %v", r)
	}
}
