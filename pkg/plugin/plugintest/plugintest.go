// Package plugintest runs plugins in tests (F03). Every built-in plugin must
// have tests using it. Run fails the test when a secret appears in the
// result output or in anything the plugin logged, or when the metric layout
// does not match the manifest.
package plugintest

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

// Options describe one run.
type Options struct {
	Address     string
	Config      any // marshalled to JSON; nil = no config
	Credentials map[string]plugin.Credential
	Network     *plugin.NetPolicy
	Timeout     time.Duration // default 10 s
	State       plugin.StateStore
}

// Run validates the config against the plugin, runs it through
// plugin.SafeRun and checks the result.
func Run(t testing.TB, c plugin.Check, o Options) plugin.Result {
	t.Helper()
	return run(t, c, o, func(ctx context.Context, target plugin.Target) plugin.Result {
		return plugin.SafeRun(ctx, c, target)
	})
}

// Collect does the same for a collector, through plugin.SafeCollect. It
// also checks every object's metric layout and output.
func Collect(t testing.TB, c plugin.Collector, o Options) plugin.Result {
	t.Helper()
	return run(t, c, o, func(ctx context.Context, target plugin.Target) plugin.Result {
		return plugin.SafeCollect(ctx, c, target)
	})
}

func run(t testing.TB, c plugin.Plugin, o Options, exec func(context.Context, plugin.Target) plugin.Result) plugin.Result {
	t.Helper()
	var raw json.RawMessage
	if o.Config != nil {
		b, err := json.Marshal(o.Config)
		if err != nil {
			t.Fatal(err)
		}
		raw = b
	}
	if err := c.Validate(raw); err != nil {
		t.Fatalf("config rejected: %v", err)
	}
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Second
	}
	if o.State == nil {
		o.State = &plugin.MemState{}
	}
	var logs bytes.Buffer
	target := plugin.Target{
		DeviceID: "test-device", Address: o.Address, Config: raw, Credentials: o.Credentials,
		State: o.State, Network: o.Network,
		Log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
	defer cancel()
	r := exec(ctx, target)

	m := c.Manifest()
	outputs := []string{r.Output}
	if m.Kind == plugin.KindCollector {
		for _, obj := range r.Objects {
			if len(obj.Metrics) != len(m.Metrics) {
				t.Errorf("object %s has %d metrics, manifest declares %d", obj.Key, len(obj.Metrics), len(m.Metrics))
			}
			outputs = append(outputs, obj.Output, obj.Name)
		}
	} else if len(r.Metrics) != len(m.Metrics) {
		t.Errorf("result has %d metrics, manifest declares %d", len(r.Metrics), len(m.Metrics))
	}
	all := strings.Join(outputs, "\n")
	for role, cred := range o.Credentials {
		for field, secret := range cred.Secret {
			v := secret.Reveal()
			if v == "" {
				continue
			}
			if strings.Contains(all, v) {
				t.Errorf("result output contains the secret %s.%s", role, field)
			}
			if strings.Contains(logs.String(), v) {
				t.Errorf("plugin logs contain the secret %s.%s", role, field)
			}
		}
	}
	return r
}
