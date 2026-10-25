// Command agent is the monitoring agent (F15): one static binary that runs
// inside a Docker image, wrapping the image's own process, or beside it as a
// sidecar, and reports host, container, probe and service metrics to the
// monitoring server over HTTP.
//
//	agent run -- nginx -g 'daemon off;'   wrap the image's process (PID 1)
//	agent daemon                          sidecar or standalone
//	agent check                           collect once and print the report
//	agent version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/plusclouds/monitoring.server/internal/agent"
	_ "github.com/plusclouds/monitoring.server/internal/agent/collectors/all" // collectors register themselves
	"github.com/plusclouds/monitoring.server/internal/buildinfo"
)

const defaultConfigPath = "/etc/monitor-agent/agent.yaml"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agent:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "daemon"
	if len(args) > 0 && len(args[0]) > 0 && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "daemon":
		return daemon(args)
	case "run":
		return wrap(args)
	case "check":
		return check(args)
	case "version":
		fmt.Printf("agent %s (%s)\n", buildinfo.Version, buildinfo.CommitOrVCS())
		return nil
	}
	return fmt.Errorf("unknown command %q; use daemon, run, check or version", cmd)
}

// loadFlags parses --config and loads the config. A missing default file is
// not an error: the environment alone is a complete configuration.
func loadFlags(name string, args []string) (agent.Config, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	def := defaultConfigPath
	if v := os.Getenv("AGENT_CONFIG"); v != "" {
		def = v
	}
	path := fs.String("config", def, "config file")
	if err := fs.Parse(args); err != nil {
		return agent.Config{}, nil, err
	}
	p := *path
	if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) && p == defaultConfigPath { //nolint:gosec // the operator chooses the config file
		p = ""
	}
	cfg, err := agent.LoadConfig(p, nil)
	return cfg, fs.Args(), err
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{}))
}

// build makes the agent and its pusher from a loaded config.
func build(cfg agent.Config, log *slog.Logger) (*agent.Agent, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	a, err := agent.New(cfg, buildinfo.Version, log)
	if err != nil {
		return nil, err
	}
	p, err := agent.NewPusher(cfg)
	if err != nil {
		return nil, err
	}
	a.SetPusher(p)
	return a, nil
}

func daemon(args []string) error {
	cfg, _, err := loadFlags("daemon", args)
	if err != nil {
		return err
	}
	log := newLogger()
	a, err := build(cfg, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Info("agent started", "agent_id", cfg.AgentID, "interval", cfg.Interval.String(), "version", buildinfo.Version)
	a.Run(ctx)
	return nil
}

// check collects once and prints the report; with --send it also delivers it.
func check(args []string) error {
	cfg, _, err := loadFlags("check", args)
	if err != nil {
		return err
	}
	log := newLogger()
	a, err := agent.New(cfg, buildinfo.Version, log)
	if err != nil {
		return err
	}
	// Rates need two samples.
	a.Collect(context.Background())
	time.Sleep(time.Second)
	r := a.Collect(context.Background())
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// wrap runs the image's own process under the agent. Whatever is wrong with
// the agent's setup, the process starts: monitoring never takes the service
// down.
func wrap(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	def := defaultConfigPath
	if v := os.Getenv("AGENT_CONFIG"); v != "" {
		def = v
	}
	path := fs.String("config", def, "config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cmdline := fs.Args()
	log := newLogger()

	p, err := agent.StartProcess(cmdline)
	if err != nil {
		return fmt.Errorf("start %v: %w", cmdline, err)
	}

	var a *agent.Agent
	cfgPath := *path
	if _, err := os.Stat(cfgPath); errors.Is(err, os.ErrNotExist) && cfgPath == defaultConfigPath { //nolint:gosec // the operator chooses the config file
		cfgPath = ""
	}
	if cfg, err := agent.LoadConfig(cfgPath, nil); err != nil {
		log.Error("monitoring disabled: config", "error", err)
	} else if a, err = build(cfg, log); err != nil {
		log.Error("monitoring disabled", "error", err)
		a = nil
	}

	if a == nil {
		<-p.Done()
		os.Exit(p.ExitCode())
	}
	a.Extra = func() []agent.Object { return []agent.Object{p.Object()} }
	ctx, cancel := context.WithCancel(context.Background())
	loop := make(chan struct{})
	go func() { a.Run(ctx); close(loop) }()

	<-p.Done()
	cancel()
	<-loop
	// One last report says why the process ended; the silence after it is
	// the server's last-seen rule.
	fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
	a.Tick(fctx)
	fcancel()
	os.Exit(p.ExitCode())
	return nil
}
