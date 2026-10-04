package api_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/internal/engine"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func status(r resp) map[string]any { return r.body["status"].(map[string]any) }

// v0.2.1: devices carry a rolled-up status from their host check and open
// incidents; lists filter by availability.
func TestDeviceStatus(t *testing.T) {
	e := setup(t, true)
	k := e.adminKey
	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "srv", "type": "server", "address": "10.0.0.1"}), 201))
	if st := status(e.must(e.do("GET", "/v1/devices/"+dev, k, nil), 200)); st["availability"] != "unmonitored" ||
		st["health"] != "ok" || st["since"] != nil {
		t.Fatalf("no host check: %v", st)
	}
	host := e.id(e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{
		"name": "ping", "plugin": "icmp", "is_host_check": true, "failure_count": 1}), 201))
	other := e.id(e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{
		"name": "web", "plugin": "http", "failure_count": 1}), 201))
	if st := status(e.must(e.do("GET", "/v1/devices/"+dev, k, nil), 200)); st["availability"] != "unknown" {
		t.Fatalf("before any result: %v", st)
	}

	eng, err := engine.New(engine.Options{System: db.System, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	tenant := uuid.MustParse(e.must(e.do("GET", "/v1/tenant", k, nil), 200).body["id"].(string))
	apply := func(check, plug string, st plugin.Status) {
		if _, err := eng.Apply(context.Background(), runner.Result{TenantID: tenant, DeviceID: uuid.MustParse(dev),
			CheckID: uuid.MustParse(check), Plugin: plug, Interval: time.Minute,
			Result: plugin.Result{Status: st, Output: "x", Time: time.Now()}}); err != nil {
			t.Fatal(err)
		}
	}
	apply(host, "icmp", plugin.OK)
	apply(other, "http", plugin.Warning)
	st := status(e.must(e.do("GET", "/v1/devices/"+dev, k, nil), 200))
	if st["availability"] != "up" || st["health"] != "warning" || st["open_incidents"] != float64(1) || st["since"] == nil {
		t.Fatalf("up with a warning: %v", st)
	}
	apply(host, "icmp", plugin.Critical)
	list := e.must(e.do("GET", "/v1/devices?availability=down", k, nil), 200)
	items := list.body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("availability filter: %s", list.raw)
	}
	if st := items[0].(map[string]any)["status"].(map[string]any); st["availability"] != "down" ||
		st["health"] != "critical" || st["open_incidents"] != float64(2) {
		t.Fatalf("down: %v", st)
	}
	if l := e.must(e.do("GET", "/v1/devices?availability=up&availability=unmonitored", k, nil), 200); len(l.body["items"].([]any)) != 0 {
		t.Errorf("up filter: %s", l.raw)
	}
	e.must(e.do("PATCH", "/v1/checks/"+host, k, map[string]any{"enabled": false}), 200)
	if st := status(e.must(e.do("GET", "/v1/devices/"+dev, k, nil), 200)); st["availability"] != "disabled" {
		t.Errorf("disabled host check: %v", st)
	}
}

// v0.2.1: reading a tenant by external ID goes through the list filter and
// never creates it.
func TestReadTenantByExternalID(t *testing.T) {
	e := setup(t, false)
	if l := e.must(e.do("GET", "/v1/tenants?external_source=plusclouds&external_id=acc", e.platformKey, nil), 200); len(l.body["items"].([]any)) != 0 {
		t.Fatalf("unknown tenant: %s", l.raw)
	}
	e.must(e.do("PUT", "/v1/tenants/by-external-id/acc", e.platformKey, map[string]any{"name": "Acme"}), 201)
	l := e.must(e.do("GET", "/v1/tenants?external_source=plusclouds&external_id=acc", e.platformKey, nil), 200)
	if items := l.body["items"].([]any); len(items) != 1 || items[0].(map[string]any)["name"] != "Acme" {
		t.Errorf("tenant: %s", l.raw)
	}
	if n := auditCount(t, "tenant.create"); n != 2 {
		t.Errorf("tenant.create events = %d, want 2 (platform + acc)", n)
	}
}

