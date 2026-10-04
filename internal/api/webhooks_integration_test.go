package api_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/engine"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/internal/webhook"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func newNotifier(t *testing.T) *webhook.Notifier {
	t.Helper()
	cfg := config.Default()
	cfg.Notifier.RetrySchedule = []config.Duration{config.Duration(time.Millisecond)}
	n, err := webhook.New(webhook.Options{System: db.System, Store: &webhook.Store{Keys: sharedKeys},
		Sender: webhook.NewSender(cfg), Config: cfg.Notifier, Logger: slog.New(slog.DiscardHandler),
		Registry: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// incidentFixture creates a device with a check and returns a function that
// feeds it a result through the engine.
func (e *env) incidentFixture(devType string) func(plugin.Status) {
	e.t.Helper()
	k := e.adminKey
	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "d-" + devType, "type": devType}), 201))
	chk := e.id(e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{"name": "c", "plugin": "http", "failure_count": 1}), 201))
	tenant := e.must(e.do("GET", "/v1/tenant", k, nil), 200).body["id"].(string)
	eng, err := engine.New(engine.Options{System: db.System, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		e.t.Fatal(err)
	}
	return func(st plugin.Status) {
		if _, err := eng.Apply(context.Background(), runner.Result{
			TenantID: uuid.MustParse(tenant), DeviceID: uuid.MustParse(dev), CheckID: uuid.MustParse(chk), Plugin: "http",
			Interval: time.Minute, Result: plugin.Result{Status: st, Output: "x", Time: time.Now()},
		}); err != nil {
			e.t.Fatal(err)
		}
	}
}

func allowLoopback(t *testing.T) {
	t.Helper()
	if _, err := db.System.Exec(context.Background(),
		`UPDATE tenants SET allowed_target_networks = '{127.0.0.0/8}' WHERE NOT is_platform`); err != nil {
		t.Fatal(err)
	}
}

// ADR-0009: failed deliveries retry; the receiver gets each event once it
// answers 2xx, in order per incident.
func TestWebhookRetryAndOrder(t *testing.T) {
	e := setup(t, true)
	allowLoopback(t)
	rcv := newReceiver(t)
	rcv.code.Store(500)
	hook := e.must(e.do("POST", "/v1/webhooks", e.adminKey, map[string]any{"name": "h", "url": rcv.URL}), 201)
	e.must(e.do("POST", "/v1/alert-routes", e.adminKey, map[string]any{"name": "all", "endpoint_id": hook.body["id"]}), 201)
	feed := e.incidentFixture("web")
	feed(plugin.Critical)
	feed(plugin.OK)

	n := newNotifier(t)
	n.Tick(context.Background())
	// Only the first event is attempted: the second waits behind it.
	if got := rcv.types(); len(got) != 1 || got[0] != "monitoring.incident.opened" {
		t.Fatalf("first tick: %v", got)
	}
	rcv.code.Store(200)
	for range 5 {
		time.Sleep(5 * time.Millisecond)
		n.Tick(context.Background())
	}
	got := rcv.types()
	if len(got) != 3 || got[1] != "monitoring.incident.opened" || got[2] != "monitoring.incident.resolved" {
		t.Fatalf("after recovery of the receiver: %v", got)
	}
	d := e.must(e.do("GET", "/v1/webhooks/"+hook.body["id"].(string)+"/deliveries", e.adminKey, nil), 200)
	for _, it := range d.body["items"].([]any) {
		m := it.(map[string]any)
		if m["status"] != "delivered" {
			t.Errorf("delivery: %v", m)
		}
		if m["event_type"] == "monitoring.incident.opened" && m["attempts"] != float64(2) {
			t.Errorf("opened attempts = %v, want 2", m["attempts"])
		}
	}

	// Replay sends a delivered event again.
	first := d.body["items"].([]any)[1].(map[string]any)["id"].(string)
	e.must(e.do("POST", "/v1/webhooks/"+hook.body["id"].(string)+"/deliveries/"+first+"/replay", e.adminKey, nil), 202)
	n.Tick(context.Background())
	if len(rcv.types()) != 4 {
		t.Errorf("replay: %v", rcv.types())
	}
}

// ADR-0009: 410 Gone disables the endpoint.
func TestWebhookGoneDisables(t *testing.T) {
	e := setup(t, true)
	allowLoopback(t)
	rcv := newReceiver(t)
	rcv.code.Store(410)
	hook := e.must(e.do("POST", "/v1/webhooks", e.adminKey, map[string]any{"name": "h", "url": rcv.URL}), 201)
	e.must(e.do("POST", "/v1/alert-routes", e.adminKey, map[string]any{"name": "all", "endpoint_id": hook.body["id"]}), 201)
	e.incidentFixture("web")(plugin.Critical)
	newNotifier(t).Tick(context.Background())
	got := e.must(e.do("GET", "/v1/webhooks/"+hook.body["id"].(string), e.adminKey, nil), 200)
	if got.body["enabled"] != false || !strings.Contains(got.body["disabled_reason"].(string), "410") {
		t.Errorf("endpoint after 410: %s", got.raw)
	}
}

