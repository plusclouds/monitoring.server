package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/metrics"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/internal/store"
	"github.com/plusclouds/monitoring.server/internal/usage"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// M4: a collector's inventory becomes child devices. VMs keep their device
// across renames and migrations, their incidents and metrics belong to
// them, they do not count toward max_devices, and they go away a week
// after the collector stops reporting them, or with the collector.
func TestDiscoveredDevices(t *testing.T) {
	ctx := context.Background()
	x := newNoise(t, 0)
	pool := x.id(x.must(x.do("POST", "/v1/devices", x.key, map[string]any{"name": "pool-dc1", "type": "hypervisor_host",
		"address": "10.0.0.10"}), 201))
	x.device("web2") // a user's device whose name a VM will also have
	chk := x.id(x.must(x.do("POST", "/v1/devices/"+pool+"/checks", x.key, map[string]any{"name": "pool", "plugin": "xapi.pool",
		"failure_count": 1}), 201))
	w, err := metrics.NewWriter(metrics.WriterOptions{System: db.System})
	if err != nil {
		t.Fatal(err)
	}
	host := func(n string) plugin.ChildDevice {
		return plugin.ChildDevice{Key: "host:" + n, Name: "xcp-" + n, Type: "hypervisor_host", Address: "10.0.0." + n}
	}
	vm := func(key, name, parent string) plugin.ChildDevice {
		return plugin.ChildDevice{Key: "vm:" + key, Name: name, Type: "vm", ParentKey: parent}
	}
	obj := func(key string, st plugin.Status) plugin.Object {
		m := plugin.NaNs(11)
		m[0] = 42
		return plugin.Object{Key: key, Name: key, Device: key, Status: st, Output: st.String(), Metrics: m}
	}
	apply := func(at time.Time, children []plugin.ChildDevice, objects ...plugin.Object) {
		t.Helper()
		if objects == nil {
			objects = []plugin.Object{} // a successful run (nil would be a failed one)
		}
		r := runner.Result{TenantID: x.tenant, DeviceID: uuid.MustParse(pool), CheckID: uuid.MustParse(chk), Plugin: "xapi.pool",
			Interval: time.Minute, Result: plugin.Result{Status: plugin.OK, Output: "pool", Time: at, Objects: objects,
				Inventory: &plugin.Inventory{Children: children}}}
		if _, err := x.eng.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	type dev struct {
		id, name, parent, typ string
	}
	devices := func() map[string]dev {
		out := map[string]dev{}
		rows, err := db.System.Query(ctx, `SELECT collector_key, id::text, name, coalesce(parent_id::text, ''), type FROM devices
			WHERE managed_by = $1`, chk)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var d dev
			if err := rows.Scan(&k, &d.id, &d.name, &d.parent, &d.typ); err != nil {
				t.Fatal(err)
			}
			out[k] = d
		}
		return out
	}

	now := time.Now()
	apply(now, []plugin.ChildDevice{host("1"), host("2"), vm("web", "web2", "host:1"), vm("db", "db", "host:2")},
		obj("vm:web", plugin.Critical), obj("host:1", plugin.OK))
	d := devices()
	if len(d) != 4 || d["host:1"].parent != pool || d["vm:web"].parent != d["host:1"].id || d["vm:web"].typ != "vm" ||
		d["vm:web"].name != "web2 [vm:web]" {
		t.Fatalf("discovered: %+v", d)
	}
	web := d["vm:web"].id

	// The VM's incident and metrics belong to the VM.
	inc := x.incident(chk)
	if inc["device_id"] != web || inc["object_key"] != "vm:web" {
		t.Errorf("VM incident: %v", inc)
	}
	r := runner.Result{TenantID: x.tenant, DeviceID: uuid.MustParse(pool), CheckID: uuid.MustParse(chk), Plugin: "xapi.pool",
		Result:        plugin.Result{Time: now, Objects: []plugin.Object{obj("vm:web", plugin.OK)}},
		ObjectDevices: map[string]uuid.UUID{"vm:web": uuid.MustParse(web)}}
	w.Add(r)
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	series := x.must(x.do("GET", "/v1/metrics/series?device_id="+web, x.key, nil), 200).body["items"].([]any)
	if len(series) != 11 {
		t.Errorf("VM metrics by the VM's device: %d series", len(series))
	}
	for _, s := range series {
		if m := s.(map[string]any); m["device_id"] != web || m["object"] != "vm:web" {
			t.Errorf("series: %v", m)
		}
	}
	objs := x.must(x.do("GET", "/v1/checks/"+chk+"/objects", x.key, nil), 200).body["items"].([]any)
	if len(objs) != 2 || objs[1].(map[string]any)["device_id"] != web {
		t.Errorf("objects: %v", objs)
	}
	dv := x.must(x.do("GET", "/v1/devices/"+web, x.key, nil), 200).body
	if disc, _ := dv["discovered"].(map[string]any); disc == nil || disc["check_id"] != chk || disc["key"] != "vm:web" ||
		disc["gone_at"] != nil || dv["managed_by"] != chk {
		t.Errorf("discovered device: %v", dv)
	}
	if p := x.must(x.do("GET", "/v1/devices/"+pool, x.key, nil), 200).body; p["discovered"] != nil {
		t.Errorf("a user's device is not discovered: %v", p["discovered"])
	}

	// Discovered devices do not count toward max_devices (pool, web2: 2).
	if _, err := db.System.Exec(ctx, `UPDATE tenants SET max_devices = 3 WHERE id = $1`, x.tenant); err != nil {
		t.Fatal(err)
	}
	x.device("third")
	x.must(x.do("POST", "/v1/devices", x.key, map[string]any{"name": "fourth", "type": "server"}), 409)

	// Migration and rename keep the device.
	apply(now.Add(time.Minute), []plugin.ChildDevice{host("1"), host("2"), vm("web", "web2", "host:2"), vm("db", "postgres", "host:2")})
	d = devices()
	if d["vm:web"].id != web || d["vm:web"].parent != d["host:2"].id || d["vm:db"].name != "postgres" {
		t.Errorf("after migration and rename: %+v", d)
	}

	// No longer reported: kept for a week, then removed.
	apply(now.Add(2*time.Minute), []plugin.ChildDevice{host("1"), host("2"), vm("web", "web2", "host:2")})
	var gone *time.Time
	if err := db.System.QueryRow(ctx, `SELECT collector_gone_at FROM devices WHERE id = $1`, d["vm:db"].id).Scan(&gone); err != nil || gone == nil {
		t.Fatalf("db not marked gone: %v %v", gone, err)
	}
	apply(now.Add(8*24*time.Hour), []plugin.ChildDevice{host("1"), host("2"), vm("web", "web2", "host:2")})
	if _, ok := devices()["vm:db"]; ok {
		t.Error("db still there after a week")
	}
	// It comes back as a new device.
	apply(now.Add(8*24*time.Hour+time.Minute), []plugin.ChildDevice{host("1"), host("2"), vm("web", "web2", "host:2"), vm("db", "db", "")})
	if d := devices(); d["vm:db"].parent != pool {
		t.Errorf("db back under the pool: %+v", d["vm:db"])
	}

	// Deleting the collector removes what it discovered.
	x.must(x.do("DELETE", "/v1/checks/"+chk, x.key, nil), 204)
	if d := devices(); len(d) != 0 {
		t.Errorf("devices left after the collector was deleted: %v", d)
	}
	var n int
	if err := db.System.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'device.discover'`).Scan(&n); err != nil || n != 5 {
		t.Errorf("discover audit events: %d %v", n, err)
	}
}

// M4: a discovered host that goes down suppresses its VMs' incidents and
// the incidents of checks on those VMs, like a failed host check; when it
// recovers, they are released (re-linked to the VM if it is still down).
func TestDiscoveredHostSuppression(t *testing.T) {
	ctx := context.Background()
	x := newNoise(t, 0)
	pool := x.id(x.must(x.do("POST", "/v1/devices", x.key, map[string]any{"name": "pool", "type": "hypervisor_host"}), 201))
	chk := x.id(x.must(x.do("POST", "/v1/devices/"+pool+"/checks", x.key, map[string]any{"name": "pool", "plugin": "xapi.pool",
		"failure_count": 1}), 201))
	children := []plugin.ChildDevice{{Key: "host:1", Name: "xcp-1", Type: "hypervisor_host"},
		{Key: "vm:web", Name: "web", Type: "vm", ParentKey: "host:1"}}
	o := func(key string, st plugin.Status) plugin.Object {
		return plugin.Object{Key: key, Name: key, Device: key, Availability: true, Status: st, Output: st.String(), Metrics: plugin.NaNs(11)}
	}
	collect := func(host, vm plugin.Status) {
		t.Helper()
		if _, err := x.eng.Apply(ctx, runner.Result{TenantID: x.tenant, DeviceID: uuid.MustParse(pool), CheckID: uuid.MustParse(chk),
			Plugin: "xapi.pool", Interval: time.Minute, Result: plugin.Result{Status: plugin.OK, Output: "pool", Time: time.Now(),
				Objects: []plugin.Object{o("host:1", host), o("vm:web", vm)}, Inventory: &plugin.Inventory{Children: children}}}); err != nil {
			t.Fatal(err)
		}
	}
	collect(plugin.OK, plugin.OK)
	var web string
	if err := db.System.QueryRow(ctx, `SELECT id::text FROM devices WHERE collector_key = 'vm:web'`).Scan(&web); err != nil {
		t.Fatal(err)
	}
	site := x.check(web, "site", "http", false)
	objIncident := func(key string) map[string]any {
		t.Helper()
		l := x.must(x.do("GET", "/v1/incidents?status=active&check_id="+chk+"&object_key="+key, x.key, nil), 200).body["items"].([]any)
		if len(l) != 1 {
			t.Fatalf("incidents of %s: %v", key, l)
		}
		return l[0].(map[string]any)
	}

	// Maintenance mode is a warning: it suppresses nothing.
	collect(plugin.Warning, plugin.Critical)
	if vm := objIncident("vm:web"); vm["suppressed"] != false {
		t.Errorf("a host in maintenance suppressed its VM: %v", vm)
	}
	collect(plugin.OK, plugin.OK)

	// The host goes down: the VM and the site on it hang under the host.
	collect(plugin.Critical, plugin.Critical)
	host := objIncident("host:1")
	vm := objIncident("vm:web")
	x.result(web, site, "http", plugin.Critical)
	s := x.incident(site)
	if host["suppressed"] != false || vm["suppressed"] != true || vm["root_incident_id"] != host["id"] ||
		s["suppressed"] != true || s["root_incident_id"] != host["id"] {
		t.Fatalf("host down: host %v\nvm %v\nsite %v", host, vm, s)
	}

	// The host comes back, the VM is still down: the VM is announced and the
	// site now hangs under the VM.
	collect(plugin.OK, plugin.Critical)
	vm, s = objIncident("vm:web"), x.incident(site)
	if vm["suppressed"] != false || s["suppressed"] != true || s["root_incident_id"] != vm["id"] {
		t.Fatalf("host back: vm %v\nsite %v", vm, s)
	}

	// The VM comes back: the site, still failing, is announced.
	collect(plugin.OK, plugin.OK)
	if s = x.incident(site); s["suppressed"] != false {
		t.Errorf("VM back: site %v", s)
	}
}

// F13 (2026-10-07): an XCP-ng pool bills per hypervisor host ("xapi.pool:host",
// weight 3), not per pool (weight 0); VMs are not billed. A host's period
// ends when it is gone or the check is disabled.
func TestHostBilling(t *testing.T) {
	ctx := context.Background()
	x := newNoise(t, 0)
	if _, err := usage.SyncWeights(ctx, db.System, config.Default().Usage, time.Now().Add(-3*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pool := x.id(x.must(x.do("POST", "/v1/devices", x.key, map[string]any{"name": "pool", "type": "hypervisor_host"}), 201))
	chk := x.id(x.must(x.do("POST", "/v1/devices/"+pool+"/checks", x.key, map[string]any{"name": "pool", "plugin": "xapi.pool"}), 201))
	collect := func(hosts ...string) {
		t.Helper()
		var children []plugin.ChildDevice
		var objs []plugin.Object
		for _, h := range hosts {
			children = append(children, plugin.ChildDevice{Key: "host:" + h, Name: "xcp-" + h, Type: "hypervisor_host"})
			objs = append(objs, plugin.Object{Key: "host:" + h, Name: h, Device: "host:" + h, Availability: true, Metrics: plugin.NaNs(11)})
		}
		children = append(children, plugin.ChildDevice{Key: "vm:a", Name: "a", Type: "vm"})
		objs = append(objs, plugin.Object{Key: "vm:a", Name: "a", Device: "vm:a", Metrics: plugin.NaNs(11)})
		if _, err := x.eng.Apply(ctx, runner.Result{TenantID: x.tenant, DeviceID: uuid.MustParse(pool), CheckID: uuid.MustParse(chk),
			Plugin: "xapi.pool", Interval: time.Minute, Result: plugin.Result{Status: plugin.OK, Output: "pool", Time: time.Now(),
				Objects: objs, Inventory: &plugin.Inventory{Children: children}}}); err != nil {
			t.Fatal(err)
		}
	}
	open := func() []string {
		rows, err := db.System.Query(ctx, `SELECT object_key FROM object_periods WHERE check_id = $1 AND ended_at IS NULL ORDER BY 1`, chk)
		if err != nil {
			t.Fatal(err)
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return keys
	}

	collect("1", "2")
	if got := open(); len(got) != 2 || got[0] != "host:1" {
		t.Fatalf("open periods: %v", got)
	}
	cur := x.must(x.do("GET", "/v1/usage/current", x.key, nil), 200).body
	byPlugin := cur["by_plugin"].(map[string]any)
	if h := byPlugin["xapi.pool:host"].(map[string]any); h["checks"] != float64(2) || h["weight"] != float64(3) {
		t.Errorf("hosts: %v", byPlugin)
	}
	if p := byPlugin["xapi.pool"].(map[string]any); p["weight"] != float64(0) {
		t.Errorf("the pool itself: %v", p)
	}
	if cur["weighted_checks"] != float64(6) {
		t.Errorf("weighted: %v", cur["weighted_checks"])
	}

	collect("1") // host 2 left the pool
	if got := open(); len(got) != 1 {
		t.Errorf("after a host left: %v", got)
	}
	x.must(x.do("PATCH", "/v1/checks/"+chk, x.key, map[string]any{"enabled": false}), 200)
	if got := open(); len(got) != 0 {
		t.Errorf("disabled check: %v", got)
	}
	x.must(x.do("PATCH", "/v1/checks/"+chk, x.key, map[string]any{"enabled": true}), 200)
	collect("1") // disabling forgot the objects: billing resumes with the next collection
	if got := open(); len(got) != 1 || got[0] != "host:1" {
		t.Errorf("enabled again: %v", got)
	}

	// A closed hour bills each host second at weight 3.
	owner, err := store.Open(ctx, store.PoolOptions{DSN: db.OwnerDSN})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	h := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	if _, err := owner.Exec(ctx, `UPDATE object_periods SET started_at = $2 WHERE check_id = $1 AND ended_at IS NULL`, chk, h); err != nil {
		t.Fatal(err)
	}
	if _, err := db.System.Exec(ctx, `SELECT usage_close_hour($1, 1, NULL)`, h); err != nil {
		t.Fatal(err)
	}
	var secs int64
	var weight float64
	if err := db.System.QueryRow(ctx, `SELECT count_seconds, weight::float8 FROM usage_hour_plugins
		WHERE plugin = 'xapi.pool:host' AND period_start = $1`, h).Scan(&secs, &weight); err != nil || secs != 3600 || weight != 3 {
		t.Errorf("closed hour: %d s at %v, %v", secs, weight, err)
	}
	if w := x.must(x.do("GET", "/v1/usage/weights", x.key, nil), 200).body["weights"].(map[string]any); w["xapi.pool:host"] != float64(3) {
		t.Errorf("weights: %v", w)
	}
	// Deleting the check ends the periods.
	x.must(x.do("DELETE", "/v1/checks/"+chk, x.key, nil), 204)
	if got := open(); len(got) != 0 {
		t.Errorf("deleted check: %v", got)
	}
}
