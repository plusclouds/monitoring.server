// Package natsingest receives the PlusClouds VM agents' telemetry from the
// platform's NATS (vm.<uuid>.telemetry) for vm.agent checks. The platform
// creates a device and a vm.agent check per VM; a message is routed by the
// VM UUID in its subject to that check, the agent's disks and interfaces
// become the check's objects, and silence is the push checks' last-seen
// rule (the VM is off or its agent stopped).
package natsingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/ingest"
	"github.com/plusclouds/monitoring.server/internal/runner"
	"github.com/plusclouds/monitoring.server/internal/tlsconf"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
	"github.com/plusclouds/monitoring.server/plugins/vmagent"
)

// cacheTTL is how long a VM's check (or its absence) is remembered: a
// check created through the API receives telemetry within it.
const cacheTTL = 30 * time.Second

// Options configure the consumer.
type Options struct {
	Config   config.IngestNATS
	TLS      config.TLSPolicy
	System   *pgxpool.Pool
	Results  chan<- runner.Result
	Logger   *slog.Logger
	Registry prometheus.Registerer
}

// Consumer subscribes and routes telemetry.
type Consumer struct {
	o   Options
	log *slog.Logger

	mu      sync.Mutex
	checks  map[string]cached                         // VM UUID -> check
	prev    map[uuid.UUID]map[string]vmagent.Counters // check -> interface counters
	touched map[uuid.UUID]time.Time

	messages *prometheus.CounterVec
}

type cached struct {
	c      *vmCheck // nil: no check for this VM
	loaded time.Time
}

type vmCheck struct {
	id, tenant, device uuid.UUID
	interval           int
	enabled            bool
}

// New builds the consumer and registers its metrics.
func New(o Options) (*Consumer, error) {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	c := &Consumer{o: o, log: o.Logger, checks: map[string]cached{}, prev: map[uuid.UUID]map[string]vmagent.Counters{},
		touched: map[uuid.UUID]time.Time{},
		messages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "nats_ingest_messages_total", Help: "VM agent messages from NATS by outcome."}, []string{"outcome"})}
	if o.Registry != nil {
		if err := o.Registry.Register(c.messages); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Run connects (reconnecting for ever) and consumes until ctx ends.
func (c *Consumer) Run(ctx context.Context) error {
	opts, err := c.options()
	if err != nil {
		return err
	}
	nc, err := nats.Connect(c.o.Config.URL, opts...)
	if err != nil {
		return fmt.Errorf("ingest.nats: connect: %w", err)
	}
	defer nc.Close()
	sub, err := nc.QueueSubscribe(c.o.Config.Subject, c.o.Config.Queue, func(m *nats.Msg) {
		c.Handle(ctx, m.Subject, m.Data)
	})
	if err != nil {
		return fmt.Errorf("ingest.nats: subscribe %s: %w", c.o.Config.Subject, err)
	}
	if err := sub.SetPendingLimits(50000, 256<<20); err != nil {
		return err
	}
	c.log.Info("nats ingest subscribed", "url", redact(c.o.Config.URL), "subject", c.o.Config.Subject, "queue", c.o.Config.Queue)
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = sub.Drain()
			c.Flush(context.Background())
			return nil
		case <-t.C:
			c.Flush(ctx)
		}
	}
}

