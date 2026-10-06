package api_test

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/metrics"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// port is an snmp.interfaces object: oper status and in_errors_rate set,
// the other metrics not collected.
func port(key string, st plugin.Status, inErrors float64) plugin.Object {
	m := plugin.NaNs(8)
	m[2], m[6] = inErrors, 1
	if st != plugin.OK {
		m[6] = 2
	}
	return plugin.Object{Key: key, Name: key, Labels: map[string]string{"alias": "a-" + key}, Status: st,
		Output: map[bool]string{true: "up", false: "down"}[st == plugin.OK], Metrics: m}
}

// M4: a collector's objects have their own state and incidents; thresholds
// can target one object; an object the device stops reporting resolves; a
// failed collection is an incident of the check itself.
func TestCollectorObjects(t *testing.T) {
	ctx := context.Background()
	x := newNoise(t, 0)
	x.must(x.do("POST", "/v1/alert-routes", x.key, map[string]any{"name": "all", "endpoint_id": x.hook}), 201)
	dev := x.device("access-sw")
	x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, map[string]any{"name": "ports", "plugin": "snmp.interfaces",
		"is_host_check": true}), 422)
	x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, map[string]any{"name": "web", "plugin": "http",
		"thresholds": []any{map[string]any{"metric": "total_ms", "object": "x", "critical": map[string]any{"op": ">", "value": 1}}}}), 422)
	chk := x.id(x.must(x.do("POST", "/v1/devices/"+dev+"/checks", x.key, map[string]any{"name": "ports",
		"plugin": "snmp.interfaces", "failure_count": 1,
		"thresholds": []any{map[string]any{"metric": "in_errors_rate", "object": "Gi1/0/1",
			"critical": map[string]any{"op": ">", "value": 10}}}}), 201))

	w, err := metrics.NewWriter(metrics.WriterOptions{System: db.System})
	if err != nil {
		t.Fatal(err)
	}
	apply := func(st plugin.Status, objects []plugin.Object) {
		t.Helper()
		r := runner.Result{TenantID: x.tenant, DeviceID: uuid.MustParse(dev), CheckID: uuid.MustParse(chk),
			Plugin: "snmp.interfaces", Interval: time.Minute,
			Result: plugin.Result{Status: st, Output: "batch", Time: time.Now(), Objects: objects}}
		w.Add(r)
		if _, err := x.eng.Apply(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	active := func(q string) []any {
		return x.must(x.do("GET", "/v1/incidents?status=active&check_id="+chk+q, x.key, nil), 200).body["items"].([]any)
	}

	// Gi1/0/2 is down: an incident of that object.
	apply(plugin.OK, []plugin.Object{port("Gi1/0/1", plugin.OK, 0), port("Gi1/0/2", plugin.Critical, 0)})
	inc := active("&object_key=Gi1/0/2")
	if len(inc) != 1 || len(active("")) != 1 {
		t.Fatalf("incidents after a port went down: %v", active(""))
	}
	i := inc[0].(map[string]any)
	if i["object_name"] != "Gi1/0/2" || i["summary"] != "Gi1/0/2: down" || i["severity"] != "critical" {
		t.Errorf("object incident: %v", i)
	}
	x.tick()
	m := x.rcv.wait(t, "monitoring.incident.opened", 2*time.Second)
	var ev struct {
		Data struct{ Object map[string]any } `json:"data"`
	}
	if err := json.Unmarshal(m.body, &ev); err != nil || ev.Data.Object["object_key"] != "Gi1/0/2" {
		t.Errorf("event object: %s", m.body)
	}

	objs := x.must(x.do("GET", "/v1/checks/"+chk+"/objects", x.key, nil), 200).body["items"].([]any)
	if len(objs) != 2 {
		t.Fatalf("objects: %v", objs)
	}
	o2 := objs[1].(map[string]any)
	if o2["key"] != "Gi1/0/2" || o2["status"] != "CRITICAL" || o2["incident_id"] != i["id"] ||
		o2["labels"].(map[string]any)["alias"] != "a-Gi1/0/2" || o2["last_metrics"].(map[string]any)["oper_status"] != float64(2) {
		t.Errorf("object state: %v", o2)
	}

	// Errors on Gi1/0/1 cross its own threshold; Gi1/0/2 keeps its incident.
	apply(plugin.OK, []plugin.Object{port("Gi1/0/1", plugin.OK, 50), port("Gi1/0/2", plugin.Critical, 0)})
	if got := active(""); len(got) != 2 {
		t.Fatalf("after errors on Gi1/0/1: %v", got)
	}
	if i := active("&object_key=Gi1/0/1")[0].(map[string]any); i["rule_id"] != "r1" {
		t.Errorf("threshold incident: %v", i)
	}

	// Gi1/0/1 recovers and Gi1/0/2 is no longer reported.
	apply(plugin.OK, []plugin.Object{port("Gi1/0/1", plugin.OK, 0)})
	if got := active(""); len(got) != 0 {
		t.Fatalf("after recovery: %v", got)
	}
	gone := x.must(x.do("GET", "/v1/incidents/"+i["id"].(string), x.key, nil), 200).body
	if gone["resolved_by"] != "object-gone" {
		t.Errorf("gone object's incident: %v", gone)
	}
	if l := x.must(x.do("GET", "/v1/checks/"+chk+"/objects?include_gone=false", x.key, nil), 200).body["items"].([]any); len(l) != 1 {
		t.Errorf("objects without gone ones: %v", l)
	}

	// The device stops answering: an incident of the check, objects untouched.
	apply(plugin.Critical, nil)
	got := active("")
	if len(got) != 1 || got[0].(map[string]any)["object_key"] != nil {
		t.Fatalf("failed collection: %v", got)
	}
	if l := x.must(x.do("GET", "/v1/checks/"+chk+"/objects", x.key, nil), 200).body["items"].([]any); len(l) != 2 {
		t.Errorf("objects after a failed run: %v", l)
	}

	// Metrics are stored per object.
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	var keys []string
	if err := db.System.QueryRow(ctx, `SELECT array_agg(DISTINCT object_key ORDER BY object_key) FROM metric_groups
		WHERE source_id = $1`, chk).Scan(&keys); err != nil || len(keys) != 2 || keys[0] != "Gi1/0/1" {
		t.Fatalf("metric groups: %v %v", keys, err)
	}
	// Grafana sees the object's name next to its series.
	graf, err := pgx.Connect(ctx, strings.Replace(db.SystemDSN, "monitor_system:change-me-system", "monitor_grafana:change-me-grafana", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = graf.Close(ctx) }()
	var v float64
	if err := graf.QueryRow(ctx, `SELECT value FROM metrics_raw r JOIN metric_series_v s USING (series_id)
		WHERE r.check_id = $1 AND r.object = 'Gi1/0/1' AND r.name = 'in_errors_rate' AND s.object_name = 'Gi1/0/1'
		ORDER BY time DESC LIMIT 1`, chk).Scan(&v); err != nil || math.Abs(v) > 0 {
		t.Errorf("latest in_errors_rate of Gi1/0/1: %v %v", v, err)
	}
	var objIncidents int
	if err := graf.QueryRow(ctx, `SELECT count(*) FROM incidents_v WHERE check_id = $1 AND object_key IS NOT NULL`,
		chk).Scan(&objIncidents); err != nil || objIncidents != 2 {
		t.Errorf("incidents_v object incidents: %d %v", objIncidents, err)
	}
}
