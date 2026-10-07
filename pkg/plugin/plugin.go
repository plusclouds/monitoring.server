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
	"strings"
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
	// CredentialsRequired: the plugin cannot run without a credential in
	// the "auth" role. False when the credential is optional or there is none.
	CredentialsRequired bool `json:"credentials_required"`
	// Objects: an ingester whose results carry objects (a VM's disks and
	// interfaces), kept like a collector's. Collectors always do.
	Objects         bool          `json:"-"`
	Metrics         []MetricDef   `json:"metrics"` // fixed layout; Result.Metrics aligns with it
	DefaultInterval time.Duration `json:"-"`
	MinInterval     time.Duration `json:"-"`
	NeedsRawSocket  bool          `json:"needs_raw_socket"`
	MaxConcurrency  int           `json:"-"` // per runner; 0 = runner default
	PerTargetLimit  int           `json:"-"` // concurrent runs against one address; 0 = runner default
	Slow            bool          `json:"-"` // runs in the slow pool (F04)
	BillingClass    string        `json:"billing_class"`
	// WhoopsyMetric is the metric Whoopsy! (band alerting) watches unless
	// the check names another: response time for http, round-trip time
	// for icmp. Empty: the check must name one.
	WhoopsyMetric string `json:"whoopsy_metric,omitempty"`
	// BillableObjects are the object kinds of a collector billed on their
	// own, by object key prefix: {"host:": "host"} bills every host object
	// as "<type>:host" (F13). Other objects are never billed.
	BillableObjects map[string]string `json:"billable_objects,omitempty"`
}

// BillingKey is the billing key of a collector object, or "" when the
// object is not billed on its own.
func (m Manifest) BillingKey(objectKey string) string {
	for prefix, kind := range m.BillableObjects {
		if strings.HasPrefix(objectKey, prefix) {
			return m.Type + ":" + kind
		}
	}
	return ""
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
	// Inventory is what a successful collection discovered: child devices
	// (hosts, VMs) the engine creates, moves and removes. Nil otherwise.
	Inventory *Inventory
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
	// Device is the key of a child device of the same run's inventory the
	// object belongs to (a VM's metrics and incidents go to the VM's
	// device); empty for the collector's own device.
	Device string
	// Availability marks the object whose status says whether its device
	// is up, like a host check: a CRITICAL incident of it suppresses the
	// incidents of everything that depends on the device (an XCP-ng host
	// down suppresses its VMs).
	Availability bool
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

// Ingester receives pushed data instead of running (F08). The ingest role
// accepts a message for one of its checks and the plugin turns it into
// results. Its metrics are named by each check's config, so the layout is
// per check: Manifest.Metrics is empty.
type Ingester interface {
	Plugin
	// MetricsFor is the metric layout of a check with this (valid) config.
	MetricsFor(cfg json.RawMessage) ([]MetricDef, error)
	// Parse turns one pushed message into results, oldest first, with
	// Metrics aligned with MetricsFor(cfg). A zero Time means the time the
	// message arrived. An error means the message is malformed.
	Parse(cfg json.RawMessage, body []byte) ([]Result, error)
}

// HasObjects reports whether a plugin's results carry objects with their
// own metrics, state and thresholds.
func (m Manifest) HasObjects() bool { return m.Kind == KindCollector || m.Objects }

// MetricsOf is the metric layout of a check: the manifest's, or for an
// ingester the one its config names (nil when the config is invalid).
func MetricsOf(p Plugin, cfg json.RawMessage) []MetricDef {
	if in, ok := p.(Ingester); ok {
		defs, err := in.MetricsFor(cfg)
		if err != nil {
			return nil
		}
		return defs
	}
	return p.Manifest().Metrics
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
// Children nil means the collector does not discover devices; an empty
// list means the device has none now (they are removed after a while).
type Inventory struct {
	Device   *DeviceInfo
	Children []ChildDevice
}

// DeviceInfo is a device's hardware and software identity.
type DeviceInfo struct {
	Vendor, Model, Serial, Firmware string
}

// ChildDevice is a device a collector discovered: a pool's host, a host's
// VM. Key is stable for the collector (a UUID) and survives renames and
// migrations.
type ChildDevice struct {
	Key, Name, Type, Address string
	// ParentKey is the key of another child that contains this one (the
	// host a VM runs on); empty for the collector's own device.
	ParentKey string
	Info      DeviceInfo
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
