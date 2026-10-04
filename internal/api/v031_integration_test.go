package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/internal/tenancy"
)

// v0.3.1: a deleted tenant can be restored intact until it is purged; a
// purge keeps the audit log and frees the external ID.
func TestTenantRestoreAndPurge(t *testing.T) {
	e := setup(t, false)
	ctx := context.Background()
	k := e.tenantKey("acc")
	as := []string{"X-Tenant-External-ID", "acc"}
	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "web", "type": "web"}), 201))

	e.must(e.do("POST", "/v1/tenants/by-external-id/nope/restore", e.platformKey, nil), 404)
	e.must(e.do("DELETE", "/v1/tenants/by-external-id/acc", e.platformKey, nil), 204)
	e.must(e.do("GET", "/v1/devices", e.platformKey, nil, as...), 404)
	r := e.must(e.do("POST", "/v1/tenants/by-external-id/acc/restore", e.platformKey, nil), 200)
	if r.body["status"] != "active" {
		t.Fatalf("restored: %s", r.raw)
	}
	e.must(e.do("POST", "/v1/tenants/by-external-id/acc/restore", e.platformKey, nil), 200)
	e.must(e.do("GET", "/v1/devices/"+dev, e.platformKey, nil, as...), 200)
	e.must(e.do("GET", "/v1/devices/"+dev, k, nil), 200) // its keys work again
	if n := auditCount(t, "tenant.restore"); n != 1 {
		t.Errorf("tenant.restore events = %d, want 1", n)
	}

	// Within the grace nothing is purged.
	e.must(e.do("DELETE", "/v1/tenants/by-external-id/acc", e.platformKey, nil), 204)
	if ids, err := tenancy.Purge(ctx, db.System, time.Hour); err != nil || len(ids) != 0 {
		t.Fatalf("purge within grace: %v, %v", ids, err)
	}
	if _, err := db.System.Exec(ctx, `UPDATE tenants SET deleted_at = now() - interval '2 hours' WHERE external_id = 'acc'`); err != nil {
		t.Fatal(err)
	}
	ids, err := tenancy.Purge(ctx, db.System, time.Hour)
	if err != nil || len(ids) != 1 {
		t.Fatalf("purge: %v, %v", ids, err)
	}
	var devices, audit int
	if err := db.System.QueryRow(ctx, `SELECT (SELECT count(*) FROM devices WHERE tenant_id = $1),
		(SELECT count(*) FROM audit_events WHERE tenant_id = $1)`, ids[0]).Scan(&devices, &audit); err != nil {
		t.Fatal(err)
	}
	if devices != 0 || audit == 0 {
		t.Errorf("after purge: %d devices, %d audit events (want 0 and some)", devices, audit)
	}
	if n := auditCount(t, "tenant.purge"); n != 1 {
		t.Errorf("tenant.purge events = %d, want 1", n)
	}
	e.must(e.do("POST", "/v1/tenants/by-external-id/acc/restore", e.platformKey, nil), 404)
	e.must(e.do("GET", "/v1/devices", k, nil), 401) // its keys are gone
	// The account can come back as a new tenant.
	again := e.must(e.do("PUT", "/v1/tenants/by-external-id/acc", e.platformKey, map[string]any{"name": "acc"}), 201)
	if again.body["id"] == ids[0].String() {
		t.Error("the purged tenant came back instead of a new one")
	}
}

// v0.3.1: operators configure what is monitored; API keys stay with admin.
func TestOperatorConfigures(t *testing.T) {
	e := setup(t, false)
	e.tenantKey("acc")
	key := func(role string) string {
		return e.must(e.do("POST", "/v1/api-keys", e.platformKey, map[string]any{"name": role, "role": role},
			"X-Tenant-External-ID", "acc"), 201).body["key"].(string)
	}
	op, ro := key("operator"), key("read-only")
	site := e.id(e.must(e.do("POST", "/v1/sites", op, map[string]any{"name": "ist"}), 201))
	dev := e.id(e.must(e.do("POST", "/v1/devices", op, map[string]any{"name": "web", "type": "web", "site_id": site}), 201))
	e.must(e.do("POST", "/v1/devices/"+dev+"/checks", op, map[string]any{"name": "home", "plugin": "http"}), 201)
	e.must(e.do("POST", "/v1/credentials", op, map[string]any{"name": "c", "type": "http_bearer",
		"fields": map[string]any{"token": "t"}}), 201)
	hook := e.id(e.must(e.do("POST", "/v1/webhooks", op, map[string]any{"name": "h", "url": "https://example.com/h"}), 201))
	e.must(e.do("POST", "/v1/alert-routes", op, map[string]any{"name": "r", "endpoint_id": hook}), 201)

	e.must(e.do("POST", "/v1/api-keys", op, map[string]any{"name": "x", "role": "read-only"}), 403)
	e.must(e.do("GET", "/v1/audit", op, nil), 403)
	e.must(e.do("POST", "/v1/devices", ro, map[string]any{"name": "x", "type": "web"}), 403)
	e.must(e.do("GET", "/v1/devices/"+dev, ro, nil), 200)
}

// v0.3.1: sites take JSON Merge Patch like devices.
func TestPatchSite(t *testing.T) {
	e := setup(t, true)
	k := e.adminKey
	e.must(e.do("PUT", "/v1/sites/by-external-id/dc-1", k, map[string]any{"name": "DC 1", "country": "TR",
		"address": "Istanbul"}), 201)
	r := e.must(e.do("PATCH", "/v1/sites/by-external-id/dc-1", k, map[string]any{"address": nil, "timezone": "Europe/Istanbul"},
		"Content-Type", "application/merge-patch+json"), 200)
	if r.body["name"] != "DC 1" || r.body["country"] != "TR" || r.body["address"] != nil || r.body["timezone"] != "Europe/Istanbul" {
		t.Fatalf("patched: %s", r.raw)
	}
	r = e.must(e.do("PATCH", "/v1/sites/"+r.body["id"].(string), k, map[string]any{"name": "DC One"}), 200)
	if r.body["name"] != "DC One" || r.body["external"] == nil {
		t.Errorf("by id: %s", r.raw)
	}
	e.must(e.do("PATCH", "/v1/sites/by-external-id/nope", k, map[string]any{"name": "x"}), 404)
	e.must(e.do("PATCH", "/v1/sites/by-external-id/dc-1", k, map[string]any{"country": "turkey"}), 422)
}
