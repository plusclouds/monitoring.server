// Package metrics is the metrics store (F07, ADR-0003): the batched write
// path from check results to grouped raw samples, rollups and retention
// for the maintenance role, and queries for the API.
package metrics

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Samples outside this window around the current time are rejected: a
// wrong clock must not create partitions years away.
const (
	maxAge    = 35 * 24 * time.Hour
	maxFuture = 10 * time.Minute
)

// WriterOptions configure a Writer.
type WriterOptions struct {
	System   *pgxpool.Pool // BYPASSRLS
	Config   config.MetricsWrite
	Logger   *slog.Logger
	Registry prometheus.Registerer
}

// Writer buffers the metrics of check results and writes them with COPY in
// batches. It never blocks the caller: when its queue is full, results are
// dropped and counted (F07).
type Writer struct {
	o   WriterOptions
	in  chan runner.Result
	log *slog.Logger

	// Used only by the Run goroutine.
	pending []entry
	groups  map[groupKey]int64
	classes map[string]int16
	tenants map[uuid.UUID]tenantClasses

	written   prometheus.Counter
	dropped   *prometheus.CounterVec
	flushTime prometheus.Histogram
	buffered  prometheus.Gauge
}

type entry struct {
	r     runner.Result
	added time.Time
}

type groupKey struct {
	source uuid.UUID
	object string
	class  int16
	layout int32
}

type tenantClasses struct {
	allowed []string
	at      time.Time
}

// tenantCacheTTL is how long a tenant's metric_classes limit is cached.
const tenantCacheTTL = time.Minute

// NewWriter builds a writer and registers its metrics.
func NewWriter(o WriterOptions) (*Writer, error) {
	if o.Config.BatchRows <= 0 {
		o.Config.BatchRows = 5000
	}
	if o.Config.FlushInterval <= 0 {
		o.Config.FlushInterval = config.Duration(time.Second)
	}
	if o.Config.MaxBuffered <= 0 {
		o.Config.MaxBuffered = config.Duration(time.Minute)
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	w := &Writer{
		o: o, log: o.Logger,
		in:      make(chan runner.Result, 2*o.Config.BatchRows),
		groups:  map[groupKey]int64{},
		classes: map[string]int16{},
		tenants: map[uuid.UUID]tenantClasses{},
		written: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "metrics_rows_written_total", Help: "Raw sample rows written.",
		}),
		dropped: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "metrics_dropped_total", Help: "Results whose metrics were not stored, by reason.",
		}, []string{"reason"}),
		flushTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "metrics_flush_seconds", Help: "Time to write one batch of samples.",
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}),
		buffered: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "metrics_buffered_results", Help: "Results waiting to be written.",
		}),
	}
	if o.Registry != nil {
		for _, c := range []prometheus.Collector{w.written, w.dropped, w.flushTime, w.buffered} {
			if err := o.Registry.Register(c); err != nil {
				return nil, err
			}
		}
	}
	return w, nil
}

// Add queues the metrics of a result. It does not block.
func (w *Writer) Add(r runner.Result) {
	if !slices.ContainsFunc(r.Metrics, usable) && !slices.ContainsFunc(r.Objects, func(o plugin.Object) bool {
		return slices.ContainsFunc(o.Metrics, usable)
	}) {
		return
	}
	r.Metrics = slices.Clone(r.Metrics)
	r.Objects = slices.Clone(r.Objects) // the engine keeps using the result
	select {
	case w.in <- r:
	default:
		w.dropped.WithLabelValues("queue_full").Inc()
	}
}

