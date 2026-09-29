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
	r := plugin.SafeRun(ctx, c, target)

	if n := len(c.Manifest().Metrics); len(r.Metrics) != n {
		t.Errorf("result has %d metrics, manifest declares %d", len(r.Metrics), n)
	}
	for role, cred := range o.Credentials {
		for field, secret := range cred.Secret {
			v := secret.Reveal()
			if v == "" {
				continue
			}
			if strings.Contains(r.Output, v) {
				t.Errorf("result output contains the secret %s.%s", role, field)
			}
			if strings.Contains(logs.String(), v) {
				t.Errorf("plugin logs contain the secret %s.%s", role, field)
			}
		}
	}
	return r
}
