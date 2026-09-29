# ADR-0004: Plugins are compiled in and registered at init

**Status:** Proposed · **Date:** 2026-09-27

## Context

New protocols and device types must be added as plugins, never as core changes. Plugins run both in the core and in remote probes. The plugin API is the `Check`, `Collector` and `Ingester` interfaces from the design document.

## Options considered

1. **Go `plugin` package (`.so` loading).** Linux/macOS only, requires identical toolchain and dependency versions between host and plugin, breaks static binaries. Rejected.
2. **Out-of-process plugins over RPC (HashiCorp `go-plugin`).** Crash isolation and third-party binaries, but a process per plugin type, harder packaging for probes, and a much larger surface for a first version.
3. **Compiled-in plugins with a registry.** Each plugin is a Go package that registers itself in `init()`. The build decides which plugins exist. Third parties add plugins by forking or by a build tag.
4. **Exec plugins.** Run an external program, read its exit code and output (Nagios plugin convention). Opens the huge existing Nagios/Icinga plugin ecosystem.

## Decision

Option 3 now, option 4 as a built-in plugin type in phase 2.

- Public interfaces and types live in `pkg/plugin` so they can be versioned separately from internal code.
- Each plugin declares a **manifest**: type name (`snmp.interfaces`), kind (check / collector / ingester), config struct, metric layout (names, units, retention class), required credential types, and whether it needs raw sockets.
- Config validation uses a **JSON Schema generated from the config struct** (`invopop/jsonschema`). The API exposes all manifests at `GET /v1/plugins`, so the panel can render forms without hard-coding plugin knowledge.
- Plugins receive decrypted credentials through the `Target` value and must never log them. A lint rule and a test helper enforce this for built-in plugins.
- Every plugin run gets a `context.Context` with the check timeout; plugins must honor cancellation. The runner also enforces a hard per-plugin concurrency cap.
- **Exec plugins (phase 2)** run only on probes or on single-tenant installs, never on a shared multi-tenant core, because they execute arbitrary programs. They follow the Nagios convention: exit code 0/1/2/3 maps to OK/WARNING/CRITICAL/UNKNOWN, and performance data after `|` becomes metrics.

## Consequences

- Static binaries and a single container image stay possible.
- A panicking plugin can crash a runner; the runner wraps every plugin call in `recover()` and turns panics into an UNKNOWN result with the stack trace in the output.
- Adding a plugin requires a release. This is acceptable while all plugins are ours; exec plugins cover ad-hoc needs.
- The `pkg/plugin` API becomes a compatibility promise once external contributors depend on it; it stays pre-1.0 until phase 3.
