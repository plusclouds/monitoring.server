package api_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func (e *env) id(r resp) string {
	e.t.Helper()
	id, ok := r.body["id"].(string)
	if !ok {
		e.t.Fatalf("no id in %s", r.raw)
	}
	return id
}

func problemType(r resp) string {
	s, _ := r.body["type"].(string)
	return s[strings.LastIndex(s, "/")+1:]
}

// F02: sites and devices, the containment tree and the 1-device limit
// interplay with names and references.
func TestDevicesAndSites(t *testing.T) {
	e := setup(t, true)
	k := e.adminKey
	site := e.id(e.must(e.do("POST", "/v1/sites", k, map[string]any{
		"name": "ist-1", "country": "TR", "timezone": "Europe/Istanbul"}), 201))
	e.must(e.do("POST", "/v1/sites", k, map[string]any{"name": "bad", "country": "Turkey"}), 400)
	if r := e.do("POST", "/v1/sites", k, map[string]any{"name": "bad", "timezone": "Mars/Base"}); r.status != 422 {
		t.Fatalf("bad timezone: %d %s", r.status, r.raw)
	}

	pool := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{
		"name": "pool-1", "type": "hypervisor_pool", "site_id": site, "tags": map[string]any{"env": "prod"}}), 201))
	host := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{
		"name": "host-1", "type": "hypervisor_host", "address": "10.0.0.5", "parent_id": pool}), 201))
	r := e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "host-1", "type": "server"}), 409)
	if problemType(r) != "already-exists" {
		t.Errorf("duplicate name: %s", r.raw)
	}
	r = e.must(e.do("POST", "/v1/devices", k, map[string]any{
		"name": "x", "type": "server", "parent_id": "0190d9c6-0000-7000-8000-000000000000"}), 422)
	if !strings.Contains(r.raw, "parent_id") {
		t.Errorf("unknown parent: %s", r.raw)
	}

	children := e.must(e.do("GET", "/v1/devices/"+pool+"/children", k, nil), 200)
	if items := children.body["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != host {
		t.Errorf("children: %s", children.raw)
	}
	byTag := e.must(e.do("GET", "/v1/devices?tag=env=prod", k, nil), 200)
	if items := byTag.body["items"].([]any); len(items) != 1 {
		t.Errorf("tag filter: %s", byTag.raw)
	}

	// Paging.
	for i := range 3 {
		e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": fmt.Sprintf("web-%d", i), "type": "web"}), 201)
	}
	first := e.must(e.do("GET", "/v1/devices?limit=2", k, nil), 200)
	if first.body["next_cursor"] == nil || len(first.body["items"].([]any)) != 2 {
		t.Fatalf("first page: %s", first.raw)
	}
	rest := e.must(e.do("GET", "/v1/devices?limit=10&cursor="+first.body["next_cursor"].(string), k, nil), 200)
	if len(rest.body["items"].([]any)) != 3 || rest.body["next_cursor"] != nil {
		t.Errorf("second page: %s", rest.raw)
	}

	// A site with devices cannot go; a device with children needs confirm.
	if r := e.must(e.do("DELETE", "/v1/sites/"+site, k, nil), 409); problemType(r) != "in-use" {
		t.Errorf("site in use: %s", r.raw)
	}
	if r := e.must(e.do("DELETE", "/v1/devices/"+pool, k, nil), 409); problemType(r) != "has-children" {
		t.Errorf("children: %s", r.raw)
	}
	e.must(e.do("DELETE", "/v1/devices/"+pool+"?confirm=true", k, nil), 204)
	e.must(e.do("GET", "/v1/devices/"+host, k, nil), 404)
	e.must(e.do("DELETE", "/v1/sites/"+site, k, nil), 204)

	// Updates replace the device; an identical PUT writes no audit event.
	web := e.do("GET", "/v1/devices?limit=1", k, nil).body["items"].([]any)[0].(map[string]any)
	body := map[string]any{"name": web["name"], "type": "web", "address": "https://example.com"}
	e.must(e.do("PUT", "/v1/devices/"+web["id"].(string), k, body), 200)
	e.must(e.do("PUT", "/v1/devices/"+web["id"].(string), k, body), 200)
	if n := auditCount(t, "device.update"); n != 1 {
		t.Errorf("device.update events = %d, want 1", n)
	}

	// Read-only keys read but do not write.
	ro := e.must(e.do("POST", "/v1/api-keys", k, map[string]any{"name": "ro", "role": "read-only"}), 201).body["key"].(string)
	e.must(e.do("GET", "/v1/devices", ro, nil), 200)
	e.must(e.do("POST", "/v1/devices", ro, map[string]any{"name": "n", "type": "web"}), 403)
}

