package engine

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/incident"
	"github.com/plusclouds/monitoring.server/internal/inventory"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/internal/threshold"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Options configure the engine.
type Options struct {
	System   *pgxpool.Pool // BYPASSRLS
	Results  <-chan runner.Result
	Writers  int // state writers; results of one check always go to the same one
	Logger   *slog.Logger
	Registry prometheus.Registerer
	// SweepEvery is how often checks without recent results are marked
	// UNKNOWN (F05: no result for 3 intervals).
	SweepEvery time.Duration
	// Metrics receives every result for the metrics store (F07); nil
	// discards metrics. It must not block.
	Metrics interface{ Add(runner.Result) }
}

// Engine consumes results and maintains state and incidents.
type Engine struct {
	o   Options
	log *slog.Logger

	results   *prometheus.CounterVec
	writeTime prometheus.Histogram
	incidents *prometheus.CounterVec
}

// New builds an engine and registers its metrics.
func New(o Options) (*Engine, error) {
	if o.Writers <= 0 {
		o.Writers = 16
	}
	if o.SweepEvery <= 0 {
		o.SweepEvery = 30 * time.Second
	}
	e := &Engine{
		o: o, log: o.Logger,
		results: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "engine_results_total", Help: "Results processed, by outcome.",
		}, []string{"outcome"}),
		writeTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "engine_state_write_seconds", Help: "Time to apply one result to state.",
			Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
		}),
		incidents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "engine_incident_actions_total", Help: "Incidents opened, updated and resolved by the engine.",
		}, []string{"action"}),
	}
	if o.Registry != nil {
		for _, c := range []prometheus.Collector{e.results, e.writeTime, e.incidents} {
			if err := o.Registry.Register(c); err != nil {
				return nil, err
			}
		}
	}
	return e, nil
}

// Run processes results until ctx is done and the result channel drains.
func (e *Engine) Run(ctx context.Context) error {
	lanes := make([]chan runner.Result, e.o.Writers)
	var wg sync.WaitGroup
	for i := range lanes {
		lanes[i] = make(chan runner.Result, 64)
		wg.Go(func() {
			for r := range lanes[i] {
				e.handle(context.WithoutCancel(ctx), r)
			}
		})
	}
	wg.Go(func() { e.sweep(ctx) })
	defer func() {
		for _, l := range lanes {
			close(l)
		}
		wg.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case r := <-e.o.Results:
			h := fnv.New32a()
			_, _ = h.Write(r.CheckID[:])
			lanes[h.Sum32()%uint32(len(lanes))] <- r //nolint:gosec // len > 0
		}
	}
}

func (e *Engine) handle(ctx context.Context, r runner.Result) {
	start := time.Now()
	outcome, err := e.apply1(ctx, &r)
	if err != nil {
		e.log.Error("apply result", "check_id", r.CheckID, "error", err)
		outcome = "error"
	}
	e.results.WithLabelValues(outcome).Inc()
	e.writeTime.Observe(time.Since(start).Seconds())
	if e.o.Metrics != nil {
		// Late results too: they are metrics only. Objects of discovered
		// devices are stored under those devices.
		if r.ObjectDevices == nil && hasObjectDevices(r) {
			if r.ObjectDevices, err = e.objectDevices(ctx, r); err != nil {
				e.log.Error("object devices", "check_id", r.CheckID, "error", err)
			}
		}
		e.o.Metrics.Add(r)
	}
}

func hasObjectDevices(r runner.Result) bool {
	for _, o := range r.Objects {
		if o.Device != "" {
			return true
		}
	}
	return false
}

// objectDevices looks up the discovered devices of a result's objects,
// for results that did not go through Apply (late ones).
func (e *Engine) objectDevices(ctx context.Context, r runner.Result) (map[string]uuid.UUID, error) {
	rows, err := e.o.System.Query(ctx, `SELECT collector_key, id FROM devices WHERE managed_by = $1 AND collector_key IS NOT NULL`,
		r.CheckID.String())
	if err != nil {
		return nil, err
	}
	byKey := map[string]uuid.UUID{}
	var k string
	var id uuid.UUID
	if _, err := pgx.ForEachRow(rows, []any{&k, &id}, func() error { byKey[k] = id; return nil }); err != nil {
		return nil, err
	}
	out := map[string]uuid.UUID{}
	for _, o := range r.Objects {
		if d, ok := byKey[o.Device]; ok {
			out[o.Key] = d
		}
	}
	return out, nil
}

// Apply applies one result: state, and incidents with their events, in one
// transaction. It returns how the result was used.
func (e *Engine) Apply(ctx context.Context, r runner.Result) (string, error) {
	return e.apply1(ctx, &r)
}

