// Package selfmon monitors the monitor (F11): the engine process reports
// its own health every minute as a monitor.self result on a built-in
// "monitor" device of the platform tenant, whose thresholds alert through
// the normal routes; and the maintenance node sends a heartbeat to outside
// receivers while the core loop is healthy, so a dead or stuck engine is
// noticed by someone else.
package selfmon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/plusclouds/monitoring.server/internal/audit"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/internal/threshold"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
	"github.com/plusclouds/monitoring.server/plugins/monitor"
)

// DeviceName is the built-in device of the platform tenant.
const DeviceName = "monitor"

// managedBy marks the device as the engine's own.
const managedBy = "self-monitoring"

// DefaultRules are the built-in alerts (F11). They can be tuned through
// the API like any check's thresholds; a deleted check comes back with
// these.
func DefaultRules() []threshold.Rule {
	c := func(op string, v float64) *threshold.Condition { return &threshold.Condition{Op: op, Value: v} }
	five := threshold.Duration(5 * time.Minute)
	lo, hi := -2.0, 2.0
	return []threshold.Rule{
		{Name: "check lag", Metric: "check_lag_p99_seconds", Warning: c(">", 5), For: five},
		{Name: "notifications stuck", Metric: "outbox_oldest_seconds", Critical: c(">", 300)},
		{Name: "metric rows dropped", Metric: "metrics_dropped_5m", Warning: c(">", 0)},
		{Name: "rollups behind", Metric: "rollup_lag_seconds", Warning: c(">", 1800)},
		{Name: "partitions missing", Metric: "partitions_missing", Critical: c(">", 0)},
		{Name: "webhook endpoint failing", Metric: "webhook_endpoints_failing", Warning: c(">", 0)},
		{Name: "clock offset", Metric: "clock_offset_seconds", Warning: &threshold.Condition{Op: threshold.OpOutside, Value: lo, ValueMax: &hi}},
		{Name: "API authentication failures", Metric: "api_auth_failures_5m", Warning: c(">", 100)},
		{Name: "API rate limiting", Metric: "api_rate_limited_5m", Warning: c(">", 500)},
		{Name: "MQTT authentication failures", Metric: "mqtt_auth_failures_5m", Warning: c(">", 100)},
	}
}

// Ensure creates the monitor device and its monitor.self check in the
// platform tenant when they are missing, and returns their IDs.
func Ensure(ctx context.Context, db *pgxpool.Pool) (tenant, device, check uuid.UUID, err error) {
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT id FROM tenants WHERE is_platform`).Scan(&tenant); err != nil {
			return fmt.Errorf("platform tenant: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('selfmon:' || $1::text, 0))`, tenant); err != nil {
			return err
		}
		actor := audit.Actor{Kind: audit.ActorSystem, Detail: managedBy}
		err := tx.QueryRow(ctx, `SELECT id FROM devices WHERE tenant_id = $1 AND managed_by = $2`, tenant, managedBy).Scan(&device)
		if errors.Is(err, pgx.ErrNoRows) {
			device = uuid.Must(uuid.NewV7())
			name := DeviceName
			var taken bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM devices WHERE tenant_id = $1 AND name = $2)`, tenant, name).Scan(&taken); err != nil {
				return err
			}
			if taken {
				name = DeviceName + " (engine)"
			}
			if _, err := tx.Exec(ctx, `INSERT INTO devices (id, tenant_id, name, type, managed_by, notes)
				VALUES ($1, $2, $3, 'server', $4, 'The monitoring engine itself (F11)')`, device, tenant, name, managedBy); err != nil {
				return err
			}
			if err := audit.Write(ctx, tx, actor.Event(tenant, "device.create", "device", device.String(), nil,
				map[string]any{"name": name, "managed_by": managedBy})); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		err = tx.QueryRow(ctx, `SELECT id FROM checks WHERE device_id = $1 AND plugin = 'monitor.self'`, device).Scan(&check)
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		names := make([]string, len(monitor.Metrics()))
		for i, d := range monitor.Metrics() {
			names[i] = d.Name
		}
		rules, err := threshold.Validate(DefaultRules(), names)
		if err != nil {
			return err
		}
		check = uuid.Must(uuid.NewV7())
		if _, err := tx.Exec(ctx, `
			INSERT INTO checks (id, tenant_id, device_id, name, plugin, config, interval_seconds, enabled, thresholds,
			                    failure_count, recovery_count, is_host_check)
			VALUES ($1, $2, $3, 'engine health', 'monitor.self', '{}', 60, true, $4, 1, 1, true)`,
			check, tenant, device, rules); err != nil {
			return err
		}
		return audit.Write(ctx, tx, actor.Event(tenant, "check.create", "check", check.String(), nil,
			map[string]any{"plugin": "monitor.self", "thresholds": rules}))
	})
	return tenant, device, check, err
}

// Collector reports monitor.self results every minute.
type Collector struct {
	System   *pgxpool.Pool
	Gatherer prometheus.Gatherer // this process's metrics
	Results  chan<- runner.Result
	Logger   *slog.Logger
	Every    time.Duration // default 1 minute

	tenant, device, check uuid.UUID
	hist                  []snapshot // last 6 samples, oldest first
}

// snapshot is the counters and histogram buckets at one time.
type snapshot struct {
	at       time.Time
	counters map[string]float64
	lag      map[float64]float64 // runner_check_lag_seconds cumulative buckets
	lagCount float64
}

// Run reports until ctx ends.
func (c *Collector) Run(ctx context.Context) error {
	if c.Every <= 0 {
		c.Every = time.Minute
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.DiscardHandler)
	}
	t := time.NewTicker(c.Every)
	defer t.Stop()
	for {
		if err := c.Tick(ctx); err != nil && ctx.Err() == nil {
			c.Logger.Error("self-monitoring", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick takes one measurement and sends it to the engine.
func (c *Collector) Tick(ctx context.Context) error {
	if c.check == uuid.Nil {
		var err error
		if c.tenant, c.device, c.check, err = Ensure(ctx, c.System); err != nil {
			return err
		}
	} else {
		var exists bool
		if err := c.System.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM checks WHERE id = $1)`, c.check).Scan(&exists); err != nil {
			return err
		}
		if !exists { // deleted: recreate with the default rules
			c.check = uuid.Nil
			return c.Tick(ctx)
		}
	}
	now := time.Now().UTC()
	v, err := c.measure(ctx, now)
	if err != nil {
		return err
	}
	r := runner.Result{TenantID: c.tenant, DeviceID: c.device, CheckID: c.check, Plugin: "monitor.self", Interval: c.Every,
		Scheduled: now, Layout: monitor.Metrics(),
		Result: plugin.Result{Status: plugin.OK, Time: now, Metrics: v, Output: summary(v)}}
	select {
	case c.Results <- r:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(10 * time.Second):
		return errors.New("the engine did not take the self-monitoring result within 10 s")
	}
}

