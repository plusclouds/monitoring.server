// Package app wires the roles of `monitor serve` together and runs them
// until the process is asked to stop (ADR-0001).
package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"golang.org/x/sync/errgroup"

	"github.com/plusclouds/monitoring.server/internal/api"
	"github.com/plusclouds/monitoring.server/internal/buildinfo"
	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/logging"
	"github.com/plusclouds/monitoring.server/internal/statusserver"
	"github.com/plusclouds/monitoring.server/internal/store"
)

// ServeOptions is what `monitor serve` resolved from flags and the config.
type ServeOptions struct {
	Config config.Config
	Roles  []string
	Logger *logging.Logger
	// Reload re-reads the config file on SIGHUP. Only the log level is applied
	// at runtime; everything else needs a restart (ADR-0011).
	Reload func() (config.Config, error)
}

// implementedRoles grows as milestones land; the others are accepted in the
// config but not started yet.
var implementedRoles = []string{config.RoleAPI}

// Serve runs until ctx is cancelled.
func Serve(ctx context.Context, o ServeOptions) error {
	log := o.Logger
	started := time.Now().UTC()

	token, err := config.ReadSecret("self_monitoring.status_token",
		o.Config.SelfMonitoring.StatusToken, o.Config.SelfMonitoring.StatusTokenFile)
	if err != nil {
		return err
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewBuildInfoCollector(),
	)
	buildInfo := prometheus.NewGauge(prometheus.GaugeOpts{
		Name:        "monitor_build_info",
		Help:        "Always 1; labels carry the build.",
		ConstLabels: prometheus.Labels{"version": buildinfo.Version, "commit": buildinfo.CommitOrVCS()},
	})
	buildInfo.Set(1)
	reg.MustRegister(buildInfo)

	sm := o.Config.SelfMonitoring
	status, err := statusserver.New(statusserver.Options{
		Listen:          sm.Listen,
		TLS:             sm.TLS,
		Policy:          o.Config.TLS,
		Token:           token,
		AllowedNetworks: sm.AllowedNetworks,
		Gatherer:        reg,
		Logger:          log.Logger,
		Status: func() any {
			return map[string]any{
				"node_id":    o.Config.Node.ID,
				"roles":      o.Roles,
				"version":    buildinfo.Version,
				"commit":     buildinfo.CommitOrVCS(),
				"started_at": started.Format(time.RFC3339),
				"uptime":     time.Since(started).Round(time.Second).String(),
			}
		},
	})
	if err != nil {
		return err
	}

	pools, err := openDatabase(ctx, o)
	if err != nil {
		return err
	}
	defer pools.close()
	status.AddReadiness("database", pools.ping)

	for _, r := range o.Roles {
		if !slices.Contains(implementedRoles, r) {
			log.Warn("role is not implemented yet and will not start", "role", r)
		}
	}

	go reloadOnHUP(ctx, o)

	log.Info("monitor starting", "node_id", o.Config.Node.ID, "roles", o.Roles, "version", buildinfo.Version)
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return status.Run(gctx) })
	if slices.Contains(o.Roles, config.RoleAPI) {
		srv, err := api.New(api.Options{Config: o.Config, DB: pools.app, Logger: log.Logger, Registry: reg})
		if err != nil {
			return err
		}
		g.Go(func() error { return srv.Run(gctx) })
	}
	err = g.Wait()
	log.Info("monitor stopped")
	return err
}

func reloadOnHUP(ctx context.Context, o ServeOptions) {
	if o.Reload == nil {
		return
	}
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			c, err := o.Reload()
			if err != nil {
				o.Logger.Error("config reload failed; keeping the running config", "error", err)
				continue
			}
			if err := o.Logger.SetLevel(c.Logging.Level); err != nil {
				o.Logger.Error("config reload failed", "error", err)
				continue
			}
			o.Logger.Info("config reloaded; log level applied, other changes need a restart", "level", c.Logging.Level)
		}
	}
}

// pools holds the connection pools this process needs: app for the api
// role (subject to RLS), system for every other role (ADR-0008).
type pools struct {
	app, system *pgxpool.Pool
}

func openDatabase(ctx context.Context, o ServeOptions) (*pools, error) {
	db := o.Config.Database
	if db.MigrateOnStart {
		owner, err := config.ReadSecret("database.owner.dsn", db.Owner.DSN, db.Owner.DSNFile)
		if err != nil {
			return nil, err
		}
		res, err := store.Migrate(ctx, owner, "up")
		if err != nil {
			return nil, fmt.Errorf("migrate: %w", err)
		}
		for _, r := range res {
			o.Logger.Info("migration applied", "version", r.Source.Version, "file", r.Source.Path)
		}
	}

	p := &pools{}
	open := func(name string, d config.DSN) (*pgxpool.Pool, error) {
		dsn, err := config.ReadSecret(name+".dsn", d.DSN, d.DSNFile)
		if err != nil {
			return nil, err
		}
		return store.Open(ctx, store.PoolOptions{
			DSN: dsn, MaxConns: d.MaxConns,
			ConnectTimeout: db.ConnectTimeout.D(), StatementTimeout: db.StatementTimeout.D(),
			AppName: "monitor/" + o.Config.Node.ID,
		})
	}
	var err error
	if slices.Contains(o.Roles, config.RoleAPI) {
		if p.app, err = open("database.app", db.App); err != nil {
			return nil, fmt.Errorf("connect as app role: %w", err)
		}
	}
	apiOnly := len(o.Roles) == 1 && o.Roles[0] == config.RoleAPI
	if !apiOnly {
		if p.system, err = open("database.system", db.System); err != nil {
			p.close()
			return nil, fmt.Errorf("connect as system role: %w", err)
		}
	}
	return p, nil
}

func (p *pools) ping(ctx context.Context) error {
	for _, pool := range []*pgxpool.Pool{p.app, p.system} {
		if pool == nil {
			continue
		}
		if err := pool.Ping(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (p *pools) close() {
	for _, pool := range []*pgxpool.Pool{p.app, p.system} {
		if pool != nil {
			pool.Close()
		}
	}
}