// apply1 is Apply that also records the object devices in r.
func (e *Engine) apply1(ctx context.Context, r *runner.Result) (string, error) {
	// Late results (a probe's buffer) are metrics only, never state (F05).
	if r.Interval > 0 && time.Since(r.Time) > 2*r.Interval {
		return "late", nil
	}
	var outcome string
	for attempt := 0; ; attempt++ {
		err := pgx.BeginFunc(ctx, e.o.System, func(tx pgx.Tx) error {
			var err error
			outcome, err = e.apply(ctx, tx, r)
			return err
		})
		if err != nil {
			r.ObjectDevices = nil
		}
		// Two writers raced to open an incident for the check: re-read and retry.
		var pg *pgconn.PgError
		if attempt < 2 && errors.As(err, &pg) && pg.Code == "23505" {
			continue
		}
		return outcome, err
	}
}

func (e *Engine) apply(ctx context.Context, tx pgx.Tx, rp *runner.Result) (string, error) {
	r := *rp
	var cfg Config
	var enabled bool
	var deviceID uuid.UUID
	var hostCheck bool
	err := tx.QueryRow(ctx, `SELECT failure_count, recovery_count, unknown_is_critical, thresholds, enabled, device_id,
		       whoopsy, is_host_check FROM checks WHERE id = $1 FOR SHARE`, r.CheckID).
		Scan(&cfg.FailureCount, &cfg.RecoveryCount, &cfg.UnknownIsCritical, &cfg.Rules, &enabled, &deviceID, &cfg.Whoopsy,
			&hostCheck)
	if errors.Is(err, pgx.ErrNoRows) {
		return "check-gone", nil
	}
	if err != nil {
		return "", err
	}
	if !enabled {
		return "check-disabled", nil
	}
	// A collector's metrics and thresholds are per object; its own state
	// follows only the run's status (device unreachable, collection failed).
	var objCfg *Config
	var manifest plugin.Manifest
	if p, ok := plugin.Lookup(r.Plugin); ok {
		m := p.Manifest()
		manifest = m
		for _, d := range m.Metrics {
			cfg.Metrics = append(cfg.Metrics, d.Name)
		}
		if m.Kind == plugin.KindCollector {
			oc := cfg
			oc.Whoopsy = nil // checks only
			objCfg = &oc
			cfg.Metrics, cfg.Rules, cfg.Whoopsy = nil, nil, nil
		}
	}

	var prev State
	var incidentID *uuid.UUID
	var version int64
	var machine []byte
	err = tx.QueryRow(ctx, `SELECT machine, incident_id, version FROM check_state WHERE check_id = $1 FOR UPDATE`, r.CheckID).
		Scan(&machine, &incidentID, &version)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return "", err
	default:
		if err := json.Unmarshal(machine, &prev); err != nil {
			return "", err
		}
	}

	out := Evaluate(cfg, prev, Input{Status: r.Status, Output: r.Output, Metrics: r.Metrics, Time: r.Time})
	if incidentID, err = e.act(ctx, tx, r, deviceID, nil, hostCheck, incidentID, out); err != nil {
		return "", err
	}
	if out.Action == ActionResolve {
		incidentID = nil
	}

	metrics := map[string]*float64{}
	for i, name := range cfg.Metrics {
		if i < len(r.Metrics) && !math.IsNaN(r.Metrics[i]) && !math.IsInf(r.Metrics[i], 0) {
			v := r.Metrics[i]
			metrics[name] = &v
		}
	}
	output := r.Output
	_, err = tx.Exec(ctx, `
		INSERT INTO check_state (check_id, tenant_id, phase, status, since, machine, last_result_at, last_status,
		                         last_output, last_metrics, incident_id, availability_since)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (check_id) DO UPDATE SET
		       phase = excluded.phase, status = excluded.status, since = excluded.since, machine = excluded.machine,
		       availability_since = excluded.availability_since,
		       last_result_at = excluded.last_result_at, last_status = excluded.last_status,
		       last_output = excluded.last_output, last_metrics = excluded.last_metrics,
		       incident_id = excluded.incident_id, version = check_state.version + 1, updated_at = now()`,
		r.CheckID, r.TenantID, out.State.Phase, out.State.Status, out.State.Since, out.State, r.Time,
		r.Status.String(), output, metrics, incidentID, nullTime(out.State.AvailabilitySince))
	if err != nil {
		return "", err
	}
	if cfg.Whoopsy != nil {
		if err := recordBand(ctx, tx, r, cfg, out.State.Whoopsy); err != nil {
			return "", err
		}
	}
	if objCfg != nil && r.Objects != nil {
		var children map[string]uuid.UUID
		if r.Inventory != nil {
			if children, err = inventory.ApplyDiscovered(ctx, tx, r.TenantID, r.CheckID, deviceID, r.Time, *r.Inventory); err != nil {
				return "", err
			}
		}
		devices := map[string]uuid.UUID{}
		for _, o := range r.Objects {
			if d, ok := children[o.Device]; ok {
				devices[o.Key] = d
			}
		}
		rp.ObjectDevices = devices
		if err := e.applyObjects(ctx, tx, r, deviceID, devices, *objCfg, manifest); err != nil {
			return "", err
		}
	}
	return "applied", nil
}

