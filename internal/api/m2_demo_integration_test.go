package api_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/credential"
	"github.com/plusclouds/monitoring.server/internal/engine"
	"github.com/plusclouds/monitoring.server/internal/execute"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/internal/webhook"
)

type received struct {
	id, sig string
	ts      int64
	body    []byte
	typ     string
}

// receiver is a webhook endpoint that records what it gets and answers
// with the status in code.
type receiver struct {
	*httptest.Server
	mu   sync.Mutex
	msgs []received
	code atomic.Int64
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{}
	r.code.Store(200)
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		ts, _ := strconv.ParseInt(req.Header.Get("webhook-timestamp"), 10, 64)
		var env struct{ Type string }
		_ = json.Unmarshal(b, &env)
		r.mu.Lock()
		r.msgs = append(r.msgs, received{req.Header.Get("webhook-id"), req.Header.Get("webhook-signature"), ts, b, env.Type})
		r.mu.Unlock()
		w.WriteHeader(int(r.code.Load()))
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.msgs))
	for i, m := range r.msgs {
		out[i] = m.typ
	}
	return out
}

func (r *receiver) wait(t *testing.T, typ string, within time.Duration) received {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		for _, m := range r.msgs {
			if m.typ == typ {
				r.mu.Unlock()
				return m
			}
		}
		r.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no %s within %v; got %v", typ, within, r.types())
	return received{}
}

// startPipeline runs runner, engine and notifier against the test database
// until the test ends.
func startPipeline(t *testing.T) {
	t.Helper()
	cfg := config.Default()
	cfg.Runner.ResyncInterval = config.Duration(time.Hour)
	cfg.Notifier.PollInterval = config.Duration(100 * time.Millisecond)
	cfg.Notifier.RetrySchedule = []config.Duration{config.Duration(200 * time.Millisecond)}
	log := slog.New(slog.DiscardHandler)
	keys := testKeys(t)
	exec, err := execute.New(cfg, &credential.Store{Keys: keys}, log)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan runner.Result, 100)
	run, err := runner.New(runner.Options{Config: cfg.Runner, System: db.System, ListenDSN: db.SystemDSN,
		Executor: exec, Sink: results, Logger: log, Standby: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.New(engine.Options{System: db.System, Results: results, Writers: 2, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	n, err := webhook.New(webhook.Options{System: db.System, Store: &webhook.Store{Keys: sharedKeys},
		Sender: webhook.NewSender(cfg), Config: cfg.Notifier, Logger: log, Registry: prometheus.NewRegistry()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() { _ = run.Run(ctx) })
	wg.Go(func() { _ = eng.Run(ctx) })
	wg.Go(func() { _ = n.Run(ctx) })
	t.Cleanup(func() { cancel(); wg.Wait() })
}

// M2 demo (F02, F04, F05, F06): add a device with a check; stop the target;
// a signed monitoring.incident.opened webhook arrives; the target recovers
// and monitoring.incident.resolved follows.
func TestM2Demo(t *testing.T) {
	e := setup(t, true)
	k := e.adminKey
	if _, err := db.System.Exec(context.Background(),
		`UPDATE tenants SET allowed_target_networks = '{127.0.0.0/8}' WHERE NOT is_platform`); err != nil {
		t.Fatal(err)
	}
	var up atomic.Bool
	up.Store(true)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer target.Close()
	rcv := newReceiver(t)

	hook := e.must(e.do("POST", "/v1/webhooks", k, map[string]any{
		"name": "n8n", "url": rcv.URL, "headers": map[string]any{"X-Api-Token": "t0ken"}}), 201)
	secret := hook.body["secret"].(string)
	e.must(e.do("POST", "/v1/alert-routes", k, map[string]any{
		"name": "everything", "endpoint_id": hook.body["id"], "labels": map[string]any{"recipient": "noc"}}), 201)

	dev := e.id(e.must(e.do("POST", "/v1/devices", k, map[string]any{"name": "web", "type": "web", "address": target.URL}), 201))
	chk := e.id(e.must(e.do("POST", "/v1/devices/"+dev+"/checks", k, map[string]any{"name": "home", "plugin": "http"}), 201))
	// One-second interval and immediate incidents keep the test short; the API
	// enforces the tenant's minimum interval, so set them directly.
	if _, err := db.System.Exec(context.Background(),
		`UPDATE checks SET interval_seconds = 1, failure_count = 2 WHERE id = $1`, chk); err != nil {
		t.Fatal(err)
	}
	startPipeline(t)

	time.Sleep(2 * time.Second)
	if len(rcv.types()) != 0 {
		t.Fatalf("healthy target produced events: %v", rcv.types())
	}
	up.Store(false)
	opened := rcv.wait(t, "monitoring.incident.opened", 10*time.Second)
	if !webhook.Verify(secret, opened.id, opened.ts, opened.body, opened.sig) {
		t.Fatal("signature does not verify with the endpoint secret")
	}
	var env map[string]any
	_ = json.Unmarshal(opened.body, &env)
	data := env["data"].(map[string]any)
	if data["route"].(map[string]any)["labels"].(map[string]any)["recipient"] != "noc" {
		t.Errorf("route labels: %v", data["route"])
	}
	if data["object"].(map[string]any)["severity"] != "critical" || data["device"].(map[string]any)["name"] != "web" {
		t.Errorf("payload: %s", opened.body)
	}

	up.Store(true)
	rcv.wait(t, "monitoring.incident.resolved", 10*time.Second)

	deliveries := e.must(e.do("GET", "/v1/webhooks/"+hook.body["id"].(string)+"/deliveries", k, nil), 200)
	for _, it := range deliveries.body["items"].([]any) {
		if s := it.(map[string]any)["status"]; s != "delivered" {
			t.Errorf("delivery status %v", s)
		}
	}
}
