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

// Result of one check run.
type Result struct {
	Status   Status
	Output   string    // human-readable, at most 4 KiB
	Metrics  []float64 // aligned with Manifest.Metrics; NaN when not collected
	Duration time.Duration
	Time     time.Time
}

// MaxOutput is the longest Result.Output kept.
const MaxOutput = 4 << 10

// Check tests one thing and returns a status and metrics.
type Check interface {
	Manifest() Manifest
	Validate(cfg json.RawMessage) error // rules the JSON Schema cannot express
	Run(ctx context.Context, target Target) (Result, error)
}

// Collector makes one connection and returns many metrics plus inventory.
// Its types are completed with the first collector (phase: F10).
type Collector interface {
	Manifest() Manifest
	Validate(cfg json.RawMessage) error
	Collect(ctx context.Context, target Target) (Batch, Inventory, error)
}

// Ingester receives pushed data (F08).
type Ingester interface {
	Manifest() Manifest
	Start(ctx context.Context, sink ResultSink) error
}

// Batch holds per-object metrics of a collector run.
type Batch struct {
	Objects []ObjectMetrics
}

// ObjectMetrics are the metrics of one child object (interface, VM, fan).
type ObjectMetrics struct {
	Key     string
	Metrics []float64
	Status  Status
	Output  string
}

// Inventory is what a collector learned about a device and its children.
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
