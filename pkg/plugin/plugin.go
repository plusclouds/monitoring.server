// Package plugin is the public SDK for checks, collectors and ingesters
// (F03, ADR-0004).
//
// A plugin is a Go package that calls Register in its init function and is
// imported by plugins/all. It declares a Manifest: its type name, config
// schema, metric layout, credential types and limits. The engine, not the
// plugin, evaluates metric thresholds; a plugin reports raw metrics plus a
// status for protocol facts (unreachable, HTTP 500, PSU failed).
//
// This package is a compatibility surface: it stays pre-1.0 until phase 3.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// Kind of plugin.
type Kind string

const (
	KindCheck     Kind = "check"
	KindCollector Kind = "collector"
	KindIngester  Kind = "ingester"
)

// Status of a result.
type Status int

const (
	OK Status = iota
	Warning
	Critical
	Unknown
)

func (s Status) String() string {
	switch s {
	case OK:
		return "OK"
	case Warning:
		return "WARNING"
	case Critical:
		return "CRITICAL"
	default:
		return "UNKNOWN"
	}
}

// MarshalText makes statuses readable in JSON and logs.
func (s Status) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// Billing classes (F13).
const (
	BillingBasic    = "basic"
	BillingStandard = "standard"
	BillingPush     = "push"
	BillingAdvanced = "advanced"
	BillingFree     = "free"
)

// Retention classes (F03).
const (
	RetentionHighFrequency = "high-frequency"
	RetentionStandard      = "standard"
	RetentionCapacity      = "capacity"
)

// Manifest describes a plugin. The API serves manifests at /v1/plugins so
// clients can build forms without hard-coding plugin knowledge.
type Manifest struct {
	Type            string          `json:"type"` // "icmp", "http", "snmp.interfaces"
	Kind            Kind            `json:"kind"`
	Description     string          `json:"description"`
	ConfigSchema    json.RawMessage `json:"config_schema"` // from SchemaFor
	CredentialTypes []string        `json:"credential_types"`
	Metrics         []MetricDef     `json:"metrics"` // fixed layout; Result.Metrics aligns with it
	DefaultInterval time.Duration   `json:"-"`
	MinInterval     time.Duration   `json:"-"`
	NeedsRawSocket  bool            `json:"needs_raw_socket"`
	MaxConcurrency  int             `json:"-"` // per runner; 0 = runner default
	PerTargetLimit  int             `json:"-"` // concurrent runs against one address; 0 = runner default
	Slow            bool            `json:"-"` // runs in the slow pool (F04)
	BillingClass    string          `json:"billing_class"`
	// WhoopsyMetric is the metric Whoopsy! (band alerting) watches unless
	// the check names another: response time for http, round-trip time
	// for icmp. Empty: the check must name one.
	WhoopsyMetric string `json:"whoopsy_metric,omitempty"`
}

// MetricDef is one slot of a plugin's metric layout.
type MetricDef struct {
	Name           string `json:"name"`
	Unit           string `json:"unit"` // SI base units where possible: "ms", "percent", "bytes", "1"
	Description    string `json:"description"`
	Kind           string `json:"kind"` // "gauge" or "rate" (computed by the plugin from counters)
	RetentionClass string `json:"retention_class"`
}

// Secret is a decrypted credential value. It never prints or logs its value.
type Secret string

func (Secret) String() string               { return "[redacted]" }
func (Secret) GoString() string             { return "[redacted]" }
func (Secret) LogValue() slog.Value         { return slog.StringValue("[redacted]") }
func (Secret) MarshalText() ([]byte, error) { return []byte("[redacted]"), nil }

// Reveal returns the value for use in a protocol call. Never log the result.
func (s Secret) Reveal() string { return string(s) }

// Credential is a decrypted credential of one of the manifest's types.
type Credential struct {
	Type   string            // "http_basic", "snmp_v3", ...
	Fields map[string]string // non-secret fields: username, auth protocol, ...
	Secret map[string]Secret // secret fields: password, community, ...
}

