package api_test

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/store"
	"github.com/plusclouds/monitoring.server/internal/usage"
)

func openPeriods(t *testing.T, tenant string) map[string]int {
	t.Helper()
	rows, err := db.System.Query(context.Background(), `SELECT plugin, count(*) FROM check_periods p
		JOIN tenants t ON t.id = p.tenant_id WHERE t.external_id = $1 AND ended_at IS NULL GROUP BY 1`, tenant)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var p string
		var n int
		if err := rows.Scan(&p, &n); err != nil {
			t.Fatal(err)
		}
		out[p] = n
	}
	return out
}

// F13: billable periods follow every change of a check, device and tenant,
// whatever path makes it, because triggers keep them.
func TestUsagePeriods(t *testing.T) {
	e := setup(t, false)
	k := e.tenantKey("acc")
	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "web", "type": "web"}), 201))
	e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{"name": "ping", "plugin": "icmp"}), 201)
	web := e.id(e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{"name": "home", "plugin": "http"}), 201))
	if p := openPeriods(t, "acc"); p["icmp"] != 1 || p["http"] != 1 {
		t.Fatalf("after create: %v", p)
	}
	e.must(e.do("PATCH", "/v1/checks/"+web, k, map[string]any{"enabled": false}), 200)
	if p := openPeriods(t, "acc"); p["http"] != 0 {
		t.Errorf("disabled check still billed: %v", p)
	}
	e.must(e.do("PATCH", "/v1/checks/"+web, k, map[string]any{"enabled": true}), 200)
	e.must(e.do("PATCH", "/v1/checks/"+web, k, map[string]any{"interval_seconds": 120}), 200)
	var n int
	if err := db.System.QueryRow(context.Background(), `SELECT count(*) FROM check_periods WHERE check_id = $1`, web).Scan(&n); err != nil || n != 3 {
		t.Errorf("http periods = %d (want 3: before disable, after enable, after interval change), %v", n, err)
	}
	cur := e.must(e.do("GET", "/v1/usage/current", k, nil), 200)
	if cur.body["checks"] != float64(2) || cur.body["weighted_checks"] != float64(2) {
		// No weights recorded yet: every plugin weighs 1.
		t.Errorf("current: %s", cur.raw)
	}

	e.must(e.do("PATCH", "/v1/tenants/by-external-id/acc", e.platformKey, map[string]any{"status": "suspended"}), 200)
	if p := openPeriods(t, "acc"); len(p) != 0 {
		t.Errorf("suspended tenant still billed: %v", p)
	}
	e.must(e.do("PATCH", "/v1/tenants/by-external-id/acc", e.platformKey, map[string]any{"status": "active"}), 200)
	if p := openPeriods(t, "acc"); p["icmp"] != 1 || p["http"] != 1 {
		t.Errorf("after resume: %v", p)
	}
	e.must(e.do("DELETE", "/v1/devices/"+dev, k, nil), 204)
	if p := openPeriods(t, "acc"); len(p) != 0 {
		t.Errorf("deleted device's checks still billed: %v", p)
	}
	if err := db.System.QueryRow(context.Background(), `SELECT count(*) FROM device_periods WHERE device_id = $1 AND ended_at IS NULL`, dev).
		Scan(&n); err != nil || n != 0 {
		t.Errorf("deleted device still counted: %d, %v", n, err)
	}
}

func usageQuery(from, to time.Time, extra string) string {
	return "/v1/usage/tenants?from=" + url.QueryEscape(from.Format(time.RFC3339)) +
		"&to=" + url.QueryEscape(to.Format(time.RFC3339)) + extra
}

