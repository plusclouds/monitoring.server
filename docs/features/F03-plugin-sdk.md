# F03: Plugin SDK and registry

**Status:** Draft · **Phase:** MVP · **Related:** [ADR-0004](../adr/0004-compiled-in-plugin-registry.md)

## Summary

The public Go package `pkg/plugin` defines how checks, collectors and ingesters are written. Built-in plugins register themselves; the API exposes their manifests so clients can build forms and validate configs.

## Types

```go
package plugin

type Kind string // "check", "collector", "ingester"

type Manifest struct {
    Type            string          // "http", "snmp.interfaces", "xapi.pool"
    Kind            Kind
    Description     string
    ConfigSchema    json.RawMessage // generated from the config struct
    CredentialTypes []string        // accepted credential types, if any
    Metrics         []MetricDef     // fixed layout; collectors may add per-object
    DefaultInterval time.Duration
    MinInterval     time.Duration   // e.g. SNMP table walks >= 60s
    NeedsRawSocket  bool            // ICMP
    MaxConcurrency  int             // per runner; 0 = default
    PerTargetLimit  int             // concurrent runs against one address
    BillingClass    string          // "basic", "standard", "push", "advanced", "free" (F13)
}

type MetricDef struct {
    Name, Unit, Description string
    Kind           string // "gauge", "rate" (plugin computes from counters)
    RetentionClass string // "standard", "high-frequency", "capacity"
}

type Target struct {
    DeviceID    string
    Address     string
    Config      json.RawMessage
    Credentials map[string]Secret // decrypted; Secret redacts itself in logs
    State       StateStore        // small per-check key/value, e.g. last counters
}

type Result struct {
    Status   Status             // OK, WARNING, CRITICAL, UNKNOWN
    Output   string             // human-readable, max 4 KB
    Metrics  []float64          // aligned with Manifest.Metrics; NaN if absent
    Duration time.Duration
    Time     time.Time
}

type Batch struct {
    Objects []ObjectMetrics // per child object: key, metric layout, values
}

type Inventory struct {
    Device   *DeviceInfo      // vendor, model, serial, firmware
    Children []ChildDevice    // VMs, interfaces, sensors
}
```

The `Check`, `Collector` and `Ingester` interfaces are as in the design document, with `Manifest() Manifest` replacing `Type()`.

## Behavior

- **Registration:** `plugin.Register(p)` in the plugin package's `init()`. `cmd/monitor` imports `plugins/all`, which imports every built-in plugin.
- **Validation:** configs are validated against `ConfigSchema` in the API before saving, then by `Validate` for rules the schema cannot express.
- **State store:** a small per-check key/value (max 64 KB) persisted by the runner between runs. Used for counter deltas (`sysUpTime`, previous octet counters) so plugins can compute rates and detect resets. Lost state only costs one interval of rate data.
- **Status from thresholds:** plugins report raw metrics and a protocol-level status (reachable, authentication failed). Metric thresholds (CPU > 90 %) are *not* evaluated in plugins; the engine evaluates them ([F05](F05-state-and-incidents.md)), so thresholds work the same for every plugin. A plugin may still return CRITICAL directly for protocol facts (HTTP 500, certificate expired, PSU failed as reported by Redfish).
- **Safety:** each call is wrapped in `recover()`; timeouts come from the context; per-target limits prevent overloading one device.

## Test kit

`pkg/plugin/plugintest` provides a harness: run a plugin against a recorded fixture or simulator, assert metrics and status, and assert that no secret appears in output or logs. Every built-in plugin must have tests using it.

## API

`GET /v1/plugins` and `GET /v1/plugins/{type}` return manifests.

## Acceptance criteria

- A new check plugin can be added in one package without editing any other file except `plugins/all`.
- A plugin that panics or ignores its context produces an UNKNOWN result with a clear message and does not stall the runner.
- `GET /v1/plugins` returns valid JSON Schema for every plugin (validated in CI).

## Open questions

- Retention classes: proposed initial set is `high-frequency` (5–30 s checks), `standard`, `capacity` (disk, storage repository, license counts). Retention durations per class are set through the API with no hardcoded defaults; the installer should still propose values.