// act carries out the outcome's incident action and returns the open
// incident afterwards, for the check itself or, with obj, for one of its
// objects.
func (e *Engine) act(ctx context.Context, tx pgx.Tx, r runner.Result, device uuid.UUID, obj *plugin.Object,
	hostCheck bool, open *uuid.UUID, out Outcome) (*uuid.UUID, error) {
	availability := hostCheck
	if obj != nil {
		availability = obj.Availability
	}
	output := r.Output
	var objKey, objName *string
	if obj != nil {
		output, objKey, objName = obj.Output, &obj.Key, &obj.Name
	}
	var ruleID, ruleName *string
	if rule := out.Cause.Rule; rule != nil {
		ruleID = &rule.ID
		if rule.Name != "" {
			ruleName = &rule.Name
		}
	}
	switch out.Action {
	case ActionOpen:
		// The state may have lost track of an incident (manual reset): reuse it.
		if open == nil {
			var id uuid.UUID
			err := tx.QueryRow(ctx, `SELECT id FROM incidents WHERE check_id = $1 AND coalesce(object_key, '') = coalesce($2, '')
				AND status <> 'resolved'`, r.CheckID, objKey).Scan(&id)
			if err == nil {
				open = &id
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
		}
		if open != nil {
			_, err := incident.Update(ctx, tx, *open, incident.UpdateInput{Severity: out.Severity, Summary: out.Summary,
				LastOutput: output, RuleID: ruleID, RuleName: ruleName, Flapping: out.State.Flapping})
			e.incidents.WithLabelValues("update").Inc()
			return open, err
		}
		inc, err := incident.Open(ctx, tx, incident.OpenInput{TenantID: r.TenantID, CheckID: r.CheckID, DeviceID: device,
			ObjectKey: objKey, ObjectName: objName, Availability: availability,
			Severity: out.Severity, Summary: out.Summary, LastOutput: output, RuleID: ruleID, RuleName: ruleName,
			Flapping: out.State.Flapping})
		e.incidents.WithLabelValues("open").Inc()
		return &inc.ID, err
	case ActionUpdate:
		if open == nil {
			return nil, nil
		}
		_, err := incident.Update(ctx, tx, *open, incident.UpdateInput{Severity: out.Severity, Summary: out.Summary,
			LastOutput: output, RuleID: ruleID, RuleName: ruleName, Flapping: out.State.Flapping})
		e.incidents.WithLabelValues("update").Inc()
		return open, err
	case ActionResolve:
		if open != nil {
			if _, _, err := incident.Resolve(ctx, tx, *open, "recovery", nil); err != nil {
				return nil, err
			}
			e.incidents.WithLabelValues("resolve").Inc()
		}
		return nil, nil
	}
	return open, nil
}

// sweep marks checks UNKNOWN when no result arrived for three intervals:
// a stuck runner or a dead probe must not leave a stale OK (F05). Checks of
// suspended tenants do not run on purpose; their state stays as it was.
func (e *Engine) sweep(ctx context.Context) {
	t := time.NewTicker(e.o.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		tag, err := e.o.System.Exec(ctx, `
			UPDATE check_state s
			   SET status = 'UNKNOWN', since = now(),
			       machine = jsonb_set(s.machine, '{status}', '"UNKNOWN"'),
			       last_output = 'no result since ' || to_char(s.last_result_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'),
			       version = s.version + 1, updated_at = now()
			  FROM checks c JOIN tenants t ON t.id = c.tenant_id
			 WHERE c.id = s.check_id AND c.enabled AND s.status <> 'UNKNOWN' AND t.status = 'active'
			   AND s.last_result_at < now() - make_interval(secs => 3 * c.interval_seconds)`)
		if err != nil {
			if ctx.Err() == nil {
				e.log.Error("stale sweep", "error", err)
			}
			continue
		}
		if n := tag.RowsAffected(); n > 0 {
			e.log.Warn("checks without recent results marked UNKNOWN", "count", n)
		}
	}
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// recordBand keeps the Whoopsy! band this result was judged against, for
// graphs: the same numbers the engine used, under the settings in force.
func recordBand(ctx context.Context, tx pgx.Tx, r runner.Result, cfg Config, ws *WhoopsyState) error {
	v := metric(cfg.Metrics, r.Metrics, cfg.Whoopsy.Metric)
	if ws == nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	var mean, sd, lower, upper *float64
	outside := false
	if b := ws.Band; b != nil {
		mean, sd = &b.Mean, &b.StdDev
		switch cfg.Whoopsy.Direction {
		case threshold.DirBelow:
			lower = &b.Lower
		case threshold.DirBoth:
			lower, upper = &b.Lower, &b.Upper
		default:
			upper = &b.Upper
		}
		outside = cfg.Whoopsy.Outside(*b, v)
	}
	_, err := tx.Exec(ctx, `INSERT INTO whoopsy_band (tenant_id, check_id, ts, value, mean, stddev, lower_bound, upper_bound,
		outside, hits, alerting) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (check_id, ts) DO NOTHING`,
		r.TenantID, r.CheckID, r.Time, v, mean, sd, lower, upper, outside, ws.Hits, ws.Level != "")
	return err
}