// F13: closed hours are weighted check-seconds per tenant, served to the
// platform key; open hours are refused; corrections are new revisions.
func TestUsageHours(t *testing.T) {
	e := setup(t, false)
	ctx := context.Background()
	acc := e.tenantKey("acc")
	e.tenantKey("bcc")
	now := time.Now().UTC()
	h := now.Truncate(time.Hour).Add(-3 * time.Hour)
	cfg := config.Usage{Weights: map[string]float64{"icmp": 1, "http": 2}, DefaultWeight: 1, WhoopsyMultiplier: 5}
	// icmp, http, the default and the Whoopsy! multiplier.
	if changes, err := usage.SyncWeights(ctx, db.System, cfg, now); err != nil || len(changes) != 4 {
		t.Fatalf("first weights: %v, %v", changes, err)
	}

	// Periods in the hour h: an icmp check from :15 to :40, two http checks
	// all hour and one device all hour for acc; one icmp check for bcc.
	// Only the triggers write periods, so the test writes them as the owner.
	owner, err := store.Open(ctx, store.PoolOptions{DSN: db.OwnerDSN})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	for _, p := range []struct {
		tenant, plugin string
		from, to       time.Duration
	}{{"acc", "icmp", 15 * time.Minute, 40 * time.Minute}, {"acc", "http", 0, time.Hour},
		{"acc", "http", -time.Hour, 2 * time.Hour}, {"bcc", "icmp", 0, time.Hour}} {
		if _, err := owner.Exec(ctx, `INSERT INTO check_periods (tenant_id, check_id, device_id, plugin, interval_seconds, started_at, ended_at)
			SELECT id, $2, $2, $3, 60, $4, $5 FROM tenants WHERE external_id = $1`,
			p.tenant, uuid.New(), p.plugin, h.Add(p.from), h.Add(p.to)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := owner.Exec(ctx, `INSERT INTO device_periods (tenant_id, device_id, started_at, ended_at)
		SELECT id, gen_random_uuid(), $1, $2 FROM tenants WHERE external_id = 'acc'`, h, h.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(ctx, `INSERT INTO usage_close (closed_until) VALUES ($1)
		ON CONFLICT (singleton) DO UPDATE SET closed_until = excluded.closed_until`, h); err != nil {
		t.Fatal(err)
	}
	closed, err := usage.CloseHours(ctx, db.System, 5*time.Minute, now)
	if err != nil || len(closed) < 2 {
		t.Fatalf("closed %v, %v", closed, err)
	}
	if again, err := usage.CloseHours(ctx, db.System, 5*time.Minute, now); err != nil || len(again) != 0 {
		t.Errorf("closing twice: %v, %v", again, err)
	}

	r := e.must(e.do("GET", usageQuery(h, h.Add(2*time.Hour), ""), e.platformKey, nil), 200)
	items := r.body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items: %s", r.raw)
	}
	a := items[0].(map[string]any)
	bd := a["breakdown"].(map[string]any)
	if a["account_id"] != "acc" || a["billable_check_seconds"] != float64(1500+2*3600*2) || a["device_seconds"] != float64(3600) ||
		a["revision"] != float64(1) || bd["icmp"].(map[string]any)["count_seconds"] != float64(1500) ||
		bd["http"].(map[string]any)["weight"] != float64(2) || a["period_end"] != h.Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("acc hour: %v", a)
	}
	// The long http period also counts in the next hour; bcc only in h.
	if b := items[1].(map[string]any); b["account_id"] != "bcc" || b["billable_check_seconds"] != float64(3600) {
		t.Errorf("bcc hour: %v", b)
	}
	if c := items[2].(map[string]any); c["account_id"] != "acc" || c["billable_check_seconds"] != float64(7200) {
		t.Errorf("acc second hour: %v", c)
	}

	page := e.must(e.do("GET", usageQuery(h, h.Add(2*time.Hour), "&limit=2"), e.platformKey, nil), 200)
	next := page.body["next_cursor"].(string)
	rest := e.must(e.do("GET", usageQuery(h, h.Add(2*time.Hour), "&limit=2&cursor="+next), e.platformKey, nil), 200)
	if l := rest.body["items"].([]any); len(l) != 1 || rest.body["next_cursor"] != nil {
		t.Errorf("second page: %s", rest.raw)
	}

	open := now.Truncate(time.Hour)
	if p := e.must(e.do("GET", usageQuery(h, open.Add(time.Hour), ""), e.platformKey, nil), 422); p.body["type"] !=
		"https://monitor.plusclouds.com/problems/usage-hour-open" {
		t.Errorf("open hour: %s", p.raw)
	}
	e.must(e.do("GET", usageQuery(h.Add(time.Minute), h.Add(time.Hour), ""), e.platformKey, nil), 422)
	e.must(e.do("GET", usageQuery(h, h.Add(time.Hour), ""), acc, nil), 403)
	own := e.must(e.do("GET", "/v1/usage/hours?from="+url.QueryEscape(h.Format(time.RFC3339))+"&to="+
		url.QueryEscape(h.Add(time.Hour).Format(time.RFC3339)), acc, nil), 200)
	if l := own.body["items"].([]any); len(l) != 1 || l[0].(map[string]any)["account_id"] != "acc" {
		t.Errorf("own hours: %s", own.raw)
	}

	// A weight change applies from the next hour; a correction is a revision.
	cfg.Weights["http"] = 3
	changes, err := usage.SyncWeights(ctx, db.System, cfg, now)
	if err != nil || len(changes) != 1 || !changes[0].EffectiveFrom.Equal(now.Truncate(time.Hour).Add(time.Hour)) {
		t.Fatalf("weight change: %v, %v", changes, err)
	}
	if again, _ := usage.SyncWeights(ctx, db.System, cfg, now); len(again) != 0 {
		t.Errorf("unchanged config recorded %v", again)
	}
	if n := auditCount(t, "usage.weight.update"); n != 5 {
		t.Errorf("usage.weight.update events = %d, want 5", n)
	}
	w := e.must(e.do("GET", "/v1/usage/weights", acc, nil), 200)
	if w.body["weights"].(map[string]any)["http"] != float64(2) {
		t.Errorf("weights now: %s", w.raw)
	}
	if hours, err := usage.Recompute(ctx, db.System, h, h.Add(time.Hour), "test"); err != nil || len(hours) != 1 {
		t.Fatalf("recompute: %v, %v", hours, err)
	}
	r = e.must(e.do("GET", usageQuery(h, h.Add(time.Hour), ""), e.platformKey, nil), 200)
	if a := r.body["items"].([]any)[0].(map[string]any); a["revision"] != float64(2) || a["billable_check_seconds"] != float64(15900) {
		t.Errorf("revision 2 (same weights for a past hour): %v", a)
	}
}