// F02: the tenant's max_devices limit.
func TestDeviceLimit(t *testing.T) {
	e := setup(t, false)
	e.must(e.do("PUT", "/v1/tenants/by-external-id/acc", e.platformKey, map[string]any{
		"name": "Acme", "limits": map[string]any{"max_devices": 1}}), 201)
	as := []string{"X-Tenant-External-ID", "acc"}
	e.must(e.do("POST", "/v1/devices", e.platformKey, map[string]any{"name": "a", "type": "web"}, as...), 201)
	r := e.must(e.do("POST", "/v1/devices", e.platformKey, map[string]any{"name": "b", "type": "web"}, as...), 409)
	if problemType(r) != "limit-reached" {
		t.Errorf("limit: %s", r.raw)
	}
}

// ADR-0012: devices upsert by external ID; the same body twice is one create.
func TestDeviceUpsertByExternalID(t *testing.T) {
	e := setup(t, false)
	e.must(e.do("PUT", "/v1/tenants/by-external-id/acc", e.platformKey, map[string]any{"name": "Acme"}), 201)
	as := []string{"X-Tenant-External-ID", "acc"}
	path := "/v1/devices/by-external-id/vm-42?type=NextDeveloper%5CIAAS%5CDatabase%5CModels%5CVirtualMachines"
	body := map[string]any{"name": "vm-42", "type": "vm", "address": "10.1.1.1"}
	created := e.must(e.do("PUT", path, e.platformKey, body, as...), 201)
	again := e.must(e.do("PUT", path, e.platformKey, body, as...), 200)
	if created.body["id"] != again.body["id"] {
		t.Fatal("upsert created a second device")
	}
	ext := created.body["external"].(map[string]any)
	if ext["source"] != "plusclouds" || ext["id"] != "vm-42" || !strings.Contains(ext["type"].(string), "VirtualMachines") {
		t.Errorf("external: %v", ext)
	}
	list := e.must(e.do("GET", "/v1/devices?external_id=vm-42", e.platformKey, nil, as...), 200)
	if len(list.body["items"].([]any)) != 1 {
		t.Errorf("external filter: %s", list.raw)
	}
	if n := auditCount(t, "device.create"); n != 1 {
		t.Errorf("device.create events = %d, want 1", n)
	}
}

// ADR-0014: dependencies, implied containment, cycles and impact.
func TestDependencies(t *testing.T) {
	e := setup(t, true)
	k := e.adminKey
	dev := func(name, parent string) string {
		body := map[string]any{"name": name, "type": "server"}
		if parent != "" {
			body["parent_id"] = parent
		}
		return e.id(e.must(e.do("POST", "/v1/devices", k, body), 201))
	}
	ups := dev("ups", "")
	sw := dev("switch", "")
	host := dev("host", "")
	vm := dev("vm", host)

	e.must(e.do("POST", "/v1/devices/"+sw+"/dependencies", k, map[string]any{"depends_on_id": ups}), 201)
	e.must(e.do("POST", "/v1/devices/"+host+"/dependencies", k, map[string]any{"depends_on_id": sw}), 201)
	e.must(e.do("POST", "/v1/devices/"+host+"/dependencies", k, map[string]any{"depends_on_id": sw}), 200)

	// ups -> switch -> host -> vm (containment): ups depending on vm is a cycle.
	r := e.must(e.do("POST", "/v1/devices/"+ups+"/dependencies", k, map[string]any{"depends_on_id": vm}), 422)
	if !strings.Contains(r.raw, "cycle") {
		t.Errorf("cycle: %s", r.raw)
	}
	// Containment cycles too: the host cannot move into its own VM.
	e.must(e.do("PUT", "/v1/devices/"+host, k, map[string]any{"name": "host", "type": "server", "parent_id": vm}), 422)

	deps := e.must(e.do("GET", "/v1/devices/"+vm+"/dependencies", k, nil), 200)
	items := deps.body["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["source"] != "containment" {
		t.Errorf("vm dependencies: %s", deps.raw)
	}
	impact := e.must(e.do("GET", "/v1/devices/"+ups+"/impact", k, nil), 200)
	var names []string
	for _, it := range impact.body["items"].([]any) {
		m := it.(map[string]any)
		names = append(names, fmt.Sprintf("%s:%v", m["device"].(map[string]any)["name"], m["distance"]))
	}
	if strings.Join(names, ",") != "switch:1,host:2,vm:3" {
		t.Errorf("impact = %v", names)
	}
	e.must(e.do("DELETE", "/v1/devices/"+host+"/dependencies/"+sw, k, nil), 204)
	impact = e.must(e.do("GET", "/v1/devices/"+ups+"/impact", k, nil), 200)
	if len(impact.body["items"].([]any)) != 1 {
		t.Errorf("impact after removal: %s", impact.raw)
	}
}

