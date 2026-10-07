// Package execute runs one check against its device: it builds the plugin
// target (address, config, decrypted credentials, network policy) and runs
// the plugin with its timeout. The runner (F04) and device tests (F02) use
// it, so a check behaves the same in both.
package execute

import (
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/plusclouds/monitoring.server/internal/config"
	"github.com/plusclouds/monitoring.server/internal/credential"
	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Querier reads credentials: a pool or a transaction.
type Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// Executor runs checks.
type Executor struct {
	Credentials *credential.Store
	Deny        []netip.Prefix // outbound.deny_networks
	Timeouts    config.RunnerTimeouts
	Log         *slog.Logger
}

// New builds an executor and passes the runner settings to every plugin
// that wants them.
func New(c config.Config, creds *credential.Store, log *slog.Logger) (*Executor, error) {
	deny := make([]netip.Prefix, 0, len(c.Outbound.DenyNetworks))
	for _, s := range c.Outbound.DenyNetworks {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("outbound.deny_networks: %w", err)
		}
		deny = append(deny, p.Masked())
	}
	settings := plugin.Settings{
		ICMPMode: c.Runner.ICMP.Mode, SourceIPv4: c.Runner.SourceAddresses.IPv4,
		SourceIPv6: c.Runner.SourceAddresses.IPv6, DNSResolvers: c.Runner.DNSResolvers,
	}
	for _, p := range plugin.All() {
		if cfg, ok := p.(plugin.Configurable); ok {
			if err := cfg.Configure(settings); err != nil {
				return nil, fmt.Errorf("plugin %s: %w", p.Manifest().Type, err)
			}
		}
	}
	return &Executor{Credentials: creds, Deny: deny, Timeouts: c.Runner.Timeouts, Log: log}, nil
}

// Tenant is what the network policy needs to know about a tenant.
type Tenant struct {
	ID                    uuid.UUID
	IsPlatform            bool
	AllowedTargetNetworks []netip.Prefix
}

// Job is one check run.
type Job struct {
	Tenant          Tenant
	DeviceID        uuid.UUID
	Address         string
	CheckID         uuid.UUID
	Plugin          string
	Config          []byte
	IntervalSeconds int
	TimeoutSeconds  *int
	Credentials     map[string]uuid.UUID // role -> credential
	State           plugin.StateStore
	// Decrypted, when set, replaces decrypting Credentials in Run: callers
	// that must not hold a transaction during the run decrypt first.
	Decrypted map[string]plugin.Credential
}

// Policy is the tenant's outgoing connection policy (design section 8):
// configured deny networks always apply; the tenant's allowed networks, when
// set, are the only reachable ones; otherwise customer tenants cannot reach
// private and local addresses.
func (e *Executor) Policy(t Tenant) *plugin.NetPolicy {
	return &plugin.NetPolicy{Deny: e.Deny, Allow: t.AllowedTargetNetworks, DenyPrivate: !t.IsPlatform}
}

// Timeout is the check's timeout: its own setting, or a fraction of the
// interval capped at runner.timeouts.check_max.
func (e *Executor) Timeout(j Job) time.Duration {
	if j.TimeoutSeconds != nil {
		return time.Duration(*j.TimeoutSeconds) * time.Second
	}
	frac := e.Timeouts.FractionOfInterval
	if frac <= 0 || frac > 1 {
		frac = 0.8
	}
	d := time.Duration(float64(j.IntervalSeconds) * frac * float64(time.Second))
	if m := e.Timeouts.CheckMax.D(); m > 0 && d > m {
		d = m
	}
	return max(d, time.Second)
}

// Run executes the job and always returns a result: a missing plugin,
// credential or decryption failure is UNKNOWN, as is anything the plugin
// does wrong. Credentials are decrypted just before the call.
func (e *Executor) Run(ctx context.Context, q Querier, j Job) plugin.Result {
	start := time.Now()
	p, ok := plugin.Lookup(j.Plugin)
	if !ok {
		return unknown(start, "plugin %q is not built into this engine", j.Plugin)
	}
	creds := j.Decrypted
	if creds == nil {
		var err error
		if creds, err = e.Decrypt(ctx, q, j); err != nil {
			return unknown(start, "%v", err)
		}
	}
	state := j.State
	if state == nil {
		state = &plugin.MemState{}
	}
	runCtx, cancel := context.WithTimeout(ctx, e.Timeout(j))
	defer cancel()
	target := plugin.Target{
		DeviceID:    j.DeviceID.String(),
		Address:     j.Address,
		Config:      j.Config,
		Credentials: creds,
		State:       state,
		Network:     e.Policy(j.Tenant),
		Log:         e.Log.With("check_id", j.CheckID, "plugin", j.Plugin),
	}
	switch p := p.(type) {
	case plugin.Check:
		return plugin.SafeRun(runCtx, p, target)
	case plugin.Collector:
		return plugin.SafeCollect(runCtx, p, target)
	}
	if p.Manifest().Kind == plugin.KindIngester {
		return unknown(start, "%s receives pushed data and cannot be run; push a message to its ingest URL", j.Plugin)
	}
	return unknown(start, "plugin %q cannot be run as a check or collector", j.Plugin)
}

// Decrypt returns the job's credentials by role.
func (e *Executor) Decrypt(ctx context.Context, q Querier, j Job) (map[string]plugin.Credential, error) {
	creds := make(map[string]plugin.Credential, len(j.Credentials))
	for role, id := range j.Credentials {
		c, err := e.Credentials.Decrypt(ctx, q, id)
		if err != nil {
			e.Log.WarnContext(ctx, "credential unavailable", "check_id", j.CheckID, "role", role, "error", err)
			return nil, fmt.Errorf("credential %q is unavailable: %w", role, err)
		}
		creds[role] = c
	}
	return creds, nil
}

func unknown(start time.Time, format string, args ...any) plugin.Result {
	return plugin.Result{Status: plugin.Unknown, Output: fmt.Sprintf(format, args...),
		Time: start.UTC(), Duration: time.Since(start)}
}
