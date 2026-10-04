package api_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

type routeData struct {
	Data struct {
		Route struct {
			Step   int               `json:"step"`
			Labels map[string]string `json:"labels"`
		} `json:"route"`
		Links map[string]string `json:"links"`
		Check struct {
			Thresholds []any `json:"thresholds"`
		} `json:"check"`
	} `json:"data"`
}

func decodeRoute(t *testing.T, b []byte) routeData {
	t.Helper()
	var r routeData
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// F06: escalation steps follow the notification with their own labels, and
// stop for an acknowledged incident when the step asks for that.
func TestEscalationSteps(t *testing.T) {
	x := newNoise(t, 0, func(n *config.Notifier) {
		n.IncidentURLTemplate = "https://panel.example/incidents/{incident_id}?t={tenant_id}"
	})
	bad := func(steps []any) {
		t.Helper()
		x.must(x.do("POST", "/v1/alert-routes", x.key, map[string]any{"name": "bad", "endpoint_id": x.hook, "steps": steps}), 422)
	}
	bad([]any{map[string]any{"after_seconds": 60}})
	bad([]any{map[string]any{"after_seconds": 0}, map[string]any{"after_seconds": 0}})
	bad([]any{map[string]any{"after_seconds": 0}, map[string]any{"after_seconds": 60,
		"schedule": map[string]any{"timezone": "Mars/Base", "from": "08:00", "to": "18:00"}}})

	r := x.must(x.do("POST", "/v1/alert-routes", x.key, map[string]any{"name": "esc", "endpoint_id": x.hook,
		"labels": map[string]any{"team": "infra"}, "steps": []any{
			map[string]any{"after_seconds": 0, "labels": map[string]any{"recipient": "noc"}},
			map[string]any{"after_seconds": 300, "labels": map[string]any{"recipient": "on-call"}},
			map[string]any{"after_seconds": 900, "labels": map[string]any{"recipient": "manager"}, "only_if_unacknowledged": true,
				"schedule": map[string]any{"timezone": "Europe/Istanbul", "from": "00:00", "to": "23:59"}},
		}}), 201)
	if len(r.body["steps"].([]any)) != 3 {
		t.Fatalf("route: %s", r.raw)
	}
	d := x.device("web")
	chk := x.id(x.must(x.do("POST", "/v1/devices/"+d+"/checks", x.key, map[string]any{"name": "web", "plugin": "http",
		"failure_count": 1, "thresholds": []any{map[string]any{"metric": "total_ms", "critical": map[string]any{"op": ">", "value": 2000}}}}), 201))
	x.result(d, chk, "http", plugin.Critical)
	x.tick()
	opened := decodeRoute(t, x.rcv.wait(t, "monitoring.incident.opened", 2*time.Second).body)
	inc := x.incident(chk)["id"].(string)
	if opened.Data.Route.Step != 0 || opened.Data.Route.Labels["recipient"] != "noc" || opened.Data.Route.Labels["team"] != "infra" {
		t.Errorf("step 0: %+v", opened.Data.Route)
	}
	if opened.Data.Links["incident"] != "https://panel.example/incidents/"+inc+"?t="+x.tenant.String() {
		t.Errorf("link: %v", opened.Data.Links)
	}
	if len(opened.Data.Check.Thresholds) != 1 {
		t.Errorf("thresholds: %v", opened.Data.Check.Thresholds)
	}

	due := func(step int) {
		if _, err := db.System.Exec(context.Background(),
			`UPDATE route_escalations SET due_at = now() - interval '1 second' WHERE step = $1`, step); err != nil {
			t.Fatal(err)
		}
	}
	x.tick()
	if n := len(x.rcv.types()); n != 1 {
		t.Fatalf("escalated early: %v", x.rcv.types())
	}
	due(1)
	x.tick()
	esc := decodeRoute(t, x.rcv.wait(t, "monitoring.incident.escalated", 2*time.Second).body)
	if esc.Data.Route.Step != 1 || esc.Data.Route.Labels["recipient"] != "on-call" {
		t.Errorf("step 1: %+v", esc.Data.Route)
	}
	x.must(x.do("POST", "/v1/incidents/"+inc+"/ack", x.key, nil), 200)
	x.tick()
	n := len(x.rcv.types())
	due(2)
	x.tick()
	time.Sleep(200 * time.Millisecond)
	if got := x.rcv.types(); len(got) != n {
		t.Errorf("acknowledged incident escalated: %v", got)
	}
	var outcome string
	if err := db.System.QueryRow(context.Background(), `SELECT outcome FROM route_escalations WHERE step = 2`).Scan(&outcome); err != nil ||
		outcome != "skipped-acknowledged" {
		t.Errorf("step 2 outcome %q, %v", outcome, err)
	}
}

// F06: the route test shows which routes a sample incident reaches.
func TestAlertRouteTest(t *testing.T) {
	x := newNoise(t, 0)
	x.must(x.do("POST", "/v1/alert-routes", x.key, map[string]any{"name": "critical", "endpoint_id": x.hook,
		"match": map[string]any{"severity": []string{"critical"}}}), 201)
	x.must(x.do("POST", "/v1/alert-routes", x.key, map[string]any{"name": "rest", "endpoint_id": x.hook}), 201)
	d := x.device("web")
	chk := x.check(d, "web", "http", false)
	res := func(body map[string]any) []any {
		return x.must(x.do("POST", "/v1/alert-routes/test", x.key, body), 200).body["routes"].([]any)
	}
	l := res(map[string]any{"check_id": chk})
	a, b := l[0].(map[string]any), l[1].(map[string]any)
	if a["matched"] != true || a["notifies"] != true || b["matched"] != true || b["notifies"] != false {
		t.Errorf("critical: %v", l)
	}
	l = res(map[string]any{"device_id": d, "severity": "warning"})
	a, b = l[0].(map[string]any), l[1].(map[string]any)
	if a["matched"] != false || b["notifies"] != true {
		t.Errorf("warning: %v", l)
	}
	x.must(x.do("POST", "/v1/alert-routes/test", x.key, map[string]any{"check_id": "01a10000-0000-7000-8000-000000000000"}), 422)
}

// F06: an endpoint's failed deliveries can be sent again in bulk.
func TestBulkReplay(t *testing.T) {
	x := newNoise(t, 0, func(n *config.Notifier) { n.GiveUpAfter = config.Duration(time.Nanosecond) })
	x.must(x.do("POST", "/v1/alert-routes", x.key, map[string]any{"name": "all", "endpoint_id": x.hook}), 201)
	x.rcv.code.Store(500)
	d := x.device("web")
	x.result(d, x.check(d, "web", "http", false), "http", plugin.Critical)
	x.tick()
	x.rcv.wait(t, "monitoring.incident.opened", 2*time.Second)
	time.Sleep(100 * time.Millisecond)
	l := x.must(x.do("GET", "/v1/webhooks/"+x.hook+"/deliveries?status=failed", x.key, nil), 200).body["items"].([]any)
	if len(l) != 1 {
		t.Fatalf("failed deliveries: %v", l)
	}
	x.rcv.code.Store(200)
	q := "/v1/webhooks/" + x.hook + "/deliveries/replay?from=" + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) +
		"&to=" + time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if r := x.must(x.do("POST", q, x.key, nil), 202); r.body["replayed"] != float64(1) {
		t.Fatalf("replay: %s", r.raw)
	}
	x.tick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l = x.must(x.do("GET", "/v1/webhooks/"+x.hook+"/deliveries?status=delivered", x.key, nil), 200).body["items"].([]any)
		if len(l) == 1 {
			return
		}
		time.Sleep(50 * time.Millisecond)
		x.tick()
	}
	t.Errorf("replayed delivery not delivered")
}

