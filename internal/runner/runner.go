// Package runner schedules and executes checks (F04). Scheduling and
// execution are one component, so there is no queue between them; results
// go to the engine through a bounded channel.
//
// MVP: one runner is active at a time. It holds a PostgreSQL session
// advisory lock on its LISTEN connection; other nodes with the runner role
// wait as hot standbys and take over when the lock is released. Shard leases
// for several active runners are phase 2.
package runner

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/execute"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Result is one check run, on its way to the engine.
type Result struct {
	TenantID  uuid.UUID
	DeviceID  uuid.UUID
	CheckID   uuid.UUID
	Plugin    string
	Interval  time.Duration
	Scheduled time.Time
	plugin.Result
	// ObjectDevices maps a collector object's key to the discovered device
	// it belongs to (a VM). The engine fills it before metrics are stored.
	ObjectDevices map[string]uuid.UUID
	// Layout is the metric layout of an ingester's check (its config names
	// the metrics); nil means the plugin manifest's.
	Layout []plugin.MetricDef
}

// Options configure a runner.
type Options struct {
	Config    config.Runner
	System    *pgxpool.Pool // BYPASSRLS: sees every tenant's checks
	ListenDSN string        // session connection for the lock and LISTEN
	Executor  *execute.Executor
	Sink      chan<- Result
	Logger    *slog.Logger
	Registry  prometheus.Registerer
	// Standby is how often a standby node tries to become active.
	Standby time.Duration
}

// Runner runs every enabled check of every active tenant.
type Runner struct {
	o   Options
	log *slog.Logger

	fast, slow chan struct{} // pool semaphores
	targetsMu  sync.Mutex
	targets    map[string]chan struct{} // per-target semaphores

	mu      sync.Mutex
	entries map[uuid.UUID]*entry
	q       queue
	wake    chan struct{}
	active  atomic.Bool

	lag        prometheus.Histogram
	executions *prometheus.CounterVec
	skipped    *prometheus.CounterVec
	inflight   *prometheus.GaugeVec
	checks     prometheus.Gauge
	activeG    prometheus.Gauge
}

type entry struct {
	job      execute.Job
	interval time.Duration
	phase    time.Duration
	slow     bool
	next     time.Time
	index    int
	running  atomic.Bool
}

// lockKey is the advisory lock that makes one runner active.
const lockKey = "monitor:runner"

// New builds a runner and registers its metrics.
func New(o Options) (*Runner, error) {
	if o.Standby == 0 {
		o.Standby = 5 * time.Second
	}
	fast, slow := max(o.Config.Pools.Fast, 1), max(o.Config.Pools.Slow, 1)
	r := &Runner{
		o: o, log: o.Logger,
		fast: make(chan struct{}, fast), slow: make(chan struct{}, slow),
		targets: map[string]chan struct{}{}, entries: map[uuid.UUID]*entry{}, wake: make(chan struct{}, 1),
		lag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "runner_check_lag_seconds", Help: "Actual start minus scheduled start of a check run.",
			Buckets: []float64{0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30},
		}),
		executions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "runner_executions_total", Help: "Check runs by plugin and status.",
		}, []string{"plugin", "status"}),
		skipped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "runner_skipped_total", Help: "Runs skipped because the previous run of the check was still going.",
		}, []string{"plugin"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "runner_inflight", Help: "Check runs in progress by pool.",
		}, []string{"pool"}),
		checks:  prometheus.NewGauge(prometheus.GaugeOpts{Name: "runner_checks", Help: "Checks scheduled on this node."}),
		activeG: prometheus.NewGauge(prometheus.GaugeOpts{Name: "runner_active", Help: "1 when this node is the active runner."}),
	}
	if o.Registry != nil {
		for _, c := range []prometheus.Collector{r.lag, r.executions, r.skipped, r.inflight, r.checks, r.activeG} {
			if err := o.Registry.Register(c); err != nil {
				return nil, err
			}
		}
	}
	return r, nil
}

// Active reports whether this node is the active runner (for readiness).
func (r *Runner) Active() bool { return r.active.Load() }