// StateStore is a small per-check key/value store kept between runs, for
// counter deltas and reset detection. Values are limited to 64 KiB in total.
type StateStore interface {
	Get(key string) ([]byte, bool)
	Set(key string, value []byte) error
}

// Target is what a plugin runs against.
type Target struct {
	DeviceID    string
	Address     string                // IP, hostname or URL
	Config      json.RawMessage       // validated against the manifest's schema
	Credentials map[string]Credential // by role name, e.g. "auth", "bmc"
	State       StateStore
	Network     *NetPolicy // outgoing connection policy; nil = no restriction
	Log         *slog.Logger
}

// Result of one check or collector run.
type Result struct {
	Status   Status
	Output   string    // human-readable, at most 4 KiB
	Metrics  []float64 // checks: aligned with Manifest.Metrics; NaN when not collected
	Duration time.Duration
	Time     time.Time
	// Objects are a collector's per-object results. Nil means the
	// collection failed (the run's Status says why) and says nothing about
	// the objects; an empty slice means the device has no objects now.
	Objects []Object
}

// Object is one part of a device reported by a collector: an interface, a
// fan, a disk (ADR-0014). It has its own metrics, thresholds, state and
// incidents, but no checks, credentials or billing of its own.
type Object struct {
	// Key identifies the object across runs; it must stay the same when
	// the device renumbers its parts (an interface by name, not ifIndex).
	// At most 200 bytes.
	Key    string
	Name   string            // shown to people; may change
	Labels map[string]string // descriptive: alias, type, speed
	Status Status            // protocol facts (interface down, PSU failed)
	Output string            // at most 1 KiB
	// Metrics are aligned with Manifest.Metrics: a collector's metric
	// layout is per object.
	Metrics []float64
}

// MaxOutput is the longest Result.Output kept.
const MaxOutput = 4 << 10

// Plugin is what every check and collector has: a manifest and config
// validation beyond the JSON Schema.
type Plugin interface {
	Manifest() Manifest
	Validate(cfg json.RawMessage) error // rules the JSON Schema cannot express
}

// Check tests one thing and returns a status and metrics.
type Check interface {
	Plugin
	Run(ctx context.Context, target Target) (Result, error)
}

// Collector makes one connection and returns the state and metrics of many
// objects (interfaces, fans, VMs), plus inventory.
type Collector interface {
	Plugin
	Collect(ctx context.Context, target Target) (Batch, Inventory, error)
}

// Ingester receives pushed data (F08).
type Ingester interface {
	Manifest() Manifest
	Start(ctx context.Context, sink ResultSink) error
}

// Batch is a collector run: the device-level status and output (a summary
// such as "48 interfaces, 2 down") and every object found. Objects nil
// means the collection failed (device unreachable: Status CRITICAL); an
// error from Collect is UNKNOWN, a problem of the monitoring itself.
type Batch struct {
	Status  Status
	Output  string
	Objects []Object
}

// Inventory is what a collector learned about a device and its children.
// The engine does not apply it yet; collectors that create child devices
// (XCP-ng) complete it.
type Inventory struct {
	Device   *DeviceInfo
	Children []ChildDevice
}

type DeviceInfo struct {
	Vendor, Model, Serial, Firmware string
}

type ChildDevice struct {
	Key, Name, Type, Address string
	Info                     DeviceInfo
}

// ResultSink receives results from ingesters.
type ResultSink interface {
	Submit(ctx context.Context, checkID string, r Result) error
}

// Settings are runner-wide options (F04) that some plugins need, passed to
// plugins implementing Configurable when the runner starts.
type Settings struct {
	ICMPMode     string // "auto", "privileged", "unprivileged"
	SourceIPv4   string
	SourceIPv6   string
	DNSResolvers []string
}

// Configurable plugins receive runner settings before their first run.
type Configurable interface {
	Configure(Settings) error
}

// Errorf builds a result with a status and a formatted output.
func Errorf(status Status, format string, args ...any) Result {
	return Result{Status: status, Output: fmt.Sprintf(format, args...)}
}