// v0.4.1: webhooks and alert routes count against tenant limits.
func TestWebhookLimits(t *testing.T) {
	e := setup(t, false)
	k := e.tenantKey("acc")
	r := e.must(e.do("PATCH", "/v1/tenants/by-external-id/acc", e.platformKey,
		map[string]any{"limits": map[string]any{"max_webhooks": 1, "max_alert_routes": 1}}), 200)
	if l := r.body["limits"].(map[string]any); l["max_webhooks"] != float64(1) || l["max_alert_routes"] != float64(1) {
		t.Fatalf("limits: %s", r.raw)
	}
	hook := e.id(e.must(e.do("POST", "/v1/webhooks", k, map[string]any{"name": "a", "url": "https://93.184.215.14/a"}), 201))
	if p := e.must(e.do("POST", "/v1/webhooks", k, map[string]any{"name": "b", "url": "https://93.184.215.14/b"}), 409); p.body["type"] !=
		"https://monitor.plusclouds.com/problems/limit-reached" {
		t.Errorf("webhook limit: %s", p.raw)
	}
	e.must(e.do("POST", "/v1/alert-routes", k, map[string]any{"name": "a", "endpoint_id": hook}), 201)
	e.must(e.do("POST", "/v1/alert-routes", k, map[string]any{"name": "b", "endpoint_id": hook}), 409)
	// Defaults for a new tenant.
	l := e.must(e.do("PUT", "/v1/tenants/by-external-id/bcc", e.platformKey, map[string]any{"name": "b"}), 201).body["limits"].(map[string]any)
	if l["max_webhooks"] != float64(20) || l["max_alert_routes"] != float64(50) {
		t.Errorf("default limits: %v", l)
	}
}

