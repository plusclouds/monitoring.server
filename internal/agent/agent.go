package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

type named struct {
	typ, name string
	c         Collector
}

// Agent runs the collectors and sends their objects.
type Agent struct {
	cfg     Config
	version string
	log     *slog.Logger
	cols    []named
	push    *Pusher

	seq           atomic.Uint64
	collectErrors atomic.Uint64

	// Extra adds objects the caller owns (the wrapped process of `agent
	// run`) to every report.
	Extra func() []Object
}

// New builds the collectors named by cfg; an unknown type or a bad option is
// an error, so a misconfigured image fails at start, not silently.
func New(cfg Config, version string, log *slog.Logger) (*Agent, error) {
	a := &Agent{cfg: cfg, version: version, log: log}
	seen := map[string]bool{}
	for _, cc := range cfg.Collectors {
		f, ok := factories[cc.Type]
		if !ok {
			return nil, fmt.Errorf("unknown collector %q (have %v)", cc.Type, Types())
		}
		opt := NewOptions(cc.Type, cc.Name, cc.Options, nil)
		if seen[opt.Name] {
			return nil, fmt.Errorf("two collectors are named %q; give one a name", opt.Name)
		}
		seen[opt.Name] = true
		c, err := f(opt)
		if err != nil {
			return nil, fmt.Errorf("collector %s: %w", opt.Name, err)
		}
		a.cols = append(a.cols, named{cc.Type, opt.Name, c})
	}
	return a, nil
}

// SetPusher sets where reports go; without one, Run only collects.
func (a *Agent) SetPusher(p *Pusher) { a.push = p }

// Collect runs every collector once, in parallel, each under a timeout of
// half the interval, and returns one report.
func (a *Agent) Collect(ctx context.Context) Report {
	timeout := a.cfg.Interval / 2
	if timeout < 2*time.Second {
		timeout = 2 * time.Second
	}
	results := make([][]Object, len(a.cols))
	var wg sync.WaitGroup
	for i, nc := range a.cols {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			objs, err := safeCollect(cctx, nc.c)
			if err != nil {
				a.collectErrors.Add(1)
				a.log.Warn("collector failed", "collector", nc.name, "error", err)
				objs = append(objs, Object{Key: "collector:" + nc.name, Kind: "agent", Name: nc.name + " collector",
					Status: Unknown, Output: err.Error()})
			}
			results[i] = objs
		}()
	}
	wg.Wait()

	r := Report{V: 1, AgentID: a.cfg.AgentID, Seq: a.seq.Add(1), AgentVersion: a.version,
		Timestamp: float64(time.Now().UnixNano()) / 1e9}
	for _, objs := range results {
		r.Objects = append(r.Objects, objs...)
	}
	if a.Extra != nil {
		r.Objects = append(r.Objects, a.Extra()...)
	}
	a.stamp(&r)
	return r
}

// stamp adds the agent's own counters to the host object.
func (a *Agent) stamp(r *Report) {
	for i := range r.Objects {
		o := &r.Objects[i]
		if o.Key != "host" {
			continue
		}
		if o.Metrics == nil {
			o.Metrics = map[string]float64{}
		}
		o.Metrics["agent_collect_errors"] = float64(a.collectErrors.Load())
		if a.push != nil {
			o.Metrics["agent_push_errors"] = float64(a.push.Errors())
		}
		return
	}
}

func safeCollect(ctx context.Context, c Collector) (objs []Object, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("collector panicked: %v", p)
		}
	}()
	return c.Collect(ctx)
}

// Run collects and sends every interval until ctx ends.
func (a *Agent) Run(ctx context.Context) {
	t := time.NewTicker(a.cfg.Interval)
	defer t.Stop()
	for {
		a.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick collects once and sends the report.
func (a *Agent) Tick(ctx context.Context) {
	if a.push == nil {
		return
	}
	r := a.Collect(ctx)
	if err := a.push.Send(ctx, r); err != nil && ctx.Err() == nil {
		a.log.Warn("report not delivered", "error", err, "seq", r.Seq)
	}
}