// v0.2.1: PATCH is a JSON Merge Patch: omitted fields stay, null clears,
// tags merge key by key.
func TestPatchDevice(t *testing.T) {
	e := setup(t, true)
	k := e.adminKey
	created := e.must(e.do("PUT", "/v1/devices/by-external-id/host-1?type=Hosts", k, map[string]any{
		"name": "a", "type": "server", "address": "10.0.0.1", "notes": "n",
		"tags": map[string]any{"rack": "a1", "env": "prod"}}), 201)
	id := created.body["id"].(string)

	r := e.must(e.do("PATCH", "/v1/devices/"+id, k, map[string]any{
		"name": "b", "notes": nil, "tags": map[string]any{"rack": "b2", "env": nil, "new": "x"}},
		"Content-Type", "application/merge-patch+json"), 200)
	tags := r.body["tags"].(map[string]any)
	if r.body["name"] != "b" || r.body["address"] != "10.0.0.1" || r.body["notes"] != nil ||
		tags["rack"] != "b2" || tags["env"] != nil || tags["new"] != "x" || r.body["external"] == nil {
		t.Fatalf("patched: %s", r.raw)
	}
	if r.body["status"] == nil {
		t.Error("PATCH response has no status")
	}

	r = e.must(e.do("PATCH", "/v1/devices/by-external-id/host-1?type=Hosts", k, map[string]any{"address": "10.0.0.2"}), 200)
	if r.body["address"] != "10.0.0.2" || r.body["name"] != "b" {
		t.Errorf("by external ID: %s", r.raw)
	}
	e.must(e.do("PATCH", "/v1/devices/by-external-id/nope", k, map[string]any{"name": "x"}), 404)
	if r := e.must(e.do("PATCH", "/v1/devices/"+id, k, map[string]any{"status": "up"}), 422); !strings.Contains(r.raw, "status") {
		t.Errorf("unknown field: %s", r.raw)
	}
	e.must(e.do("PATCH", "/v1/devices/"+id, k, map[string]any{"name": nil}), 422)
	e.must(e.do("PATCH", "/v1/devices/"+id, k, map[string]any{}), 200)
	if n := auditCount(t, "device.update"); n != 2 {
		t.Errorf("device.update events = %d, want 2 (an empty patch changes nothing)", n)
	}
}

func TestPatchCheck(t *testing.T) {
	e := setup(t, true)
	k := e.adminKey
	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "w", "type": "web"}), 201))
	cred := e.id(e.must(e.do("POST", "/v1/credentials", k, map[string]any{
		"name": "c", "type": "http_basic", "fields": map[string]any{"username": "u", "password": "p"}}), 201))
	chk := e.id(e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{
		"name": "home", "plugin": "http", "interval_seconds": 60, "credentials": map[string]any{"auth": cred},
		"config":     map[string]any{"keyword": "Welcome", "expected_status": []int{200}},
		"thresholds": []any{map[string]any{"metric": "total_ms", "critical": map[string]any{"op": ">", "value": 2000}}}}), 201))

	r := e.must(e.do("PATCH", "/v1/checks/"+chk, k, map[string]any{
		"interval_seconds": 120, "config": map[string]any{"keyword": nil, "method": "HEAD"}}), 200)
	cfg := r.body["config"].(map[string]any)
	if r.body["interval_seconds"] != float64(120) || cfg["keyword"] != nil || cfg["method"] != "HEAD" ||
		cfg["expected_status"] == nil || len(r.body["thresholds"].([]any)) != 1 ||
		r.body["credentials"].(map[string]any)["auth"] != cred || r.body["name"] != "home" {
		t.Fatalf("patched check: %s", r.raw)
	}
	e.must(e.do("PATCH", "/v1/checks/"+chk, k, map[string]any{"plugin": "icmp"}), 409)
	e.must(e.do("PATCH", "/v1/checks/"+chk, k, map[string]any{"interval_seconds": 1}), 422)
}