func summary(v []float64) string {
	f := func(x float64, format string) string {
		if math.IsNaN(x) {
			return "n/a"
		}
		return fmt.Sprintf(format, x)
	}
	return strings.Join([]string{
		"lag p99 " + f(v[monitor.CheckLagP99], "%.2fs"),
		"results/min " + f(v[monitor.ResultsPerMinute], "%.0f"),
		"outbox " + f(v[monitor.OutboxDepth], "%.0f") + " (oldest " + f(v[monitor.OutboxOldest], "%.0fs") + ")",
		"rollup lag " + f(v[monitor.RollupLag], "%.0fs"),
		"clock " + f(v[monitor.ClockOffset], "%+.3fs"),
	}, ", ")
}

// measure collects every value; one it cannot read is NaN.
func (c *Collector) measure(ctx context.Context, now time.Time) ([]float64, error) {
	v := plugin.NaNs(monitor.NumMetrics)
	// Database values: installation-wide.
	var depth int64
	var oldest *float64
	if err := c.System.QueryRow(ctx, `SELECT count(*), extract(epoch FROM now() - min(created_at))::float8
		FROM webhook_deliveries WHERE status = 'pending'`).Scan(&depth, &oldest); err != nil {
		return nil, err
	}
	v[monitor.OutboxDepth] = float64(depth)
	v[monitor.OutboxOldest] = 0
	if oldest != nil {
		v[monitor.OutboxOldest] = *oldest
	}
	var lag *float64
	if err := c.System.QueryRow(ctx, `SELECT extract(epoch FROM now() - min(done_until))::float8
		FROM metric_rollup_watermarks WHERE level = '5m'`).Scan(&lag); err != nil {
		return nil, err
	}
	if lag != nil {
		v[monitor.RollupLag] = *lag
	}
	var missing int64
	if err := c.System.QueryRow(ctx, `SELECT count(*) FROM retention_classes rc
		WHERE to_regclass(format('public.metric_samples_c%s_d%s', rc.id,
		      to_char((now() AT TIME ZONE 'UTC')::date + 1, 'YYYYMMDD'))) IS NULL`).Scan(&missing); err != nil {
		return nil, err
	}
	v[monitor.PartitionsMissing] = float64(missing)
	var failing int64
	if err := c.System.QueryRow(ctx, `
		SELECT count(*) FROM webhook_endpoints e
		 WHERE e.enabled
		   AND EXISTS (SELECT 1 FROM webhook_deliveries d WHERE d.endpoint_id = e.id AND d.status IN ('pending', 'failed')
		               AND d.attempts > 0 AND d.created_at < now() - interval '1 hour'
		               AND d.created_at > now() - interval '1 day')
		   AND NOT EXISTS (SELECT 1 FROM webhook_deliveries d WHERE d.endpoint_id = e.id AND d.status = 'delivered'
		                   AND d.delivered_at > now() - interval '1 hour')`).Scan(&failing); err != nil {
		return nil, err
	}
	v[monitor.WebhooksFailing] = float64(failing)
	before := time.Now()
	var dbNow time.Time
	if err := c.System.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&dbNow); err != nil {
		return nil, err
	}
	mid := before.Add(time.Since(before) / 2)
	v[monitor.ClockOffset] = mid.Sub(dbNow).Seconds()

	// Process values: this node's Prometheus registry, as differences over
	// the last minute or five.
	if c.Gatherer == nil {
		return v, nil
	}
	s, gauges, err := c.snap(now)
	if err != nil {
		return v, nil //nolint:nilerr // without the registry the database values still count
	}
	c.hist = append(c.hist, s)
	if len(c.hist) > 6 {
		c.hist = c.hist[len(c.hist)-6:]
	}
	if old, ok := c.ago(now, 5*time.Minute); ok {
		for slot, name := range map[int]string{monitor.MetricsDropped: "metrics_dropped_total",
			monitor.APIAuthFailures: "api_auth_failures_total", monitor.APIRateLimited: "api_rate_limited_total",
			monitor.MQTTAuthFailures: "mqtt_auth_failures_total"} {
			if cur, ok := s.counters[name]; ok {
				v[slot] = max(cur-old.counters[name], 0)
			}
		}
		if s.lag != nil {
			v[monitor.CheckLagP99] = quantile(0.99, s.lag, old.lag, s.lagCount-old.lagCount)
		}
	}
	if old, ok := c.ago(now, time.Minute); ok {
		if cur, ok := s.counters["engine_results_total"]; ok {
			v[monitor.ResultsPerMinute] = max(cur-old.counters["engine_results_total"], 0) * time.Minute.Seconds() /
				max(now.Sub(old.at).Seconds(), 1)
		}
	}
	if g, ok := gauges["mqtt_connections"]; ok {
		v[monitor.MQTTConnections] = g
	}
	return v, nil
}

