package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/agent"
)

type postgres struct {
	name, dsn string
	timeout   time.Duration
	xacts     rate
	now       func() time.Time
}

func newPostgres(o agent.Options) (agent.Collector, error) {
	to, err := o.Duration("timeout", 5*time.Second)
	if err != nil {
		return nil, err
	}
	dsn := o.String("dsn", "")
	if dsn == "" {
		dsn = "host=127.0.0.1 port=5432 user=postgres dbname=postgres sslmode=disable"
		if v := o.String("password", ""); v != "" {
			dsn += " password=" + quote(v)
		}
		if v := o.String("user", ""); v != "" {
			dsn = strings.Replace(dsn, "user=postgres", "user="+quote(v), 1)
		}
		if v := o.String("addr", ""); v != "" {
			h, p, _ := strings.Cut(v, ":")
			dsn = strings.Replace(dsn, "host=127.0.0.1 port=5432", "host="+quote(h)+" port="+quote(orDefault(p, "5432")), 1)
		}
	}
	if _, err := pgx.ParseConfig(dsn); err != nil {
		return nil, errors.New("dsn is not a valid PostgreSQL connection string")
	}
	return &postgres{name: o.Name, dsn: dsn, timeout: to, now: time.Now}, nil
}

func quote(s string) string { return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s) + "'" }

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// Collect connects as a monitoring role and reads the statistics views. It
// needs only `pg_monitor` (GRANT pg_monitor TO monitor), never superuser.
func (p *postgres) Collect(ctx context.Context) ([]agent.Object, error) {
	obj := object("postgres", p.name, nil)
	cctx, cancel := context.WithTimeout(ctx, p.timeout*3)
	defer cancel()
	start := time.Now()
	conn, err := pgx.Connect(cctx, p.dsn)
	if err != nil {
		return down(obj, scrub(err, p.dsn)), nil
	}
	defer func() { _ = conn.Close(context.Background()) }()
	m := obj.Metrics
	m["up"], m["response_ms"] = 1, ms(time.Since(start))

	var conns, maxConns, waiting, size float64
	var longest, uptime, xidAge float64
	var lag *float64
	var xacts, deadlocks, hit, read float64
	var ver string
	err = conn.QueryRow(cctx, `SELECT
		(SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend'),
		current_setting('max_connections')::float8,
		(SELECT count(*) FROM pg_locks WHERE NOT granted),
		(SELECT COALESCE(sum(pg_database_size(datname)), 0) FROM pg_database),
		(SELECT COALESCE(max(EXTRACT(EPOCH FROM now() - query_start)), 0) FROM pg_stat_activity
		   WHERE state = 'active' AND pid <> pg_backend_pid() AND backend_type = 'client backend'),
		EXTRACT(EPOCH FROM now() - pg_postmaster_start_time()),
		(SELECT COALESCE(max(age(datfrozenxid)), 0) FROM pg_database),
		CASE WHEN pg_is_in_recovery() THEN EXTRACT(EPOCH FROM now() - pg_last_xact_replay_timestamp()) END,
		(SELECT COALESCE(sum(xact_commit + xact_rollback), 0) FROM pg_stat_database),
		(SELECT COALESCE(sum(deadlocks), 0) FROM pg_stat_database),
		(SELECT COALESCE(sum(blks_hit), 0) FROM pg_stat_database),
		(SELECT COALESCE(sum(blks_read), 0) FROM pg_stat_database),
		current_setting('server_version')`).
		Scan(&conns, &maxConns, &waiting, &size, &longest, &uptime, &xidAge, &lag, &xacts, &deadlocks, &hit, &read, &ver)
	if err != nil {
		// Connected but cannot read the views: the monitoring role lacks rights.
		obj.Status, obj.Output = agent.Unknown, "statistics query failed: "+scrub(err, p.dsn).Error()
		return []agent.Object{obj}, nil
	}
	m["connections"], m["connections_max"], m["locks_waiting"] = conns, maxConns, waiting
	if maxConns > 0 {
		m["connections_usage_pct"] = conns / maxConns * 100
	}
	m["data_size_bytes"], m["pg_longest_query_seconds"], m["uptime_seconds"] = size, longest, uptime
	m["pg_xid_age"], m["pg_deadlocks"] = xidAge, deadlocks
	if lag != nil {
		m["replication_lag_seconds"] = max(*lag, 0)
		obj.Labels = withKind("postgres", map[string]string{"role": "replica"})
	}
	if hit+read > 0 {
		m["cache_hit_pct"] = hit / (hit + read) * 100
	}
	if r, ok := p.xacts.next(p.now(), xacts); ok {
		m["ops_per_sec"] = r
	}
	obj.Labels["version"] = ver
	return []agent.Object{obj}, nil
}

// scrub keeps the password out of an error that may echo the DSN.
func scrub(err error, dsn string) error {
	s := err.Error()
	if cfg, e := pgx.ParseConfig(dsn); e == nil && cfg.Password != "" {
		s = strings.ReplaceAll(s, cfg.Password, "[redacted]")
	}
	return fmt.Errorf("%s", s)
}
