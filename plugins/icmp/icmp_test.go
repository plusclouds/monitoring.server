package icmp

import (
	"strings"
	"testing"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
	"github.com/plusclouds/monitoring.server/pkg/plugin/plugintest"
)

// skipWithoutICMP skips when the host allows neither unprivileged ICMP nor
// raw sockets for this user.
func skipWithoutICMP(t *testing.T, r plugin.Result) {
	t.Helper()
	if r.Status == plugin.Unknown && (strings.Contains(r.Output, "permission") || strings.Contains(r.Output, "not permitted")) {
		t.Skip("ICMP not permitted here:", r.Output)
	}
}

func TestPingLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "::1"} {
		t.Run(addr, func(t *testing.T) {
			r := plugintest.Run(t, &Check{mode: "auto"}, plugintest.Options{
				Address: addr, Config: Config{Count: 2, IntervalMS: 20}, Timeout: 3 * time.Second,
			})
			skipWithoutICMP(t, r)
			if r.Status != plugin.OK || r.Metrics[2] != 0 || r.Metrics[0] <= 0 {
				t.Fatalf("got %v %q metrics %v", r.Status, r.Output, r.Metrics)
			}
		})
	}
}

func TestPingNoReply(t *testing.T) {
	// 192.0.2.0/24 is TEST-NET-1: never routed, so no reply comes back.
	r := plugintest.Run(t, &Check{mode: "auto"}, plugintest.Options{
		Address: "192.0.2.1", Config: Config{Count: 1}, Timeout: 700 * time.Millisecond,
	})
	skipWithoutICMP(t, r)
	if r.Status != plugin.Critical || r.Metrics[2] != 100 {
		t.Fatalf("got %v %q metrics %v", r.Status, r.Output, r.Metrics)
	}
}

func TestPingPolicy(t *testing.T) {
	r := plugintest.Run(t, &Check{mode: "auto"}, plugintest.Options{
		Address: "127.0.0.1", Network: &plugin.NetPolicy{DenyPrivate: true},
	})
	if r.Status != plugin.Unknown || !strings.Contains(r.Output, "private or local") {
		t.Fatalf("got %v %q", r.Status, r.Output)
	}
}

func TestPingValidate(t *testing.T) {
	if err := (&Check{}).Validate([]byte(`{"count":3,"bogus":true}`)); err == nil {
		t.Error("unknown field should be rejected")
	}
	if err := (&Check{}).Configure(plugin.Settings{ICMPMode: "raw"}); err == nil {
		t.Error("unknown ICMP mode should be rejected")
	}
}