// ago returns the oldest snapshot at most d old (but older than now), so a
// young process uses what it has.
func (c *Collector) ago(now time.Time, d time.Duration) (snapshot, bool) {
	for _, s := range c.hist {
		if now.Sub(s.at) <= d+5*time.Second && s.at.Before(now) {
			return s, true
		}
	}
	return snapshot{}, false
}

func (c *Collector) snap(now time.Time) (snapshot, map[string]float64, error) {
	fams, err := c.Gatherer.Gather()
	if err != nil {
		return snapshot{}, nil, err
	}
	s := snapshot{at: now, counters: map[string]float64{}}
	gauges := map[string]float64{}
	for _, f := range fams {
		name := f.GetName()
		for _, m := range f.GetMetric() {
			switch f.GetType() {
			case dto.MetricType_COUNTER:
				s.counters[name] += m.GetCounter().GetValue()
			case dto.MetricType_GAUGE:
				gauges[name] += m.GetGauge().GetValue()
			case dto.MetricType_HISTOGRAM:
				if name != "runner_check_lag_seconds" {
					continue
				}
				if s.lag == nil {
					s.lag = map[float64]float64{}
				}
				for _, b := range m.GetHistogram().GetBucket() {
					s.lag[b.GetUpperBound()] += float64(b.GetCumulativeCount())
				}
				s.lagCount += float64(m.GetHistogram().GetSampleCount())
			}
		}
	}
	return s, gauges, nil
}

// quantile estimates a quantile from the difference of two cumulative
// histograms, interpolating inside the bucket; observations above the last
// bound count as the last bound. NaN without observations.
func quantile(q float64, cur, old map[float64]float64, total float64) float64 {
	if total <= 0 {
		return math.NaN()
	}
	bounds := make([]float64, 0, len(cur))
	for b := range cur {
		bounds = append(bounds, b)
	}
	slices.Sort(bounds)
	rank := q * total
	prevBound, prevCount := 0.0, 0.0
	for _, b := range bounds {
		n := cur[b] - old[b]
		if n >= rank {
			if n == prevCount {
				return b
			}
			return prevBound + (b-prevBound)*(rank-prevCount)/(n-prevCount)
		}
		prevBound, prevCount = b, n
	}
	if len(bounds) == 0 {
		return math.NaN()
	}
	return bounds[len(bounds)-1]
}
