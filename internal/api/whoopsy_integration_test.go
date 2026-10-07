package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/internal/store"
	"github.com/plusclouds/monitoring.server/internal/usage"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Whoopsy!: turned on per check through its own endpoint, an alert when
// the response time leaves its moving band for N results in a row, and the
// check billed at five times its weight while it is on.
func TestWhoopsyCheck(t *testing.T) {
	ctx := context.Background()
	x := newNoise(t, 0)
	x.must(x.do("POST", "/v1/alert-routes", x.key, map[string]any{"name": "all", "endpoint_id": x.hook}), 201)
	if _, err := usage.SyncWeights(ctx, db.System, config.Default().Usage, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	dev := x.device("shop")
	chk := x.id(x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, map[string]any{"name": "home", "plugin": "http",
		"config": map[string]any{"url": "https://shop.example.com/"}}), 201))
	path := "/v1/checks/" + chk + "/whoopsy"

	if st := x.must(x.do("GET", path, x.key, nil), 200).body; st["enabled"] != false || st["settings"] != nil {
		t.Fatalf("off by default: %v", st)
	}
	st := x.must(x.do("PUT", path, x.key, map[string]any{}), 200).body
	set := st["settings"].(map[string]any)
	if st["enabled"] != true || set["metric"] != "total_ms" || set["window"] != float64(7) || set["deviations"] != float64(1) ||
		set["consecutive"] != float64(3) || set["direction"] != "above" || set["severity"] != "warning" {
		t.Fatalf("defaults: %v", st)
	}
	x.must(x.do("PUT", path, x.key, map[string]any{"window": 7, "consecutive": 3, "severity": "critical"}), 200)
	x.must(x.do("PUT", path, x.key, map[string]any{"metric": "nope"}), 422)
	x.must(x.do("PUT", path, x.key, map[string]any{"window": 2}), 400)

	// A full replace of the check does not touch Whoopsy!.
	x.must(x.do("PUT", "/v1/checks/"+chk, x.key, map[string]any{"name": "home", "plugin": "http",
		"config": map[string]any{"url": "https://shop.example.com/"}}), 200)
	if c := x.must(x.do("GET", "/v1/checks/"+chk, x.key, nil), 200).body; c["whoopsy"] == nil {
		t.Fatalf("a check update turned Whoopsy! off: %v", c["whoopsy"])
	}

	ports := x.id(x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, map[string]any{"name": "ports", "plugin": "snmp.interfaces"}), 201))
	x.must(x.do("PUT", "/v1/checks/"+ports+"/whoopsy", x.key, map[string]any{}), 422)
	sys := x.id(x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, map[string]any{"name": "sys", "plugin": "snmp.system"}), 201))
	x.must(x.do("PUT", "/v1/checks/"+sys+"/whoopsy", x.key, map[string]any{}), 422) // no default metric
	x.must(x.do("PUT", "/v1/checks/"+sys+"/whoopsy", x.key, map[string]any{"metric": "cpu_percent"}), 200)

	// Response times around 500 ms, then three slow ones.
	feed := func(ms float64) {
		t.Helper()
		m := plugin.NaNs(7)
		m[4] = ms
		if _, err := x.eng.Apply(ctx, runner.Result{TenantID: x.tenant, DeviceID: uuid.MustParse(dev), CheckID: uuid.MustParse(chk),
			Plugin: "http", Interval: time.Minute, Result: plugin.Result{Status: plugin.OK, Output: "200 OK", Time: time.Now(), Metrics: m}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []float64{400, 500, 600, 400, 500, 600, 500} {
		feed(v)
	}
	feed(900)
	feed(950)
	if l := x.must(x.do("GET", "/v1/incidents?status=active&check_id="+chk, x.key, nil), 200).body["items"].([]any); len(l) != 0 {
		t.Fatalf("alert before three in a row: %v", l)
	}
	feed(1000)
	inc := x.incident(chk)
	if inc["rule_id"] != "whoopsy" || inc["rule_name"] != "Whoopsy!" || inc["severity"] != "critical" ||
		!strings.HasPrefix(inc["summary"].(string), "Whoopsy!: total_ms = 1000, above ") {
		t.Fatalf("incident: %v", inc)
	}
	x.tick()
	m := x.rcv.wait(t, "monitoring.incident.opened", 2*time.Second)
	var ev struct {
		Data struct {
			Check struct {
				RuleID string `json:"rule_id"`
			}
		} `json:"data"`
	}
	if err := json.Unmarshal(m.body, &ev); err != nil || ev.Data.Check.RuleID != "whoopsy" {
		t.Errorf("webhook: %s", m.body)
	}
	// The band of every result, as the engine judged it.
	hist := x.must(x.do("GET", path+"/band", x.key, nil), 200).body["points"].([]any)
	if len(hist) != 10 {
		t.Fatalf("band history: %d points", len(hist))
	}
	first, eighth, last := hist[0].(map[string]any), hist[7].(map[string]any), hist[9].(map[string]any)
	if first["mean"] != nil || first["upper"] != nil || first["outside"] != false || first["value"] != float64(400) {
		t.Errorf("learning point: %v", first)
	}
	if !approx(eighth["mean"], 500) || !approx(eighth["upper"], 500+81.64965809277261) || eighth["lower"] != nil ||
		eighth["outside"] != true || eighth["hits"] != float64(1) {
		t.Errorf("first slow result: %v", eighth)
	}
	// The band is frozen: the third slow result is judged against the same band.
	if !approx(last["mean"], 500) || last["hits"] != float64(3) || last["alerting"] != true {
		t.Errorf("third slow result: %v", last)
	}
	old := time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	x.must(x.do("GET", path+"/band?from="+old, x.key, nil), 422)

	band := x.must(x.do("GET", path, x.key, nil), 200).body["band"].(map[string]any)
	if band["points"] != float64(7) || band["consecutive_hits"] != float64(3) || band["alerting"] != true ||
		band["last_value"] != float64(1000) || band["upper"] == nil {
		t.Errorf("band: %v", band)
	}

	// Billing: the period changed when Whoopsy! was turned on.
	cur := x.must(x.do("GET", "/v1/usage/current", x.key, nil), 200).body["by_plugin"].(map[string]any)
	if w := cur["http+whoopsy"].(map[string]any); w["checks"] != float64(1) || w["weight"] != float64(10) {
		t.Errorf("current usage: %v", cur)
	}
	if mult := x.must(x.do("GET", "/v1/usage/weights", x.key, nil), 200).body; mult["whoopsy_multiplier"] != float64(5) {
		t.Errorf("weights: %v", mult)
	}
	// A closed hour bills "<plugin>+whoopsy" at the plugin's weight times five.
	h := time.Now().UTC().Truncate(time.Hour).Add(-6 * time.Hour)
	owner, err := store.Open(ctx, store.PoolOptions{DSN: db.OwnerDSN}) // only triggers write periods
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := owner.Exec(ctx, `UPDATE check_periods SET started_at = $2 WHERE check_id = $1 AND whoopsy`, chk, h); err != nil {
		t.Fatal(err)
	}
	if _, err := db.System.Exec(ctx, `SELECT usage_close_hour($1, 1, NULL)`, h); err != nil {
		t.Fatal(err)
	}
	var weight float64
	var secs int64
	if err := db.System.QueryRow(ctx, `SELECT weight::float8, count_seconds FROM usage_hour_plugins
		WHERE plugin = 'http+whoopsy' AND period_start = $1`, h).Scan(&weight, &secs); err != nil || weight != 10 || secs != 3600 {
		t.Errorf("closed hour: weight %v, %d s, %v", weight, secs, err)
	}

	// Reset: the band is learned again, and the incident resolves.
	st = x.must(x.do("POST", path+"/reset", x.key, nil), 200).body
	if st["band"] != nil {
		t.Errorf("band after reset: %v", st["band"])
	}
	feed(1000)
	if l := x.must(x.do("GET", "/v1/incidents?status=active&check_id="+chk, x.key, nil), 200).body["items"].([]any); len(l) != 0 {
		t.Errorf("incident after reset: %v", l)
	}

	x.must(x.do("DELETE", path, x.key, nil), 204)
	x.must(x.do("POST", path+"/reset", x.key, nil), 409)
	if st := x.must(x.do("GET", path, x.key, nil), 200).body; st["enabled"] != false {
		t.Errorf("after disable: %v", st)
	}
	var n int
	if err := db.System.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action LIKE 'check.whoopsy.%' AND object_id = $1`,
		chk).Scan(&n); err != nil || n != 4 {
		t.Errorf("audit: %d %v", n, err)
	}
	if err := db.System.QueryRow(ctx, `SELECT count(*) FROM check_periods WHERE check_id = $1 AND ended_at IS NULL AND NOT whoopsy`,
		chk).Scan(&n); err != nil || n != 1 {
		t.Errorf("billing back to http: %d %v", n, err)
	}
}
