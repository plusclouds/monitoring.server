package metrics

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/config"
)

// Rollup levels.
const (
	LevelRaw = "raw"
	Level5m  = "5m"
	Level1h  = "1h"
)

// rollupLag is how long after a bucket ends it is rolled up: results of the
// bucket's last seconds must have been flushed by then.
const rollupLag = time.Minute

// maxChunk bounds one rollup statement, so catching up after downtime is
// many short transactions rather than one long one.
const maxChunk = 6 * time.Hour

// lockKey makes one maintenance node active (F07).
const lockKey = "monitor:maintenance"

// MaintainerOptions configure a Maintainer.
type MaintainerOptions struct {
	System   *pgxpool.Pool // BYPASSRLS
	Config   config.Metrics
	Logger   *slog.Logger
	Registry prometheus.Registerer
	// Jobs are other singleton jobs run under the same lock, such as the
	// tenant purge.
	Jobs []Job
}

// Job is a periodic maintenance job.
type Job struct {
	Name  string
	Every time.Duration
	Run   func(context.Context) error
}

// Maintainer keeps the metrics store in shape: partitions ahead, rollups
// and retention. One node runs it at a time.
type Maintainer struct {
	o   MaintainerOptions
	log *slog.Logger

	errors  *prometheus.CounterVec
	created prometheus.Counter
	dropped prometheus.Counter
	lag     *prometheus.GaugeVec
}

// NewMaintainer builds a maintainer and registers its metrics.
func NewMaintainer(o MaintainerOptions) (*Maintainer, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	m := &Maintainer{
		o: o, log: o.Logger,
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "metrics_maintenance_errors_total", Help: "Failed maintenance jobs, by job.",
		}, []string{"job"}),
		created: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "metrics_partitions_created_total", Help: "Metric partitions created.",
		}),
		dropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "metrics_partitions_dropped_total", Help: "Metric partitions dropped by retention.",
		}),
		lag: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "metrics_rollup_lag_seconds", Help: "How far the oldest rollup watermark is behind now.",
		}, []string{"level"}),
	}
	if o.Registry != nil {
		for _, c := range []prometheus.Collector{m.errors, m.created, m.dropped, m.lag} {
			if err := o.Registry.Register(c); err != nil {
				return nil, err
			}
		}
	}
	return m, nil
}

// Run waits for the maintenance lock and then runs the jobs on their
// intervals until ctx is done.
func (m *Maintainer) Run(ctx context.Context) error {
	for {
		held, err := m.lead(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			m.log.Error("metrics maintenance stopped; retrying", "error", err)
		} else if !held {
			m.log.Debug("another node runs metrics maintenance")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(30 * time.Second):
		}
	}
}

// lead holds the session lock on a dedicated connection while running the
// jobs. It returns false when another node holds the lock.
func (m *Maintainer) lead(ctx context.Context) (bool, error) {
	conn, err := m.o.System.Acquire(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, lockKey).Scan(&got); err != nil {
		return false, err
	}
	if !got {
		return false, nil
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, lockKey)
	}()
	m.log.Info("metrics maintenance active on this node")

	c := m.o.Config
	type job struct {
		name  string
		every time.Duration
		run   func(context.Context) error
		next  time.Time
	}
	jobs := []*job{
		{name: "partitions", every: time.Hour, run: m.EnsurePartitions},
		{name: "rollup_5m", every: c.Rollups.FiveMinuteEvery.D(), run: func(ctx context.Context) error { _, err := m.Rollup5m(ctx); return err }},
		{name: "rollup_1h", every: c.Rollups.HourlyEvery.D(), run: func(ctx context.Context) error { _, err := m.Rollup1h(ctx); return err }},
		{name: "retention", every: c.RetentionEvery.D(), run: func(ctx context.Context) error { _, err := m.ApplyRetention(ctx); return err }},
	}
	for _, j := range m.o.Jobs {
		jobs = append(jobs, &job{name: j.Name, every: j.Every, run: j.Run})
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		// The lock lives on this connection: losing it means losing the lock.
		if err := conn.Ping(ctx); err != nil {
			return true, err
		}
		now := time.Now()
		for _, j := range jobs {
			if now.Before(j.next) {
				continue
			}
			j.next = now.Add(max(j.every, time.Second))
			if err := j.run(ctx); err != nil && ctx.Err() == nil {
				m.errors.WithLabelValues(j.name).Inc()
				m.log.Error("metrics maintenance job failed", "job", j.name, "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return true, nil
		case <-tick.C:
		}
	}
}

// EnsurePartitions creates partitions from yesterday to partitions_ahead.
func (m *Maintainer) EnsurePartitions(ctx context.Context) error {
	ahead := m.o.Config.PartitionsAhead.D()
	if ahead <= 0 {
		ahead = 72 * time.Hour
	}
	var n int
	if err := m.o.System.QueryRow(ctx, `SELECT metrics_ensure_partitions(now() - interval '1 day', now() + $1::interval)`,
		ahead).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		m.created.Add(float64(n))
		m.log.Info("metric partitions created", "count", n)
	}
	return nil
}