// Run takes the runner lock, then schedules checks until ctx is done or the
// lock connection fails, in which case it stops all scheduling at once and
// tries again.
func (r *Runner) Run(ctx context.Context) error {
	for {
		err := r.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			r.log.Warn("runner session ended", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.o.Standby):
		}
	}
}

// session is one lock connection's lifetime.
func (r *Runner) session(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, r.o.ListenDSN)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	for {
		var got bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, lockKey).Scan(&got); err != nil {
			return err
		}
		if got {
			break
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(r.o.Standby):
		}
	}
	if _, err := conn.Exec(ctx, `LISTEN config_changed; LISTEN run_now`); err != nil {
		return err
	}
	r.log.Info("runner active")
	r.active.Store(true)
	r.activeG.Set(1)
	defer func() {
		r.active.Store(false)
		r.activeG.Set(0)
		r.clear()
		r.log.Info("runner inactive")
	}()

	if err := r.reload(ctx); err != nil {
		return err
	}
	sctx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	// Stop scheduling and the listener before the connection closes (the
	// first defer), so nothing uses the connection after the lock is gone.
	defer func() { stop(); wg.Wait() }()
	wg.Go(func() { r.schedule(sctx) })

	notes := make(chan *pgconn.Notification)
	errc := make(chan error, 1)
	wg.Go(func() {
		for {
			n, err := conn.WaitForNotification(sctx)
			if err != nil {
				errc <- err
				return
			}
			select {
			case notes <- n:
			case <-sctx.Done():
				return
			}
		}
	})

	resync := time.NewTicker(max(r.o.Config.ResyncInterval.D(), time.Second))
	defer resync.Stop()
	var debounce <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-errc:
			return fmt.Errorf("lock connection: %w", err)
		case n := <-notes:
			switch n.Channel {
			case "run_now":
				id, err := uuid.Parse(n.Payload)
				if err != nil {
					continue
				}
				// A check created a moment ago may arrive before its
				// config_changed reload: load it first.
				if !r.has(id) {
					if err := r.reload(ctx); err != nil {
						r.log.Error("reload checks", "error", err)
					}
				}
				r.runNow(sctx, &wg, id)
			default:
				if debounce == nil {
					debounce = time.After(200 * time.Millisecond)
				}
			}
		case <-debounce:
			debounce = nil
			if err := r.reload(ctx); err != nil {
				r.log.Error("reload checks", "error", err)
			}
		case <-resync.C:
			if err := r.reload(ctx); err != nil {
				r.log.Error("resync checks", "error", err)
			}
		}
	}
}

// reload reads every enabled check of every active tenant and updates the
// schedule. Checks whose interval did not change keep their next run time,
// run state and plugin state.
func (r *Runner) reload(ctx context.Context) error {
	rows, err := r.o.System.Query(ctx, `
		SELECT c.id, c.tenant_id, c.device_id, d.address, c.plugin, c.config, c.interval_seconds,
		       c.timeout_seconds, t.is_platform, t.allowed_target_networks,
		       coalesce((SELECT jsonb_object_agg(cc.role, cc.credential_id)
		                   FROM check_credentials cc WHERE cc.check_id = c.id), '{}')
		  FROM checks c
		  JOIN devices d ON d.id = c.device_id
		  JOIN tenants t ON t.id = c.tenant_id
		 WHERE c.enabled AND t.status = 'active'`)
	if err != nil {
		return err
	}
	jobs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (execute.Job, error) {
		var j execute.Job
		var nets []netip.Prefix
		err := row.Scan(&j.CheckID, &j.Tenant.ID, &j.DeviceID, &j.Address, &j.Plugin, &j.Config,
			&j.IntervalSeconds, &j.TimeoutSeconds, &j.Tenant.IsPlatform, &nets, &j.Credentials)
		j.Tenant.AllowedTargetNetworks = nets
		return j, err
	})
	if err != nil {
		return err
	}

	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	next := make(map[uuid.UUID]*entry, len(jobs))
	for _, j := range jobs {
		// Ingesters do not run: their data is pushed (F08).
		if p, ok := plugin.Lookup(j.Plugin); ok && p.Manifest().Kind == plugin.KindIngester {
			continue
		}
		iv := time.Duration(j.IntervalSeconds) * time.Second
		e := r.entries[j.CheckID]
		if e == nil || e.interval != iv {
			e = &entry{interval: iv, phase: phase(j.CheckID, iv), index: -1}
			e.next = nextRun(now, iv, e.phase)
		}
		state := e.job.State
		if state == nil {
			state = &plugin.MemState{}
		}
		j.State = state
		e.job = j
		if p, ok := plugin.Lookup(j.Plugin); ok {
			e.slow = p.Manifest().Slow
		}
		next[j.CheckID] = e
	}
	r.entries = next
	r.q = r.q[:0]
	for _, e := range next {
		heap.Push(&r.q, e)
	}
	r.checks.Set(float64(len(next)))
	r.poke()
	return nil
}