// F02, ADR-0006: secrets are write-only, encrypted at rest, never in
// responses or audit events; credentials in use cannot be deleted.
func TestCredentials(t *testing.T) {
	e := setup(t, true)
	k := e.adminKey
	const marker = "MARKER-s3cret-value"
	types := e.must(e.do("GET", "/v1/credential-types", k, nil), 200)
	if !strings.Contains(types.raw, `"writeOnly":true`) {
		t.Errorf("credential types should mark secrets writeOnly: %s", types.raw)
	}

	r := e.must(e.do("POST", "/v1/credentials", k, map[string]any{
		"name": "web", "type": "http_basic", "fields": map[string]any{"username": "monitor", "password": marker}}), 201)
	cred := e.id(r)
	if r.body["fields"].(map[string]any)["username"] != "monitor" || strings.Contains(r.raw, marker) {
		t.Fatalf("create response: %s", r.raw)
	}
	e.must(e.do("POST", "/v1/credentials", k, map[string]any{
		"name": "bad", "type": "http_basic", "fields": map[string]any{"username": "x"}}), 422)
	e.must(e.do("POST", "/v1/credentials", k, map[string]any{
		"name": "v3", "type": "snmp_v3", "fields": map[string]any{"username": "u", "auth_password": "a"}}), 422) // authPriv needs priv_password

	// Changing only the username keeps the password.
	upd := e.must(e.do("PUT", "/v1/credentials/"+cred, k, map[string]any{
		"name": "web", "type": "http_basic", "fields": map[string]any{"username": "monitor2"}}), 200)
	if s := upd.body["secrets_set"].([]any); len(s) != 1 || s[0] != "password" {
		t.Errorf("secrets_set after update: %s", upd.raw)
	}
	e.must(e.do("PUT", "/v1/credentials/"+cred, k, map[string]any{"name": "web", "type": "http_bearer"}), 409)

	var plain int
	if err := db.System.QueryRow(context.Background(), `
		SELECT count(*) FROM credentials WHERE position(convert_to($1, 'UTF8') IN ciphertext) > 0
		   OR fields::text LIKE '%' || $1 || '%'`, marker).Scan(&plain); err != nil || plain != 0 {
		t.Errorf("secret stored in plain text (%d rows, err %v)", plain, err)
	}
	var leaks int
	if err := db.System.QueryRow(context.Background(), `
		SELECT count(*) FROM audit_events WHERE coalesce(before::text, '') || coalesce(after::text, '') LIKE '%' || $1 || '%'`,
		marker).Scan(&leaks); err != nil || leaks != 0 {
		t.Errorf("secret in %d audit events (err %v)", leaks, err)
	}

	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "site", "type": "web"}), 201))
	e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{
		"name": "home", "plugin": "http", "credentials": map[string]any{"auth": cred}}), 201)
	if r := e.must(e.do("DELETE", "/v1/credentials/"+cred, k, nil), 409); problemType(r) != "in-use" {
		t.Errorf("delete in use: %s", r.raw)
	}
}

