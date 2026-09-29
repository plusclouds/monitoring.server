// Package app wires the roles of `monitor serve` together and runs them
// until the process is asked to stop (ADR-0001).
package app

import (
	"context"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/plusclouds/monitoring.server/internal/buildinfo"
	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/logging"
	"github.com/plusclouds/monitoring.server/internal/statusserver"
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
var implementedRoles = []string{}

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

	for _, r := range o.Roles {
		if !slices.Contains(implementedRoles, r) {
			log.Warn("role is not implemented yet and will not start", "role", r)
		}
	}

	go reloadOnHUP(ctx, o)

	log.Info("monitor starting", "node_id", o.Config.Node.ID, "roles", o.Roles, "version", buildinfo.Version)
	err = status.Run(ctx)
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