// ApplyRetention drops expired partitions and returns their names.
func (m *Maintainer) ApplyRetention(ctx context.Context) ([]string, error) {
	rows, err := m.o.System.Query(ctx, `SELECT metrics_drop_expired()`)
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if len(names) > 0 {
		m.dropped.Add(float64(len(names)))
		m.log.Info("metric partitions dropped by retention", "partitions", names)
	}
	return names, nil
}

const rollup5mSQL = `
	INSERT INTO metric_rollup_5m (retention_class, series_id, ts, min, max, sum, count)
	SELECT $1::smallint, s.id, date_bin('5 minutes', m.ts, TIMESTAMPTZ '2000-01-01 00:00:00+00'),
	       min(x.v), max(x.v), sum(x.v), count(*)
	  FROM metric_samples m
	  JOIN metric_series s ON s.group_id = m.group_id
	 CROSS JOIN LATERAL (SELECT m.vals[s.slot] AS v) x
	 WHERE m.retention_class = $1 AND m.ts >= $2 AND m.ts < $3 AND x.v <> 'NaN'::float8
	 GROUP BY 2, 3
	ON CONFLICT (retention_class, series_id, ts) DO UPDATE
	   SET min = excluded.min, max = excluded.max, sum = excluded.sum, count = excluded.count`

const rollup1hSQL = `
	INSERT INTO metric_rollup_1h (retention_class, series_id, ts, min, max, sum, count)
	SELECT $1::smallint, series_id, date_trunc('hour', ts, 'UTC'), min(min), max(max), sum(sum), sum(count)
	  FROM metric_rollup_5m
	 WHERE retention_class = $1 AND ts >= $2 AND ts < $3
	 GROUP BY 2, 3
	ON CONFLICT (retention_class, series_id, ts) DO UPDATE
	   SET min = excluded.min, max = excluded.max, sum = excluded.sum, count = excluded.count`

// Rollup5m rolls raw samples up into closed 5-minute buckets and returns
// the number of rollup rows written.
func (m *Maintainer) Rollup5m(ctx context.Context) (int64, error) {
	until := time.Now().Add(-rollupLag).Truncate(5 * time.Minute)
	return m.rollup(ctx, Level5m, 5*time.Minute, rollup5mSQL, `metric_samples`, func(int16) (time.Time, error) {
		return until, nil
	})
}

// Rollup1h rolls 5-minute rollups up into hours that the 5-minute rollups
// have completed.
func (m *Maintainer) Rollup1h(ctx context.Context) (int64, error) {
	return m.rollup(ctx, Level1h, time.Hour, rollup1hSQL, `metric_rollup_5m`, func(class int16) (time.Time, error) {
		wm, err := m.watermark(ctx, Level5m, class)
		return wm.Truncate(time.Hour), err
	})
}

// rollup advances the watermark of each class to closed(class), one chunk
// per transaction. Each run recomputes the bucket before the watermark too,
// so data up to one bucket late still lands in the rollups.
func (m *Maintainer) rollup(ctx context.Context, level string, bucket time.Duration, stmt, source string,
	closed func(int16) (time.Time, error)) (int64, error) {
	classes, err := m.classIDs(ctx)
	if err != nil {
		return 0, err
	}
	var total int64
	oldest := time.Now()
	for _, c := range classes {
		until, err := closed(c)
		if err != nil {
			return total, err
		}
		from, err := m.watermark(ctx, level, c)
		if err != nil {
			return total, err
		}
		if from.IsZero() {
			// First run for the class: start at its oldest data.
			var first *time.Time
			if err := m.o.System.QueryRow(ctx, `SELECT min(ts) FROM `+source+` WHERE retention_class = $1`, c).
				Scan(&first); err != nil {
				return total, err
			}
			if first == nil {
				continue
			}
			from = first.Truncate(bucket)
		} else {
			from = from.Add(-bucket)
		}
		for from.Before(until) {
			to := from.Add(maxChunk)
			if to.After(until) {
				to = until
			}
			var n int64
			err := pgx.BeginFunc(ctx, m.o.System, func(tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SELECT metrics_ensure_partitions($1, $2)`, from, to); err != nil {
					return err
				}
				tag, err := tx.Exec(ctx, stmt, c, from, to)
				if err != nil {
					return err
				}
				n = tag.RowsAffected()
				_, err = tx.Exec(ctx, `
					INSERT INTO metric_rollup_watermarks (level, retention_class, done_until) VALUES ($1, $2, $3)
					ON CONFLICT (level, retention_class) DO UPDATE SET done_until = excluded.done_until, updated_at = now()`,
					level, c, to)
				return err
			})
			if err != nil {
				return total, err
			}
			total += n
			from = to
		}
		if until.Before(oldest) {
			oldest = until
		}
	}
	m.lag.WithLabelValues(level).Set(time.Since(oldest).Seconds())
	return total, nil
}

// watermark returns the end of the completed rollups, or zero.
func (m *Maintainer) watermark(ctx context.Context, level string, class int16) (time.Time, error) {
	var t time.Time
	err := m.o.System.QueryRow(ctx, `SELECT done_until FROM metric_rollup_watermarks WHERE level = $1 AND retention_class = $2`,
		level, class).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	return t, err
}

func (m *Maintainer) classIDs(ctx context.Context) ([]int16, error) {
	rows, err := m.o.System.Query(ctx, `SELECT id FROM retention_classes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int16])
}