// Routes: first match wins unless continue; filters by severity and device type.
func TestAlertRouteMatching(t *testing.T) {
	e := setup(t, true)
	allowLoopback(t)
	k := e.adminKey
	a, b, c := newReceiver(t), newReceiver(t), newReceiver(t)
	hook := func(name, url string) any {
		return e.must(e.do("POST", "/v1/webhooks", k, map[string]any{"name": name, "url": url}), 201).body["id"]
	}
	ha, hb, hc := hook("a", a.URL), hook("b", b.URL), hook("c", c.URL)
	e.must(e.do("POST", "/v1/alert-routes", k, map[string]any{"name": "servers", "endpoint_id": ha,
		"match": map[string]any{"device_types": []string{"server"}}, "continue": true}), 201)
	e.must(e.do("POST", "/v1/alert-routes", k, map[string]any{"name": "critical", "endpoint_id": hb,
		"match": map[string]any{"severity": []string{"critical"}}}), 201)
	e.must(e.do("POST", "/v1/alert-routes", k, map[string]any{"name": "rest", "endpoint_id": hc}), 201)

	e.incidentFixture("server")(plugin.Critical)
	e.incidentFixture("web")(plugin.Warning)
	newNotifier(t).Tick(context.Background())
	if len(a.types()) != 1 || len(b.types()) != 1 || len(c.types()) != 1 {
		t.Errorf("a %v, b %v, c %v; want one each (server critical -> a and b, web warning -> c)",
			a.types(), b.types(), c.types())
	}
}

// ADR-0009: during rotation deliveries carry both signatures; headers and
// secrets never appear in responses, the database in clear, or audit.
func TestWebhookSecretsAndRotation(t *testing.T) {
	e := setup(t, true)
	allowLoopback(t)
	k := e.adminKey
	rcv := newReceiver(t)
	created := e.must(e.do("POST", "/v1/webhooks", k, map[string]any{"name": "h", "url": rcv.URL,
		"headers": map[string]any{"Authorization": "Bearer MARKER-header"}}), 201)
	id := created.body["id"].(string)
	oldSecret := created.body["secret"].(string)
	if !strings.HasPrefix(oldSecret, "whsec_") {
		t.Fatalf("secret %q", oldSecret)
	}
	got := e.must(e.do("GET", "/v1/webhooks/"+id, k, nil), 200)
	if strings.Contains(got.raw, "MARKER") || strings.Contains(got.raw, oldSecret) || got.body["secret"] != nil {
		t.Fatalf("GET leaks secrets: %s", got.raw)
	}
	rot := e.must(e.do("POST", "/v1/webhooks/"+id+"/rotate-secret", k, nil), 200)
	newSecret := rot.body["secret"].(string)
	if newSecret == oldSecret || rot.body["previous_valid_until"] == nil {
		t.Fatalf("rotation: %s", rot.raw)
	}

	test := e.must(e.do("POST", "/v1/webhooks/"+id+"/test", k, nil), 200)
	if test.body["ok"] != true {
		t.Fatalf("test: %s", test.raw)
	}
	m := rcv.wait(t, "monitoring.webhook.test", time.Second)
	if !webhook.Verify(oldSecret, m.id, m.ts, m.body, m.sig) || !webhook.Verify(newSecret, m.id, m.ts, m.body, m.sig) {
		t.Errorf("during rotation both secrets must verify: %s", m.sig)
	}

	var leaks int
	if err := db.System.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM webhook_endpoints
		         WHERE position(convert_to('MARKER', 'UTF8') IN ciphertext) > 0 OR position(convert_to($1, 'UTF8') IN ciphertext) > 0)
		     + (SELECT count(*) FROM audit_events
		         WHERE coalesce(before::text, '') || coalesce(after::text, '') LIKE '%MARKER%'
		            OR coalesce(after::text, '') LIKE '%' || $1 || '%')`, newSecret).Scan(&leaks); err != nil || leaks != 0 {
		t.Errorf("secret material stored in clear: %d (err %v)", leaks, err)
	}
}

// Design section 8: a customer tenant's webhooks cannot reach private
// addresses unless its allowed networks include them.
func TestWebhookSSRFGuard(t *testing.T) {
	e := setup(t, true)
	rcv := newReceiver(t)
	id := e.must(e.do("POST", "/v1/webhooks", e.adminKey, map[string]any{"name": "h", "url": rcv.URL}), 201).body["id"].(string)
	r := e.must(e.do("POST", "/v1/webhooks/"+id+"/test", e.adminKey, nil), 200)
	if r.body["ok"] != false || !strings.Contains(r.body["error"].(string), "network policy") || len(rcv.types()) != 0 {
		t.Errorf("SSRF guard: %s", r.raw)
	}
}
