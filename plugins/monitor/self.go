// Package monitor implements monitor.self (F11): the engine's own health,
// reported every minute by the engine process on the built-in "monitor"
// device of the platform tenant, so its thresholds alert through the normal
// routes and its metrics are graphable without Prometheus.
package monitor

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/plusclouds/monitoring.server/pkg/plugin"
)

func init() { plugin.Register(&Self{}) }

// Metric slots of monitor.self.
const (
	CheckLagP99 = iota
	ResultsPerMinute
	OutboxOldest
	OutboxDepth
	MetricsDropped
	RollupLag
	PartitionsMissing
	WebhooksFailing
	ClockOffset
	APIAuthFailures
	APIRateLimited
	MQTTAuthFailures
	MQTTConnections
	NumMetrics
)

var defs = []plugin.MetricDef{
	g("check_lag_p99_seconds", "s", "Runner: 99th percentile of scheduled-to-start lag over the last 5 minutes"),
	g("results_per_minute", "1/min", "Engine: results applied in the last minute"),
	g("outbox_oldest_seconds", "s", "Notifier: age of the oldest undelivered event"),
	g("outbox_depth", "events", "Notifier: undelivered events"),
	g("metrics_dropped_5m", "rows", "Metrics store: rows dropped in the last 5 minutes"),
	g("rollup_lag_seconds", "s", "Metrics store: how far rollups trail"),
	g("partitions_missing", "count", "Metrics store: retention classes without tomorrow's partition"),
	g("webhook_endpoints_failing", "count", "Endpoints with undelivered events older than 1 hour and no success since"),
	g("clock_offset_seconds", "s", "This node's clock minus the database's"),
	g("api_auth_failures_5m", "count", "Failed API authentications in the last 5 minutes"),
	g("api_rate_limited_5m", "count", "Rate-limited API requests in the last 5 minutes"),
	g("mqtt_auth_failures_5m", "count", "Refused MQTT logins and topics in the last 5 minutes"),
	g("mqtt_connections", "count", "Open MQTT connections on this node"),
}

func g(name, unit, desc string) plugin.MetricDef {
	return plugin.MetricDef{Name: name, Unit: unit, Description: desc, Kind: "gauge", RetentionClass: plugin.RetentionStandard}
}

// Metrics is the fixed layout.
func Metrics() []plugin.MetricDef { return defs }

// Self implements monitor.self.
type Self struct{}

func (*Self) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Type: "monitor.self",
		Kind: plugin.KindIngester,
		Description: "The monitoring engine's own health, reported every minute by the engine process: check lag, " +
			"outbox, metrics store, webhooks, clock and authentication failures. Created on the platform tenant's " +
			"\"monitor\" device.",
		ConfigSchema:    plugin.SchemaFor[struct{}](),
		DefaultInterval: time.Minute,
		MinInterval:     time.Minute,
		BillingClass:    plugin.BillingFree,
	}
}

func (*Self) Validate(json.RawMessage) error { return nil }

func (*Self) MetricsFor(json.RawMessage) ([]plugin.MetricDef, error) { return defs, nil }

func (*Self) Parse(json.RawMessage, []byte) ([]plugin.Result, error) {
	return nil, errors.New("monitor.self is reported by the engine itself")
}
