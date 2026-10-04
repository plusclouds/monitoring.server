// Package dbtest starts a real PostgreSQL for integration tests
// (testcontainers-go, ADR-0011), with the three roles from
// deploy/postgres/init-roles.sql and all migrations applied.
//
// Tests using it are skipped when Docker is not available, unless
// MONITOR_REQUIRE_DB=1 (CI sets it) turns that into a failure.
package dbtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/plusclouds/monitoring.server/internal/store"
)

// Image is the PostgreSQL version integration tests run against.
const Image = "postgres:18-alpine"

// DB is a migrated database with a pool per role.
type DB struct {
	OwnerDSN, AppDSN, SystemDSN string
	App, System                 *pgxpool.Pool
}

// Start runs a PostgreSQL container for the calling package's tests. Call it
// from TestMain and pass the result to the tests.
func Start(ctx context.Context) (*DB, func(), error) {
	c, err := postgres.Run(ctx, Image,
		postgres.WithDatabase("postgres"),
		postgres.WithUsername("postgres"),
		postgres.WithPassword("postgres"),
		postgres.WithInitScripts(initScript()),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		return nil, nil, err
	}
	stop := func() { _ = testcontainers.TerminateContainer(c) }

	host, err := c.Host(ctx)
	if err != nil {
		stop()
		return nil, nil, err
	}
	port, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		stop()
		return nil, nil, err
	}
	dsn := func(user, password string) string {
		u := url.URL{Scheme: "postgres", User: url.UserPassword(user, password),
			Host: fmt.Sprintf("%s:%s", host, port.Port()), Path: "/monitor", RawQuery: "sslmode=disable"}
		return u.String()
	}
	db := &DB{
		OwnerDSN:  dsn("monitor_owner", "change-me-owner"),
		AppDSN:    dsn("monitor_app", "change-me-app"),
		SystemDSN: dsn("monitor_system", "change-me-system"),
	}
	if _, err := store.Migrate(ctx, db.OwnerDSN, "up"); err != nil {
		stop()
		return nil, nil, fmt.Errorf("migrate: %w", err)
	}
	if db.App, err = store.Open(ctx, store.PoolOptions{DSN: db.AppDSN}); err != nil {
		stop()
		return nil, nil, err
	}
	if db.System, err = store.Open(ctx, store.PoolOptions{DSN: db.SystemDSN}); err != nil {
		stop()
		return nil, nil, err
	}
	return db, func() { db.App.Close(); db.System.Close(); stop() }, nil
}

// Main is a TestMain helper: it starts the database, runs the tests and
// stops it. Without Docker the tests are skipped (or fail in CI).
func Main(m *testing.M, out **DB) {
	ctx := context.Background()
	db, stop, err := Start(ctx)
	if err != nil {
		if os.Getenv("MONITOR_REQUIRE_DB") == "1" {
			fmt.Fprintln(os.Stderr, "dbtest: cannot start PostgreSQL:", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "dbtest: skipping integration tests, no PostgreSQL container:", err)
		os.Exit(0)
	}
	*out = db
	code := m.Run()
	stop()
	os.Exit(code)
}

// Reset removes all rows, so each test starts from an empty, migrated schema.
func (db *DB) Reset(t *testing.T) {
	t.Helper()
	owner, err := store.Open(context.Background(), store.PoolOptions{DSN: db.OwnerDSN})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	_, err = owner.Exec(context.Background(),
		`TRUNCATE audit_events, api_keys, tenant_members, users, tenants, installation,
		          metric_samples, metric_rollup_5m, metric_rollup_1h, metric_rollup_watermarks, retention_policies CASCADE`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(context.Background(), `DELETE FROM retention_classes WHERE id > 3`); err != nil {
		t.Fatal(err)
	}
}

func initScript() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "deploy", "postgres", "init-roles.sql")
}