func (r *Runner) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries, r.q = map[uuid.UUID]*entry{}, nil
	r.checks.Set(0)
}

func (r *Runner) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// schedule starts due checks until ctx is done.
func (r *Runner) schedule(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	var runs sync.WaitGroup
	defer runs.Wait()
	for {
		r.mu.Lock()
		now := time.Now()
		for len(r.q) > 0 && !r.q[0].next.After(now) {
			e := r.q[0]
			scheduled := e.next
			e.next = nextRun(now, e.interval, e.phase)
			heap.Fix(&r.q, 0)
			r.start(ctx, &runs, e, scheduled)
		}
		wait := time.Hour
		if len(r.q) > 0 {
			wait = time.Until(r.q[0].next)
		}
		r.mu.Unlock()

		timer.Reset(max(wait, 0))
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-r.wake:
		}
	}
}

func (r *Runner) has(id uuid.UUID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entries[id] != nil
}

// runNow starts a check at once (POST /checks/{id}/run-now), unless it is
// already running.
func (r *Runner) runNow(ctx context.Context, runs *sync.WaitGroup, id uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e := r.entries[id]; e != nil {
		r.start(ctx, runs, e, time.Now())
	}
}

// start runs an entry unless its previous run is still going (skipped, not
// queued: F04). The caller holds r.mu.
func (r *Runner) start(ctx context.Context, runs *sync.WaitGroup, e *entry, scheduled time.Time) {
	if !e.running.CompareAndSwap(false, true) {
		r.skipped.WithLabelValues(e.job.Plugin).Inc()
		return
	}
	job, slow, iv := e.job, e.slow, e.interval
	runs.Go(func() {
		defer e.running.Store(false)
		res, ok := r.execute(ctx, job, slow, scheduled)
		if !ok {
			return
		}
		select {
		case r.o.Sink <- Result{TenantID: job.Tenant.ID, DeviceID: job.DeviceID, CheckID: job.CheckID,
			Plugin: job.Plugin, Interval: iv, Scheduled: scheduled, Result: res}:
		case <-ctx.Done():
		}
	})
}

// execute waits for pool and per-target capacity, then runs the check.
func (r *Runner) execute(ctx context.Context, job execute.Job, slow bool, scheduled time.Time) (plugin.Result, bool) {
	pool, name := r.fast, "fast"
	if slow {
		pool, name = r.slow, "slow"
	}
	// The target first: checks queued behind a slow device must not hold
	// pool slots that checks of other devices could use.
	target := r.target(job.Address)
	for _, sem := range []chan struct{}{target, pool} {
		select {
		case sem <- struct{}{}:
			defer func(s chan struct{}) { <-s }(sem)
		case <-ctx.Done():
			return plugin.Result{}, false
		}
	}
	r.inflight.WithLabelValues(name).Inc()
	defer r.inflight.WithLabelValues(name).Dec()
	r.lag.Observe(time.Since(scheduled).Seconds())
	res := r.o.Executor.Run(ctx, r.o.System, job)
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
		return res, false // stopping: the result is an artifact of cancellation
	}
	r.executions.WithLabelValues(job.Plugin, res.Status.String()).Inc()
	return res, true
}

func (r *Runner) target(addr string) chan struct{} {
	r.targetsMu.Lock()
	defer r.targetsMu.Unlock()
	t, ok := r.targets[addr]
	if !ok {
		t = make(chan struct{}, max(r.o.Config.PerTargetConcurrency, 1))
		r.targets[addr] = t
	}
	return t
}
