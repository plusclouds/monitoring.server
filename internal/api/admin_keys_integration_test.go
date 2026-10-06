package api_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/internal/admin"
)

// A lost bootstrap key is replaced from the command line: rotation revokes
// the old platform key unless asked to keep it; standalone tenants get a new
// admin key.
func TestAdminKeyCommands(t *testing.T) {
	ctx := context.Background()
	e := setup(t, false)
	old := e.platformKey
	e.must(e.do("GET", "/v1/tenants", old, nil), 200)

	kept, err := admin.RotatePlatformKey(ctx, db.System, true)
	if err != nil || kept.Revoked != 0 {
		t.Fatalf("rotate --keep-old: %+v %v", kept, err)
	}
	e.must(e.do("GET", "/v1/tenants", old, nil), 200)
	e.must(e.do("GET", "/v1/tenants", kept.Key, nil), 200)
	if _, err := admin.RevokePlatformKeys(ctx, db.System, uuid.New()); err == nil {
		t.Error("revoking with an unknown key to keep should fail")
	}
	if n, err := admin.RevokePlatformKeys(ctx, db.System, kept.ID); err != nil || n != 1 {
		t.Fatalf("revoke-platform-keys: %d %v", n, err)
	}
	e.must(e.do("GET", "/v1/tenants", old, nil), 401)

	rotated, err := admin.RotatePlatformKey(ctx, db.System, false)
	if err != nil || rotated.Revoked != 1 {
		t.Fatalf("rotate: %+v %v", rotated, err)
	}
	e.must(e.do("GET", "/v1/tenants", kept.Key, nil), 401)
	e.must(e.do("GET", "/v1/tenants", rotated.Key, nil), 200)

	var n int
	if err := db.System.QueryRow(ctx, `SELECT count(*) FROM audit_events
		WHERE action IN ('api_key.rotate', 'api_key.revoke') AND actor_kind = 'admin-cli'`).Scan(&n); err != nil || n != 3 {
		t.Errorf("audit events: %d %v", n, err)
	}

	// Standalone: a new admin key for the tenant; the platform tenant has none.
	s := setup(t, true)
	tenant := uuid.MustParse(s.must(s.do("GET", "/v1/tenant", s.adminKey, nil), 200).body["id"].(string))
	k, err := admin.CreateAdminKey(ctx, db.System, tenant)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.must(s.do("GET", "/v1/tenant", k.Key, nil), 200).body["id"]; got != tenant.String() {
		t.Errorf("new admin key reads tenant %v", got)
	}
	s.must(s.do("GET", "/v1/api-keys", k.Key, nil), 200) // admin role
	var platform uuid.UUID
	if err := db.System.QueryRow(ctx, `SELECT id FROM tenants WHERE is_platform`).Scan(&platform); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.CreateAdminKey(ctx, db.System, platform); err == nil {
		t.Error("admin key for the platform tenant should be refused")
	}
	if _, err := admin.CreateAdminKey(ctx, db.System, uuid.New()); err == nil {
		t.Error("admin key for an unknown tenant should be refused")
	}
}