func usable(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Run writes queued metrics until ctx is done, then flushes what is left.
func (w *Writer) Run(ctx context.Context) error {
	t := time.NewTicker(w.o.Config.FlushInterval.D())
	defer t.Stop()
	var retryAt time.Time
	backoff := time.Second
	flush := func(ctx context.Context) {
		if len(w.pending) == 0 || time.Now().Before(retryAt) {
			return
		}
		if err := w.Flush(ctx); err != nil {
			if ctx.Err() == nil {
				w.log.Error("metrics flush failed; keeping the batch", "error", err, "retry_in", backoff)
			}
			retryAt = time.Now().Add(backoff)
			backoff = min(2*backoff, 30*time.Second)
			w.expire()
			return
		}
		retryAt, backoff = time.Time{}, time.Second
	}
	for {
		select {
		case <-ctx.Done():
			w.drain()
			final, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			retryAt = time.Time{}
			flush(final)
			cancel()
			return nil
		case r := <-w.in:
			w.pending = append(w.pending, entry{r: r, added: time.Now()})
			if len(w.pending) >= w.o.Config.BatchRows {
				flush(ctx)
			}
		case <-t.C:
			flush(ctx)
		}
		w.buffered.Set(float64(len(w.pending)))
	}
}

func (w *Writer) drain() {
	for {
		select {
		case r := <-w.in:
			w.pending = append(w.pending, entry{r: r, added: time.Now()})
		default:
			return
		}
	}
}

// expire drops the oldest results beyond the buffer limit, so a database
// outage costs memory for at most max_buffered of data.
func (w *Writer) expire() {
	cut := time.Now().Add(-w.o.Config.MaxBuffered.D())
	n := 0
	for n < len(w.pending) && w.pending[n].added.Before(cut) {
		n++
	}
	if n > 0 {
		w.dropped.WithLabelValues("buffer_expired").Add(float64(n))
		w.pending = slices.Delete(w.pending, 0, n)
	}
}

type sample struct {
	class int16
	group int64
	ts    time.Time
	vals  []float64
}

// Flush writes every pending result. On error the results stay pending.
// Run calls it; tests call it directly.
func (w *Writer) Flush(ctx context.Context) error {
	start := time.Now()
	w.drain()
	rows := make([][]any, 0, len(w.pending))
	var lo, hi time.Time
	for _, e := range w.pending {
		ss, err := w.samples(ctx, e.r)
		if err != nil {
			return err
		}
		for _, s := range ss {
			rows = append(rows, []any{s.class, s.group, s.ts, s.vals})
			if lo.IsZero() || s.ts.Before(lo) {
				lo = s.ts
			}
			if s.ts.After(hi) {
				hi = s.ts
			}
		}
	}
	if len(rows) > 0 {
		err := w.copy(ctx, rows)
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23514" { // no partition for a row
			if _, err := w.o.System.Exec(ctx, `SELECT metrics_ensure_partitions($1, $2)`, lo, hi); err != nil {
				return fmt.Errorf("create partitions: %w", err)
			}
			err = w.copy(ctx, rows)
		}
		if err != nil {
			return err
		}
	}
	w.written.Add(float64(len(rows)))
	w.flushTime.Observe(time.Since(start).Seconds())
	w.pending = w.pending[:0]
	return nil
}

func (w *Writer) copy(ctx context.Context, rows [][]any) error {
	_, err := w.o.System.CopyFrom(ctx, pgx.Identifier{"metric_samples"},
		[]string{"retention_class", "group_id", "ts", "vals"}, pgx.CopyFromRows(rows))
	return err
}

// samples splits a result into one sample per retention class of its
// plugin's layout.
func (w *Writer) samples(ctx context.Context, r runner.Result) ([]sample, error) {
	p, ok := plugin.Lookup(r.Plugin)
	if !ok {
		w.dropped.WithLabelValues("unknown_plugin").Inc()
		return nil, nil
	}
	now := time.Now()
	if r.Time.Before(now.Add(-maxAge)) || r.Time.After(now.Add(maxFuture)) {
		w.dropped.WithLabelValues("out_of_range").Inc()
		return nil, nil
	}
	defs := p.Manifest().Metrics
	allowed, err := w.tenantClasses(ctx, r.TenantID)
	if err != nil {
		return nil, err
	}
	// A check's metrics are one set; a collector's are one set per object.
	if p.Manifest().Kind != plugin.KindCollector {
		return w.split(ctx, r, "", r.Metrics, defs, allowed)
	}
	var out []sample
	for _, o := range r.Objects {
		ss, err := w.split(ctx, r, o.Key, o.Metrics, defs, allowed)
		if err != nil {
			return nil, err
		}
		out = append(out, ss...)
	}
	return out, nil
}

// split turns one set of metric values into one sample per retention class.
func (w *Writer) split(ctx context.Context, r runner.Result, object string, values []float64,
	defs []plugin.MetricDef, allowed []string) ([]sample, error) {
	byClass := map[string][]int{}
	var order []string
	for i := range defs {
		c := chooseClass(defs[i].RetentionClass, allowed)
		if _, seen := byClass[c]; !seen {
			order = append(order, c)
		}
		byClass[c] = append(byClass[c], i)
	}
	var out []sample
	for _, name := range order {
		slots := byClass[name]
		vals := make([]float64, len(slots))
		have := false
		for j, i := range slots {
			vals[j] = math.NaN()
			if i < len(values) && usable(values[i]) {
				vals[j], have = values[i], true
			}
		}
		if !have {
			continue
		}
		class, err := w.classID(ctx, name)
		if err != nil {
			return nil, err
		}
		group, err := w.group(ctx, r, object, class, name, defs, slots)
		if err != nil {
			return nil, err
		}
		out = append(out, sample{class: class, group: group, ts: r.Time, vals: vals})
	}
	return out, nil
}

