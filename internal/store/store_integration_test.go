package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/plusclouds/monitoring.server/internal/admin"
	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/auth"
	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/dbtest"
	"github.com/plusclouds/monitoring.server/internal/store"
)

var db *dbtest.DB

func TestMain(m *testing.M) { dbtest.Main(m, &db) }

func standalone(t *testing.T, name string) admin.BootstrapResult {
	t.Helper()
	db.Reset(t)
	res, err := admin.Bootstrap(context.Background(), db.System, admin.BootstrapOptions{
		Standalone: true, TenantName: name, TenantDefaults: config.Default().Platform.TenantDefaults,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func addTenant(t *testing.T, name string) uuid.UUID {
	t.Helper()
	id := uuid.Must(uuid.NewV7())
	_, err := db.System.Exec(context.Background(), `
		INSERT INTO tenants (id, name, max_devices, max_checks, min_check_interval_seconds, api_rate_per_minute)
		VALUES ($1, $2, 10, 10, 30, 60)`, id, name)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMigrationsDownAndUp(t *testing.T) {
	ctx := context.Background()
	if _, err := store.Migrate(ctx, db.OwnerDSN, "down"); err != nil {
		t.Fatalf("down: %v", err)
	}
	if _, err := store.Migrate(ctx, db.OwnerDSN, "up"); err != nil {
		t.Fatalf("up again: %v", err)
	}
	st, err := store.MigrationStatus(ctx, db.OwnerDSN)
	if err != nil || len(st) == 0 {
		t.Fatalf("status: %v, %d migrations", err, len(st))
	}
	for _, s := range st {
		if s.State != "applied" {
			t.Errorf("migration %d is %s", s.Source.Version, s.State)
		}
	}
}

func TestBootstrapRunsOnce(t *testing.T) {
	res := standalone(t, "acme")
	if res.AdminKey == "" || res.PlatformKey != "" || res.TenantID == uuid.Nil {
		t.Fatalf("unexpected result %+v", res)
	}
	_, err := admin.Bootstrap(context.Background(), db.System, admin.BootstrapOptions{})
	if !errors.Is(err, admin.ErrAlreadyBootstrapped) {
		t.Fatalf("second bootstrap: got %v, want ErrAlreadyBootstrapped", err)
	}
}

// ADR-0008: the app role sees only the tenant set in app.tenant_id, and
// nothing without one.
func TestRLSIsolatesTenants(t *testing.T) {
	res := standalone(t, "acme")
	other := addTenant(t, "other")
	ctx := context.Background()

	count := func(tenant uuid.UUID, table string) int {
		var n int
		err := store.InTenant(ctx, db.App, tenant, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&n)
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(res.TenantID, "tenants"); n != 1 {
		t.Errorf("acme sees %d tenants, want 1", n)
	}
	if n := count(res.TenantID, "api_keys"); n != 1 {
		t.Errorf("acme sees %d api keys, want 1", n)
	}
	if n := count(other, "api_keys"); n != 0 {
		t.Errorf("other tenant sees %d api keys, want 0", n)
	}
	if n := count(other, "audit_events"); n != 0 {
		t.Errorf("other tenant sees %d audit events, want 0", n)
	}
	var n int
	if err := db.App.QueryRow(ctx, "SELECT count(*) FROM tenants").Scan(&n); err != nil || n != 0 {
		t.Errorf("without app.tenant_id the app role sees %d tenants (err %v), want 0", n, err)
	}
}

// The API finds a key before it knows the tenant only through auth_lookup_key.
func TestAuthLookupKey(t *testing.T) {
	res := standalone(t, "acme")
	prefix, err := auth.ParseKey(res.AdminKey)
	if err != nil {
		t.Fatal(err)
	}
	var tenant uuid.UUID
	var hash []byte
	var role string
	err = db.App.QueryRow(context.Background(),
		"SELECT tenant_id, hash, role FROM auth_lookup_key($1)", prefix).Scan(&tenant, &hash, &role)
	if err != nil {
		t.Fatal(err)
	}
	if tenant != res.TenantID || role != "admin" || !auth.MatchKey(res.AdminKey, hash) {
		t.Errorf("lookup returned tenant %s role %s", tenant, role)
	}
}

// ADR-0008: a migration test fails if a table with tenant_id has RLS disabled.
func TestEveryTenantTableHasRLS(t *testing.T) {
	rows, err := db.System.Query(context.Background(), `
		SELECT c.relname FROM pg_class c
		  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'public'
		 WHERE c.relkind IN ('r', 'p') AND NOT c.relrowsecurity
		   AND EXISTS (SELECT 1 FROM pg_attribute a
		                WHERE a.attrelid = c.oid AND a.attname = 'tenant_id' AND NOT a.attisdropped)`)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("tables with tenant_id but without row-level security: %v", missing)
	}
}

// F01: the user mirror holds no personal data.
func TestUsersHaveNoPersonalData(t *testing.T) {
	var cols []string
	rows, err := db.System.Query(context.Background(),
		`SELECT column_name FROM information_schema.columns WHERE table_name = 'users'`)
	if err != nil {
		t.Fatal(err)
	}
	if cols, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		for _, bad := range []string{"email", "phone", "mobile", "birth", "national", "address"} {
			if contains(c, bad) {
				t.Errorf("users.%s looks like personal data", c)
			}
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestAuditIsAppendOnlyAndChained(t *testing.T) {
	res := standalone(t, "acme")
	ctx := context.Background()

	// Both application roles can write events; the chain continues.
	err := store.InTenant(ctx, db.App, res.TenantID, func(tx pgx.Tx) error {
		return audit.Write(ctx, tx, audit.Event{
			TenantID: res.TenantID, ActorKind: audit.ActorAPIKey, Action: "device.update",
			ObjectType: "device", ObjectID: "d1", Before: map[string]any{"name": "a"}, After: map[string]any{"name": "b"},
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := audit.Verify(ctx, db.System, nil); err != nil || len(p) != 0 {
		t.Fatalf("fresh chain: problems %v, err %v", p, err)
	}

	for _, pool := range []struct {
		name string
		exec func(string) error
	}{
		{"app", func(sql string) error {
			return store.InTenant(ctx, db.App, res.TenantID, func(tx pgx.Tx) error { _, err := tx.Exec(ctx, sql); return err })
		}},
		{"system", func(sql string) error { _, err := db.System.Exec(ctx, sql); return err }},
	} {
		for _, sql := range []string{
			"UPDATE audit_events SET action = 'x'",
			"DELETE FROM audit_events",
		} {
			var pgErr *pgconn.PgError
			if err := pool.exec(sql); !errors.As(err, &pgErr) || pgErr.Code != "42501" {
				t.Errorf("%s role: %q should be denied, got %v", pool.name, sql, err)
			}
		}
	}

	// A change made with owner rights (standing in for a superuser) is detected.
	owner, err := store.Open(ctx, store.PoolOptions{DSN: db.OwnerDSN})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.Exec(ctx, `UPDATE audit_events SET after = '{"name":"evil"}' WHERE tenant_id = $1 AND seq = 2`, res.TenantID); err != nil {
		t.Fatal(err)
	}
	problems, err := audit.Verify(ctx, db.System, &res.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || problems[0].Seq != 2 {
		t.Fatalf("tampering not detected at seq 2: %+v", problems)
	}
}