func (c *Consumer) options() ([]nats.Option, error) {
	cfg := c.o.Config
	opts := []nats.Option{nats.Name("plusclouds-monitoring-ingest"), nats.MaxReconnects(-1), nats.ReconnectWait(2 * time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				c.log.Warn("nats ingest disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(*nats.Conn) { c.log.Info("nats ingest reconnected") }),
		nats.ErrorHandler(func(_ *nats.Conn, _ *nats.Subscription, err error) {
			c.log.Warn("nats ingest error", "error", err)
		}),
	}
	base, err := tlsconf.Base(c.o.TLS)
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(cfg.URL, "tls://") || strings.HasPrefix(cfg.URL, "wss://") {
		opts = append(opts, nats.Secure(base))
	}
	if cfg.CAFile != "" {
		opts = append(opts, nats.RootCAs(cfg.CAFile))
	}
	if cfg.CertFile != "" {
		opts = append(opts, nats.ClientCert(cfg.CertFile, cfg.KeyFile))
	}
	switch {
	case cfg.CredsFile != "":
		opts = append(opts, nats.UserCredentials(cfg.CredsFile))
	case cfg.TokenFile != "":
		token, err := config.ReadSecret("ingest.nats.token", "", cfg.TokenFile)
		if err != nil {
			return nil, err
		}
		opts = append(opts, nats.Token(token))
	case cfg.User != "":
		pw, err := config.ReadSecret("ingest.nats.password", cfg.Password, cfg.PasswordFile)
		if err != nil {
			return nil, err
		}
		opts = append(opts, nats.UserInfo(cfg.User, pw))
	}
	return opts, nil
}

// Handle routes one message (exported for tests).
func (c *Consumer) Handle(ctx context.Context, subject string, data []byte) {
	m, err := vmagent.Decode(data)
	if err != nil {
		c.messages.WithLabelValues("invalid").Inc()
		return
	}
	// The platform's NATS lets an agent publish only on its own subject, so
	// the subject, not the payload, names the VM.
	vm, ok := subjectVM(subject)
	if !ok || vm != m.VM {
		c.messages.WithLabelValues("subject-mismatch").Inc()
		return
	}
	chk, err := c.check(ctx, vm)
	if err != nil {
		c.messages.WithLabelValues("error").Inc()
		c.log.Error("nats ingest check lookup", "vm", vm, "error", err)
		return
	}
	switch {
	case chk == nil:
		c.messages.WithLabelValues("no-check").Inc()
		return
	case !chk.enabled:
		c.messages.WithLabelValues("check-disabled").Inc()
		return
	}
	c.mu.Lock()
	prev := c.prev[chk.id]
	if prev == nil {
		prev = map[string]vmagent.Counters{}
		c.prev[chk.id] = prev
	}
	c.mu.Unlock()
	now := time.Now().UTC()
	if m.Time.IsZero() {
		m.Time = now
	}
	objs := vmagent.Objects(m, prev)
	res := plugin.Result{Status: plugin.OK, Time: m.Time, Objects: objs, Output: vmagent.Summary(m, objs)}
	res.Time = ingest.Window(&res, now)
	select {
	case c.o.Results <- runner.Result{TenantID: chk.tenant, DeviceID: chk.device, CheckID: chk.id, Plugin: "vm.agent",
		Interval: time.Duration(chk.interval) * time.Second, Scheduled: res.Time, Result: res, Layout: vmagent.Metrics()}:
	case <-ctx.Done():
		return
	case <-time.After(5 * time.Second):
		c.messages.WithLabelValues("engine-busy").Inc()
		return
	}
	c.mu.Lock()
	if t, ok := c.touched[chk.id]; !ok || res.Time.After(t) {
		c.touched[chk.id] = res.Time
	}
	c.mu.Unlock()
	c.messages.WithLabelValues("accepted").Inc()
}

// subjectVM reads <prefix>.<uuid>.telemetry.
func subjectVM(subject string) (string, bool) {
	parts := strings.Split(subject, ".")
	if len(parts) < 3 {
		return "", false
	}
	id := strings.ToLower(parts[len(parts)-2])
	if _, err := uuid.Parse(id); err != nil {
		return "", false
	}
	return id, true
}

func (c *Consumer) check(ctx context.Context, vm string) (*vmCheck, error) {
	c.mu.Lock()
	if e, ok := c.checks[vm]; ok && time.Since(e.loaded) < cacheTTL {
		c.mu.Unlock()
		return e.c, nil
	}
	c.mu.Unlock()
	var k vmCheck
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := c.o.System.QueryRow(qctx, `
		SELECT c.id, c.tenant_id, c.device_id, c.interval_seconds, c.enabled AND t.status = 'active'
		  FROM checks c JOIN tenants t ON t.id = c.tenant_id
		 WHERE c.plugin = 'vm.agent' AND lower(c.config->>'vm_uuid') = $1`, vm).
		Scan(&k.id, &k.tenant, &k.device, &k.interval, &k.enabled)
	var out *vmCheck
	switch {
	case errors.Is(err, pgx.ErrNoRows):
	case err != nil:
		return nil, err
	default:
		out = &k
	}
	c.mu.Lock()
	c.checks[vm] = cached{c: out, loaded: time.Now()}
	if len(c.checks) > 200000 { // a flood of unknown VMs must not grow the cache for ever
		c.checks = map[string]cached{vm: c.checks[vm]}
	}
	c.mu.Unlock()
	return out, nil
}

// Flush writes the newest telemetry time of each check for the last-seen
// rule, in one statement.
func (c *Consumer) Flush(ctx context.Context) {
	c.mu.Lock()
	if len(c.touched) == 0 {
		c.mu.Unlock()
		return
	}
	ids := make([]uuid.UUID, 0, len(c.touched))
	times := make([]time.Time, 0, len(c.touched))
	for id, t := range c.touched {
		ids = append(ids, id)
		times = append(times, t)
	}
	c.touched = map[uuid.UUID]time.Time{}
	c.mu.Unlock()
	if _, err := c.o.System.Exec(ctx, `
		UPDATE push_sources p SET last_push_at = greatest(p.last_push_at, v.t), stale_reported_at = NULL
		  FROM unnest($1::uuid[], $2::timestamptz[]) AS v (id, t) WHERE p.check_id = v.id`, ids, times); err != nil {
		c.log.Error("nats ingest last seen", "error", err)
	}
}

// redact drops credentials from a URL for logs.
func redact(u string) string {
	if i := strings.Index(u, "@"); i >= 0 {
		if j := strings.Index(u, "://"); j >= 0 && j < i {
			return u[:j+3] + "…@" + u[i+1:]
		}
	}
	return u
}