// F02, F03: check validation against the plugin and the tenant.
func TestChecks(t *testing.T) {
	e := setup(t, true)
	k := e.adminKey
	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "web", "type": "web", "address": "https://example.com"}), 201))
	path := "/v1/devices/" + dev + "/checks"

	c := e.must(e.do("POST", path, k, map[string]any{
		"name": "home", "plugin": "http", "interval_seconds": 60,
		"config": map[string]any{"expected_status": []int{200}, "keyword": "Welcome"},
		"thresholds": []any{map[string]any{"metric": "total_ms",
			"warning": map[string]any{"op": ">", "value": 800}, "critical": map[string]any{"op": ">", "value": 2000}, "for": "5m"}},
	}), 201)
	if c.body["failure_count"] != float64(3) || c.body["thresholds"].([]any)[0].(map[string]any)["id"] != "r1" {
		t.Errorf("defaults: %s", c.raw)
	}

	cases := map[string]map[string]any{
		"unknown plugin": {"name": "a", "plugin": "nope"},
		"bad config":     {"name": "a", "plugin": "http", "config": map[string]any{"no_such": true}},
		"plugin rule":    {"name": "a", "plugin": "http", "config": map[string]any{"expected_status": []int{999}}},
		"tenant minimum": {"name": "a", "plugin": "http", "interval_seconds": 10}, // tenant default minimum is 30
		"unknown metric": {"name": "a", "plugin": "http", "thresholds": []any{map[string]any{"metric": "cpu", "critical": map[string]any{"op": ">", "value": 1}}}},
		"range op":       {"name": "a", "plugin": "http", "thresholds": []any{map[string]any{"metric": "total_ms", "critical": map[string]any{"op": "between", "value": 1}}}},
		"wrong cred":     {"name": "a", "plugin": "icmp", "credentials": map[string]any{"auth": "0190d9c6-0000-7000-8000-000000000000"}},
	}
	for name, body := range cases {
		if r := e.do("POST", path, k, body); r.status != 422 && r.status != 400 {
			t.Errorf("%s: status %d %s", name, r.status, r.raw)
		}
	}
	e.must(e.do("POST", path, k, map[string]any{"name": "home", "plugin": "http"}), 409)
	e.must(e.do("POST", path, k, map[string]any{"name": "ping", "plugin": "icmp", "is_host_check": true}), 201)
	if r := e.must(e.do("POST", path, k, map[string]any{"name": "ping2", "plugin": "icmp", "is_host_check": true}), 409); problemType(r) != "already-exists" {
		t.Errorf("second host check: %s", r.raw)
	}

	id := e.id(c)
	e.must(e.do("PUT", "/v1/checks/"+id, k, map[string]any{"name": "home", "plugin": "icmp"}), 409)
	upd := e.must(e.do("PUT", "/v1/checks/"+id, k, map[string]any{"name": "home", "plugin": "http", "enabled": false}), 200)
	if upd.body["enabled"] != false || len(upd.body["thresholds"].([]any)) != 0 {
		t.Errorf("PUT should replace the check: %s", upd.raw)
	}
	list := e.must(e.do("GET", "/v1/checks?enabled=false", k, nil), 200)
	if len(list.body["items"].([]any)) != 1 {
		t.Errorf("enabled filter: %s", list.raw)
	}
	e.must(e.do("DELETE", "/v1/checks/"+id, k, nil), 204)
	e.must(e.do("GET", "/v1/checks/"+id, k, nil), 404)
}

// F02: POST /devices/{id}/test runs enabled checks once and stores nothing.
func TestDeviceTest(t *testing.T) {
	e := setup(t, true)
	k := e.adminKey
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("Welcome")) }))
	defer ok.Close()

	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "web", "type": "web", "address": ok.URL}), 201))
	e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{
		"name": "home", "plugin": "http", "config": map[string]any{"keyword": "Welcome"}}), 201)
	e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{
		"name": "missing", "plugin": "http", "config": map[string]any{"keyword": "Goodbye"}}), 201)
	e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{
		"name": "off", "plugin": "http", "enabled": false}), 201)

	run := func() map[string]map[string]any {
		r := e.must(e.do("POST", "/v1/devices/"+dev+"/test", k, nil), 200)
		got := map[string]map[string]any{}
		for _, it := range r.body["items"].([]any) {
			m := it.(map[string]any)
			got[m["name"].(string)] = m
		}
		if len(got) != 2 {
			t.Fatalf("want the 2 enabled checks: %s", r.raw)
		}
		return got
	}

	// The standalone tenant is a customer tenant: without allowed networks
	// it cannot reach loopback, and the check reports UNKNOWN, not CRITICAL.
	got := run()
	if got["home"]["status"] != "UNKNOWN" || !strings.Contains(got["home"]["output"].(string), "private or local") {
		t.Errorf("policy: %v", got["home"])
	}

	if _, err := db.System.Exec(context.Background(),
		`UPDATE tenants SET allowed_target_networks = '{127.0.0.0/8}' WHERE NOT is_platform`); err != nil {
		t.Fatal(err)
	}
	got = run()
	if got["home"]["status"] != "OK" || got["home"]["metrics"].(map[string]any)["status_code"] != float64(200) {
		t.Errorf("home: %v", got["home"])
	}
	if got["missing"]["status"] != "CRITICAL" {
		t.Errorf("missing keyword: %v", got["missing"])
	}
	var states int
	if err := db.System.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action LIKE 'check.%' AND action <> 'check.create'`).Scan(&states); err != nil || states != 0 {
		t.Errorf("device test wrote %d events (err %v)", states, err)
	}
}
