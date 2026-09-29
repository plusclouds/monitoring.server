package plugin_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

type fake struct {
	run func(ctx context.Context) (plugin.Result, error)
}

func (f fake) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Type: "fake", Kind: plugin.KindCheck, ConfigSchema: json.RawMessage(`{}`),
		Metrics: []plugin.MetricDef{
			{Name: "a", Kind: "gauge", RetentionClass: plugin.RetentionStandard},
			{Name: "b", Kind: "gauge", RetentionClass: plugin.RetentionStandard},
		},
		DefaultInterval: time.Minute, MinInterval: time.Second, BillingClass: plugin.BillingBasic,
	}
}
func (fake) Validate(json.RawMessage) error                                    { return nil }
func (f fake) Run(ctx context.Context, _ plugin.Target) (plugin.Result, error) { return f.run(ctx) }

func withTimeout(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// F03: a plugin that panics or ignores its context produces UNKNOWN with a
// clear message and does not stall the runner.
func TestSafeRun(t *testing.T) {
	r := plugin.SafeRun(withTimeout(t, time.Second), fake{run: func(context.Context) (plugin.Result, error) { panic("boom") }}, plugin.Target{})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "plugin panicked: boom") {
		t.Errorf("panic: got %v %q", r.Status, r.Output)
	}

	start := time.Now()
	r = plugin.SafeRun(withTimeout(t, 100*time.Millisecond), fake{run: func(context.Context) (plugin.Result, error) {
		time.Sleep(5 * time.Second)
		return plugin.Result{}, nil
	}}, plugin.Target{})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "did not stop at its timeout") {
		t.Errorf("stuck plugin: got %v %q", r.Status, r.Output)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("stuck plugin held the caller for %s", took)
	}

	r = plugin.SafeRun(withTimeout(t, time.Second), fake{run: func(context.Context) (plugin.Result, error) {
		return plugin.Result{}, errors.New("auth failed")
	}}, plugin.Target{})
	if r.Status != plugin.Unknown || r.Output != "auth failed" {
		t.Errorf("error: got %v %q", r.Status, r.Output)
	}
	if len(r.Metrics) != 2 || !math.IsNaN(r.Metrics[0]) {
		t.Errorf("metrics must match the manifest with NaN placeholders: %v", r.Metrics)
	}

	r = plugin.SafeRun(withTimeout(t, time.Second), fake{run: func(context.Context) (plugin.Result, error) {
		return plugin.Result{Status: plugin.OK, Output: strings.Repeat("x", 10000), Metrics: []float64{1}}, nil
	}}, plugin.Target{})
	if len(r.Output) != plugin.MaxOutput || r.Metrics[0] != 1 || !math.IsNaN(r.Metrics[1]) || r.Time.IsZero() {
		t.Errorf("normalize: output %d bytes, metrics %v", len(r.Output), r.Metrics)
	}
}

func TestSecretNeverPrints(t *testing.T) {
	s := plugin.Secret("hunter2")
	for _, got := range []string{s.String(), s.GoString(), s.LogValue().String()} {
		if got != "[redacted]" {
			t.Errorf("secret printed as %q", got)
		}
	}
	b, _ := json.Marshal(map[string]plugin.Secret{"p": s})
	if strings.Contains(string(b), "hunter2") {
		t.Errorf("secret in JSON: %s", b)
	}
	if s.Reveal() != "hunter2" {
		t.Error("Reveal must return the value")
	}
}

func TestNetPolicy(t *testing.T) {
	p := func(s string) netip.Prefix { return netip.MustParsePrefix(s) }
	a := func(s string) netip.Addr { return netip.MustParseAddr(s) }
	customer := &plugin.NetPolicy{Deny: []netip.Prefix{p("169.254.169.254/32")}, DenyPrivate: true}
	allowed := &plugin.NetPolicy{Allow: []netip.Prefix{p("10.1.0.0/16")}, Deny: []netip.Prefix{p("10.1.9.0/24")}}
	var none *plugin.NetPolicy

	cases := []struct {
		policy *plugin.NetPolicy
		ip     string
		ok     bool
	}{
		{customer, "93.184.216.34", true},
		{customer, "10.0.0.1", false},
		{customer, "127.0.0.1", false},
		{customer, "::1", false},
		{customer, "fe80::1", false},
		{customer, "169.254.169.254", false},
		{customer, "::ffff:192.168.1.1", false},
		{allowed, "10.1.2.3", true},
		{allowed, "10.2.0.1", false},
		{allowed, "10.1.9.9", false},
		{none, "127.0.0.1", true},
	}
	for _, c := range cases {
		err := c.policy.Check(a(c.ip))
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want allowed=%v", c.ip, err, c.ok)
		}
		if _, isPolicy := errors.AsType[*plugin.PolicyError](err); err != nil && !isPolicy {
			t.Errorf("%s: error is not a *PolicyError", c.ip)
		}
	}
}

func TestMemStateLimit(t *testing.T) {
	var s plugin.MemState
	if err := s.Set("a", make([]byte, plugin.MaxStateSize)); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("b", []byte{1}); err == nil {
		t.Error("state above 64 KiB should be refused")
	}
	if err := s.Set("a", []byte{1}); err != nil {
		t.Errorf("replacing a value should free space: %v", err)
	}
}

type badInterval struct{ fake }

func (badInterval) Manifest() plugin.Manifest {
	m := fake{}.Manifest()
	m.Type, m.DefaultInterval = "bad", time.Millisecond
	return m
}

func TestRegisterRejectsBadManifest(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Register should panic on an invalid manifest")
		}
	}()
	plugin.Register(badInterval{})
}