// chooseClass is the class a metric is stored in: the plugin's class when
// the tenant may use it, else standard, else the tenant's first class.
func chooseClass(want string, allowed []string) string {
	switch {
	case want != "" && slices.Contains(allowed, want):
		return want
	case len(allowed) == 0 || slices.Contains(allowed, plugin.RetentionStandard):
		return plugin.RetentionStandard
	}
	return allowed[0]
}

func (w *Writer) tenantClasses(ctx context.Context, tenant uuid.UUID) ([]string, error) {
	if c, ok := w.tenants[tenant]; ok && time.Since(c.at) < tenantCacheTTL {
		return c.allowed, nil
	}
	var allowed []string
	err := w.o.System.QueryRow(ctx, `SELECT metric_classes FROM tenants WHERE id = $1`, tenant).Scan(&allowed)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	w.tenants[tenant] = tenantClasses{allowed: allowed, at: time.Now()}
	return allowed, nil
}

// classID returns the ID of a retention class, falling back to standard for
// a name the database does not know.
func (w *Writer) classID(ctx context.Context, name string) (int16, error) {
	if id, ok := w.classes[name]; ok {
		return id, nil
	}
	rows, err := w.o.System.Query(ctx, `SELECT name, id FROM retention_classes`)
	if err != nil {
		return 0, err
	}
	var n string
	var id int16
	if _, err := pgx.ForEachRow(rows, []any{&n, &id}, func() error { w.classes[n] = id; return nil }); err != nil {
		return 0, err
	}
	if id, ok := w.classes[name]; ok {
		return id, nil
	}
	if id, ok := w.classes[plugin.RetentionStandard]; ok {
		w.classes[name] = id
		return id, nil
	}
	return 0, fmt.Errorf("retention class %q does not exist", name)
}

// layoutVersion identifies the slots of a group: a plugin that adds,
// removes or renames a metric gets new groups.
func layoutVersion(class string, defs []plugin.MetricDef, slots []int) int32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(class))
	for _, i := range slots {
		d := defs[i]
		_, _ = fmt.Fprintf(h, "\x00%s\x00%s\x00%s", d.Name, d.Unit, d.Kind)
	}
	return int32(h.Sum32()) //nolint:gosec // a hash; wrapping is intended
}

// group returns the group ID of a result's metrics in one class, creating
// the group and its series on first use.
func (w *Writer) group(ctx context.Context, r runner.Result, object string, class int16, className string,
	defs []plugin.MetricDef, slots []int) (int64, error) {
	k := groupKey{source: r.CheckID, object: object, class: class, layout: layoutVersion(className, defs, slots)}
	if id, ok := w.groups[k]; ok {
		return id, nil
	}
	// A discovered device's object (a VM) is stored under that device, so
	// its metrics are found by the VM's device_id.
	device := r.DeviceID
	if d, ok := r.ObjectDevices[object]; ok {
		device = d
	}
	var id int64
	err := pgx.BeginFunc(ctx, w.o.System, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO metric_groups (tenant_id, retention_class, layout_version, device_id, source_id, plugin, object_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (source_id, object_key, retention_class, layout_version) DO NOTHING RETURNING id`,
			r.TenantID, class, k.layout, device, r.CheckID, r.Plugin, k.object).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return tx.QueryRow(ctx, `SELECT id FROM metric_groups
				WHERE source_id = $1 AND object_key = $2 AND retention_class = $3 AND layout_version = $4`,
				r.CheckID, k.object, class, k.layout).Scan(&id)
		}
		if err != nil {
			return err
		}
		for j, i := range slots {
			d := defs[i]
			kind := d.Kind
			if kind != "rate" {
				kind = "gauge"
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO metric_series (tenant_id, group_id, slot, name, unit, kind, labels)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`,
				r.TenantID, id, j+1, d.Name, d.Unit, kind, map[string]string{"plugin": r.Plugin}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	w.groups[k] = id
	return id, nil
}

// Sink is what the engine hands results to; Writer implements it.
type Sink interface {
	Add(runner.Result)
}

var _ Sink = (*Writer)(nil)