// v0.4.1: a webhook URL the tenant's network policy blocks is refused when
// it is saved, not only at delivery.
func TestWebhookURLPolicy(t *testing.T) {
	e := setup(t, true)
	k := e.adminKey
	for _, u := range []string{"http://127.0.0.1:9/h", "http://localhost/h", "http://10.1.2.3/h", "http://169.254.169.254/latest"} {
		if p := e.must(e.do("POST", "/v1/webhooks", k, map[string]any{"name": "x", "url": u}), 422); !strings.Contains(p.raw, "url") {
			t.Errorf("%s: %s", u, p.raw)
		}
	}
	id := e.id(e.must(e.do("POST", "/v1/webhooks", k, map[string]any{"name": "ok", "url": "https://93.184.215.14/h"}), 201))
	e.must(e.do("PUT", "/v1/webhooks/"+id, k, map[string]any{"name": "ok", "url": "http://10.1.2.3/h"}), 422)
	e.must(e.do("PUT", "/v1/webhooks/by-external-id/n8n", k, map[string]any{"name": "n8n", "url": "http://10.1.2.3/h"}), 422)
	if _, err := db.System.Exec(context.Background(),
		`UPDATE tenants SET allowed_target_networks = '{10.0.0.0/8}' WHERE NOT is_platform`); err != nil {
		t.Fatal(err)
	}
	e.must(e.do("PUT", "/v1/webhooks/"+id, k, map[string]any{"name": "ok", "url": "http://10.1.2.3/h"}), 200)
	// The operator deny list wins over the tenant's allowed networks.
	e.must(e.do("POST", "/v1/webhooks", k, map[string]any{"name": "meta", "url": "http://169.254.169.254/"}), 422)
}

// v0.4.1: finished deliveries and their events are deleted after the
// delivery retention.
func TestDeliveryRetention(t *testing.T) {
	x := newNoise(t, 0, func(n *config.Notifier) { n.DeliveryRetention = config.Duration(time.Millisecond) })
	x.must(x.do("POST", "/v1/alert-routes", x.key, map[string]any{"name": "all", "endpoint_id": x.hook}), 201)
	d := x.device("web")
	x.result(d, x.check(d, "web", "http", false), "http", plugin.Critical)
	x.tick()
	x.rcv.wait(t, "monitoring.incident.opened", 2*time.Second)
	time.Sleep(100 * time.Millisecond)
	n, err := x.n.Cleanup(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("cleanup: %d, %v", n, err)
	}
	var events int
	if err := db.System.QueryRow(context.Background(), `SELECT count(*) FROM events`).Scan(&events); err != nil || events != 0 {
		t.Errorf("events left: %d, %v", events, err)
	}
	if l := x.must(x.do("GET", "/v1/webhooks/"+x.hook+"/deliveries", x.key, nil), 200).body["items"].([]any); len(l) != 0 {
		t.Errorf("deliveries left: %v", l)
	}
}
