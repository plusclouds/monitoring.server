// Package icmp is the ping check (F10): round-trip time and packet loss,
// IPv4 and IPv6. Usually a device's host check.
package icmp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"strings"
	"time"

	probing "github.com/prometheus-community/pro-bing"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&Check{mode: "auto"}) }

// Config of an icmp check.
type Config struct {
	Count      int `json:"count,omitempty" jsonschema:"minimum=1,maximum=20,default=3,description=Echo requests per run"`
	IntervalMS int `json:"interval_ms,omitempty" jsonschema:"minimum=10,maximum=1000,default=200,description=Milliseconds between requests"`
	Size       int `json:"size,omitempty" jsonschema:"minimum=24,maximum=1472,default=56,description=Payload size in bytes"`
}

var defaults = Config{Count: 3, IntervalMS: 200, Size: 56}

// Check implements plugin.Check.
type Check struct {
	mode         string // "auto", "privileged", "unprivileged"
	srcV4, srcV6 string
}

func (c *Check) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Type:         "icmp",
		Kind:         plugin.KindCheck,
		Description:  "Ping: round-trip time and packet loss. CRITICAL at 100 % loss.",
		ConfigSchema: plugin.SchemaFor[Config](),
		Metrics: []plugin.MetricDef{
			{Name: "rtt_avg_ms", Unit: "ms", Description: "Average round-trip time", Kind: "gauge", RetentionClass: plugin.RetentionHighFrequency},
			{Name: "rtt_max_ms", Unit: "ms", Description: "Maximum round-trip time", Kind: "gauge", RetentionClass: plugin.RetentionHighFrequency},
			{Name: "packet_loss_percent", Unit: "percent", Description: "Lost echo requests", Kind: "gauge", RetentionClass: plugin.RetentionHighFrequency},
		},
		DefaultInterval: time.Minute,
		MinInterval:     5 * time.Second,
		NeedsRawSocket:  true,
		BillingClass:    plugin.BillingBasic,
		WhoopsyMetric:   "rtt_avg_ms",
	}
}

// Configure applies the runner's ICMP mode and source addresses.
func (c *Check) Configure(s plugin.Settings) error {
	switch s.ICMPMode {
	case "", "auto", "privileged", "unprivileged":
	default:
		return fmt.Errorf("unknown icmp mode %q", s.ICMPMode)
	}
	if s.ICMPMode != "" {
		c.mode = s.ICMPMode
	}
	c.srcV4, c.srcV6 = s.SourceIPv4, s.SourceIPv6
	return nil
}

func (c *Check) Validate(raw json.RawMessage) error {
	_, err := plugin.DecodeConfig(raw, defaults)
	return err
}

func (c *Check) Run(ctx context.Context, t plugin.Target) (plugin.Result, error) {
	cfg, err := plugin.DecodeConfig(t.Config, defaults)
	if err != nil {
		return plugin.Result{}, err
	}
	probe, err := probing.NewPinger(t.Address)
	if err != nil {
		return plugin.Result{Status: plugin.Critical, Output: fmt.Sprintf("cannot resolve %s: %v", t.Address, err)}, nil
	}
	ip, ok := netip.AddrFromSlice(probe.IPAddr().IP)
	if !ok {
		return plugin.Result{}, fmt.Errorf("resolved address of %s is invalid", t.Address)
	}
	ip = ip.Unmap()
	if err := t.Network.Check(ip); err != nil {
		return plugin.Result{Status: plugin.Unknown, Output: err.Error()}, nil
	}

	// Each attempt needs a fresh pinger; the resolved address is reused so
	// both attempts ping the same host.
	newPinger := func(privileged bool) *probing.Pinger {
		p := probing.New("")
		p.SetIPAddr(probe.IPAddr())
		p.SetPrivileged(privileged)
		p.Count = cfg.Count
		p.Interval = time.Duration(cfg.IntervalMS) * time.Millisecond
		p.Size = cfg.Size
		if ip.Is4() && c.srcV4 != "" {
			p.Source = c.srcV4
		} else if ip.Is6() && c.srcV6 != "" {
			p.Source = c.srcV6
		}
		// Leave room to report inside the check's own timeout.
		if dl, ok := ctx.Deadline(); ok {
			p.Timeout = time.Until(dl) - 50*time.Millisecond
		}
		return p
	}

	var p *probing.Pinger
	switch c.mode {
	case "privileged":
		p = newPinger(true)
		err = p.RunWithContext(ctx)
	case "unprivileged":
		p = newPinger(false)
		err = p.RunWithContext(ctx)
	default: // auto: unprivileged ICMP first, raw sockets when it is not allowed
		p = newPinger(false)
		if err = p.RunWithContext(ctx); err != nil && isPermission(err) {
			p = newPinger(true)
			err = p.RunWithContext(ctx)
		}
	}
	if err != nil {
		return plugin.Result{}, fmt.Errorf("ping %s: %w", t.Address, err)
	}

	st := p.Statistics()
	r := plugin.Result{Metrics: []float64{math.NaN(), math.NaN(), st.PacketLoss}}
	if st.PacketsRecv == 0 {
		r.Status = plugin.Critical
		r.Output = fmt.Sprintf("%s (%s): no reply to %d echo requests", t.Address, ip, st.PacketsSent)
		return r, nil
	}
	r.Metrics[0], r.Metrics[1] = ms(st.AvgRtt), ms(st.MaxRtt)
	r.Status = plugin.OK
	r.Output = fmt.Sprintf("%s (%s): %d/%d replies, avg %.2f ms, max %.2f ms, loss %.0f %%",
		t.Address, ip, st.PacketsRecv, st.PacketsSent, r.Metrics[0], r.Metrics[1], st.PacketLoss)
	return r, nil
}

func isPermission(err error) bool {
	s := err.Error()
	return errors.Is(err, os.ErrPermission) || strings.Contains(s, "permission denied") ||
		strings.Contains(s, "operation not permitted")
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
