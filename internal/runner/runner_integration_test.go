package runner_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/plusclouds/monitoring.server/internal/admin"
	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/credential"
	"github.com/plusclouds/monitoring.server/internal/crypto"
	"github.com/plusclouds/monitoring.server/internal/dbtest"
	"github.com/plusclouds/monitoring.server/internal/execute"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
	_ "github.com/plusclouds/monitoring.server/plugins/all"
)

var db *dbtest.DB

func TestMain(m *testing.M) { dbtest.Main(m, &db) }

// seed bootstraps a standalone install allowed to reach loopback and adds
// one device with an http check every second against url.
func seed(t *testing.T, url string) (tenant, check uuid.UUID) {
	t.Helper()
	db.Reset(t)
	ctx := context.Background()
	res, err := admin.Bootstrap(ctx, db.System, admin.BootstrapOptions{
		Standalone: true, TenantName: "acme", TenantDefaults: config.Default().Platform.TenantDefaults,
	})
	if err != nil {
		t.Fatal(err)
	}
	tenant = res.TenantID
	device, check := uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7())
	_, err = db.System.Exec(ctx, `
		UPDATE tenants SET allowed_target_networks = '{127.0.0.0/8}' WHERE id = $1;`, tenant)
	if err == nil {
		_, err = db.System.Exec(ctx, `INSERT INTO devices (id, tenant_id, name, type, address) VALUES ($1, $2, 'web', 'web', $3)`,
			device, tenant, url)
	}
	if err == nil {
		_, err = db.System.Exec(ctx, `INSERT INTO checks (id, tenant_id, device_id, name, plugin, interval_seconds)
			VALUES ($1, $2, $3, 'home', 'http', 1)`, check, tenant, device)
	}
	if err != nil {
		t.Fatal(err)
	}
	return tenant, check
}

func newRunner(t *testing.T, sink chan runner.Result) *runner.Runner {
	t.Helper()
	cfg := config.Default()
	keys, err := crypto.NewKeyring("k", map[string][]byte{"k": make([]byte, crypto.KeySize)})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	exec, err := execute.New(cfg, &credential.Store{Keys: keys}, log)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Runner.ResyncInterval = config.Duration(time.Hour)
	r, err := runner.New(runner.Options{
		Config: cfg.Runner, System: db.System, ListenDSN: db.SystemDSN, Executor: exec, Sink: sink,
		Logger: log, Standby: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func receive(t *testing.T, sink chan runner.Result, within time.Duration) runner.Result {
	t.Helper()
	select {
	case r := <-sink:
		return r
	case <-time.After(within):
		t.Fatal("no result")
		return runner.Result{}
	}
}

// F04: the runner runs enabled checks on their interval and sends results to
// the engine; disabling a check stops it after the change notification.
func TestRunnerRunsChecks(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits.Add(1) }))
	defer srv.Close()
	tenant, check := seed(t, srv.URL)

	sink := make(chan runner.Result, 100)
	r := newRunner(t, sink)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() { cancel(); <-done }()

	first := receive(t, sink, 5*time.Second)
	if first.CheckID != check || first.TenantID != tenant || first.Status != plugin.OK {
		t.Fatalf("unexpected result: %+v", first)
	}
	second := receive(t, sink, 3*time.Second)
	if gap := second.Scheduled.Sub(first.Scheduled); gap != time.Second {
		t.Errorf("runs %v apart, want 1s", gap)
	}

	if _, err := db.System.Exec(context.Background(), `UPDATE checks SET enabled = false WHERE id = $1`, check); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond) // notification, debounce, one run possibly in flight
	for len(sink) > 0 {
		<-sink
	}
	before := hits.Load()
	time.Sleep(2 * time.Second)
	if hits.Load() != before || len(sink) != 0 {
		t.Errorf("disabled check kept running: %d more requests", hits.Load()-before)
	}
}

// F04 (MVP): only one runner is active; a standby takes over when it stops.
func TestOneActiveRunner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	seed(t, srv.URL)

	sinkA, sinkB := make(chan runner.Result, 100), make(chan runner.Result, 100)
	a, b := newRunner(t, sinkA), newRunner(t, sinkB)
	ctxA, stopA := context.WithCancel(context.Background())
	ctxB, stopB := context.WithCancel(context.Background())
	doneA, doneB := make(chan error, 1), make(chan error, 1)
	go func() { doneA <- a.Run(ctxA) }()
	receive(t, sinkA, 5*time.Second)
	go func() { doneB <- b.Run(ctxB) }()
	defer func() { stopB(); <-doneB }()

	time.Sleep(1500 * time.Millisecond)
	if b.Active() || len(sinkB) != 0 {
		t.Fatal("standby runner ran checks while another was active")
	}
	stopA()
	<-doneA
	receive(t, sinkB, 5*time.Second)
	if !b.Active() {
		t.Error("standby did not become active")
	}
}

// F04: run-now for a check created a moment ago runs it, even before the
// change notification's reload.
func TestRunNowNewCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	_, check := seed(t, srv.URL)
	if _, err := db.System.Exec(context.Background(), `UPDATE checks SET interval_seconds = 3600 WHERE id = $1`, check); err != nil {
		t.Fatal(err)
	}
	sink := make(chan runner.Result, 10)
	r := newRunner(t, sink)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	defer func() { cancel(); <-done }()
	for !r.Active() {
		time.Sleep(20 * time.Millisecond)
	}

	device := uuid.Must(uuid.NewV7())
	fresh := uuid.Must(uuid.NewV7())
	_, err := db.System.Exec(context.Background(), `
		WITH d AS (INSERT INTO devices (id, tenant_id, name, type, address)
		           SELECT $1, tenant_id, 'web2', 'web', $3 FROM checks WHERE id = $4 RETURNING id, tenant_id)
		INSERT INTO checks (id, tenant_id, device_id, name, plugin, interval_seconds)
		SELECT $2, tenant_id, id, 'home', 'http', 3600 FROM d;
		`, device, fresh, srv.URL, check)
	if err == nil {
		_, err = db.System.Exec(context.Background(), `SELECT pg_notify('run_now', $1)`, fresh.String())
	}
	if err != nil {
		t.Fatal(err)
	}
	for {
		res := receive(t, sink, 3*time.Second)
		if res.CheckID == fresh {
			return
		}
	}
}
