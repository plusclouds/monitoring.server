package engine_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/admin"
	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/dbtest"
	"github.com/plusclouds/monitoring.server/internal/engine"
	"github.com/plusclouds/monitoring.server/internal/incident"
	"github.com/plusclouds/monitoring.server/internal/inventory"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
	_ "github.com/plusclouds/monitoring.server/plugins/all"
)

var db *dbtest.DB

func TestMain(m *testing.M) { dbtest.Main(m, &db) }

type fixture struct {
	tenant, device, check uuid.UUID
	eng                   *engine.Engine
}

func setup(t *testing.T, thresholds string) fixture {
	t.Helper()
	db.Reset(t)
	ctx := context.Background()
	res, err := admin.Bootstrap(ctx, db.System, admin.BootstrapOptions{
		Standalone: true, TenantName: "acme", TenantDefaults: config.Default().Platform.TenantDefaults,
	})
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{tenant: res.TenantID, device: uuid.Must(uuid.NewV7()), check: uuid.Must(uuid.NewV7())}
	parent := uuid.Must(uuid.NewV7())
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO devices (id, tenant_id, name, type, external_source, external_id) VALUES ($1, $2, 'rack-1', 'other', 'plusclouds', 'r-1')`,
			[]any{parent, f.tenant}},
		{`INSERT INTO devices (id, tenant_id, name, type, address, parent_id) VALUES ($1, $2, 'web', 'web', 'https://example.com', $3)`,
			[]any{f.device, f.tenant, parent}},
		{`INSERT INTO checks (id, tenant_id, device_id, name, plugin, interval_seconds, failure_count, thresholds)
		  VALUES ($1, $2, $3, 'home', 'http', 60, 2, $4)`, []any{f.check, f.tenant, f.device, thresholds}},
	} {
		if _, err := db.System.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	f.eng, err = engine.New(engine.Options{System: db.System, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fixture) apply(t *testing.T, status plugin.Status, totalMS float64) {
	t.Helper()
	metrics := plugin.NaNs(7)
	metrics[4] = totalMS // total_ms in the http plugin's layout
	out, err := f.eng.Apply(context.Background(), runner.Result{
		TenantID: f.tenant, DeviceID: f.device, CheckID: f.check, Plugin: "http", Interval: time.Minute,
		Scheduled: time.Now(), Result: plugin.Result{Status: status, Output: "GET: " + status.String(), Metrics: metrics, Time: time.Now()},
	})
	if err != nil || out != "applied" {
		t.Fatalf("apply: %q, %v", out, err)
	}
}

type event struct {
	Type     string
	Envelope incident.Envelope
	Raw      map[string]any
}

func events(t *testing.T) []event {
	t.Helper()
	rows, err := db.System.Query(context.Background(), `SELECT type, envelope FROM events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (event, error) {
		var e event
		var raw []byte
		err := r.Scan(&e.Type, &raw)
		if err == nil {
			err = json.Unmarshal(raw, &e.Envelope)
		}
		if err == nil {
			err = json.Unmarshal(raw, &e.Raw)
		}
		return e, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func activeIncidents(t *testing.T) int {
	t.Helper()
	var n int
	if err := db.System.QueryRow(context.Background(), `SELECT count(*) FROM incidents WHERE status <> 'resolved'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// F05, ADR-0009: failure_count results open an incident with one event in
// the CloudEvents envelope; recovery resolves it with another.
func TestIncidentLifecycle(t *testing.T) {
	f := setup(t, `[]`)
	f.apply(t, plugin.Critical, 10)
	if activeIncidents(t) != 0 || len(events(t)) != 0 {
		t.Fatal("one failure must not open an incident")
	}
	f.apply(t, plugin.Critical, 10)
	f.apply(t, plugin.Critical, 10)
	evs := events(t)
	if activeIncidents(t) != 1 || len(evs) != 1 || evs[0].Type != incident.EventOpened {
		t.Fatalf("after failures: %d incidents, events %+v", activeIncidents(t), evs)
	}
	e := evs[0].Envelope
	if e.SpecVersion != "1.0" || !strings.HasSuffix(e.Source, "/"+f.tenant.String()) || e.DataContentType != "application/json" {
		t.Errorf("envelope: %+v", e)
	}
	data := evs[0].Raw["data"].(map[string]any)
	if data["object_type"] != `Monitoring\Incidents` || data["account_id"] != nil {
		t.Errorf("data: %v", data)
	}
	dev := data["device"].(map[string]any)
	path := dev["path"].([]any)
	if dev["name"] != "web" || len(path) != 1 || path[0].(map[string]any)["external"].(map[string]any)["id"] != "r-1" {
		t.Errorf("device and path: %v", dev)
	}
	if obj := data["object"].(map[string]any); obj["severity"] != "critical" || obj["status"] != "open" {
		t.Errorf("object: %v", obj)
	}

	var phase, status string
	if err := db.System.QueryRow(context.Background(), `SELECT phase, status FROM check_state WHERE check_id = $1`, f.check).
		Scan(&phase, &status); err != nil || phase != "PROBLEM" || status != "CRITICAL" {
		t.Errorf("state: %s %s %v", phase, status, err)
	}

	f.apply(t, plugin.OK, 10)
	evs = events(t)
	if activeIncidents(t) != 0 || len(evs) != 2 || evs[1].Type != incident.EventResolved {
		t.Fatalf("after recovery: %+v", evs)
	}
}

// F05: thresholds drive incidents; severity changes update them.
func TestThresholdIncident(t *testing.T) {
	f := setup(t, `[{"id": "slow", "name": "Slow page", "metric": "total_ms",
		"warning": {"op": ">", "value": 800}, "critical": {"op": ">", "value": 2000}}]`)
	f.apply(t, plugin.OK, 900)
	f.apply(t, plugin.OK, 900)
	f.apply(t, plugin.OK, 2500)
	evs := events(t)
	if len(evs) != 2 || evs[0].Type != incident.EventOpened || evs[1].Type != incident.EventUpdated {
		t.Fatalf("events: %+v", evs)
	}
	var sev, summary, rule string
	if err := db.System.QueryRow(context.Background(), `SELECT severity, summary, rule_id FROM incidents`).
		Scan(&sev, &summary, &rule); err != nil || sev != "critical" || rule != "slow" || !strings.Contains(summary, "Slow page") {
		t.Errorf("incident: %s %q %s %v", sev, summary, rule, err)
	}
}

// Deleting a check ends its incident so receivers see the resolution.
func TestDeletingCheckResolvesIncident(t *testing.T) {
	f := setup(t, `[]`)
	f.apply(t, plugin.Critical, 1)
	f.apply(t, plugin.Critical, 1)
	err := pgx.BeginFunc(context.Background(), db.System, func(tx pgx.Tx) error {
		return inventory.DeleteCheck(context.Background(), tx, audit.Actor{Kind: audit.ActorSystem}, f.check)
	})
	if err != nil {
		t.Fatal(err)
	}
	evs := events(t)
	if activeIncidents(t) != 0 || evs[len(evs)-1].Type != incident.EventResolved {
		t.Fatalf("events: %+v", evs)
	}
	if by := evs[len(evs)-1].Raw["data"].(map[string]any)["object"].(map[string]any)["resolved_by"]; by != "check-deleted" {
		t.Errorf("resolved_by = %v", by)
	}
}

// v0.3.1: checks of a suspended tenant do not run on purpose, so the stale
// sweep leaves their state alone; an active tenant's stale check turns UNKNOWN.
func TestSweepSkipsSuspendedTenants(t *testing.T) {
	f := setup(t, `[]`)
	f.apply(t, plugin.OK, 1)
	ctx := context.Background()
	if _, err := db.System.Exec(ctx, `UPDATE check_state SET last_result_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	sweep := func() string {
		t.Helper()
		eng, err := engine.New(engine.Options{System: db.System, Results: make(chan runner.Result),
			SweepEvery: 20 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		run, stop := context.WithTimeout(ctx, 200*time.Millisecond)
		defer stop()
		_ = eng.Run(run)
		var status string
		if err := db.System.QueryRow(ctx, `SELECT status FROM check_state WHERE check_id = $1`, f.check).Scan(&status); err != nil {
			t.Fatal(err)
		}
		return status
	}
	if _, err := db.System.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, f.tenant); err != nil {
		t.Fatal(err)
	}
	if s := sweep(); s != "OK" {
		t.Errorf("suspended tenant: status %s, want OK", s)
	}
	if _, err := db.System.Exec(ctx, `UPDATE tenants SET status = 'active' WHERE id = $1`, f.tenant); err != nil {
		t.Fatal(err)
	}
	if s := sweep(); s != "UNKNOWN" {
		t.Errorf("active tenant: status %s, want UNKNOWN", s)
	}
}
