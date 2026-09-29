// Package store opens the database pools and runs migrations.
//
// Request code gets a database handle only through InTenant, which starts a
// transaction and sets app.tenant_id before anything else, so row-level
// security always applies (ADR-0008).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"

	"github.com/plusclouds/monitoring.server/migrations"
)

// PoolOptions configure one pool.
type PoolOptions struct {
	DSN              string
	MaxConns         int
	ConnectTimeout   time.Duration
	StatementTimeout time.Duration
	AppName          string
}

// Open connects a pool and checks it with a ping.
func Open(ctx context.Context, o PoolOptions) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(o.DSN)
	if err != nil {
		return nil, errDSN
	}
	if o.MaxConns > 0 {
		cfg.MaxConns = int32(min(o.MaxConns, 1<<20)) //nolint:gosec // bounded above
	}
	if o.ConnectTimeout > 0 {
		cfg.ConnConfig.ConnectTimeout = o.ConnectTimeout
	}
	if o.StatementTimeout > 0 {
		cfg.ConnConfig.RuntimeParams["statement_timeout"] = strconv.FormatInt(o.StatementTimeout.Milliseconds(), 10)
	}
	if o.AppName != "" {
		cfg.ConnConfig.RuntimeParams["application_name"] = o.AppName
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return pool, nil
}

// errDSN replaces pgx parse errors, which can quote the DSN and its password.
var errDSN = errors.New("invalid database connection string")

// InTenant runs fn in a transaction scoped to one tenant through RLS.
// The transaction commits when fn returns nil.
func InTenant(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
			return err
		}
		return fn(tx)
	})
}

// Migrate applies (up) or rolls back one version (down) of the embedded
// migrations as the schema owner, under a PostgreSQL advisory lock so
// several starting nodes never migrate at once.
func Migrate(ctx context.Context, ownerDSN string, direction string) ([]*goose.MigrationResult, error) {
	p, db, err := provider(ownerDSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	switch direction {
	case "up":
		return p.Up(ctx)
	case "down":
		r, err := p.Down(ctx)
		if r == nil {
			return nil, err
		}
		return []*goose.MigrationResult{r}, err
	default:
		return nil, fmt.Errorf("unknown direction %q", direction)
	}
}

// MigrationStatus lists every migration and whether it is applied.
func MigrationStatus(ctx context.Context, ownerDSN string) ([]*goose.MigrationStatus, error) {
	p, db, err := provider(ownerDSN)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	return p.Status(ctx)
}

func provider(dsn string) (*goose.Provider, *sql.DB, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, nil, errDSN
	}
	db := stdlib.OpenDB(*cfg)
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithSessionLocker(locker))
	if err != nil {
		_ = db.Close()
		return nil, nil, err
	}
	return p, db, nil
}
