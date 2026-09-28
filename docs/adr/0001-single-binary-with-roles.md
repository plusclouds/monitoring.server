# ADR-0001: One Go binary with switchable roles

**Status:** Proposed · **Date:** 2026-09-27

## Context

The design asks for a system that is simple to run for a small install (one VM, a few hundred devices) and that scales to 10,000+ devices with remote sites. The same code must run in our datacenter, inside the PlusClouds panel stack and at customer sites. Remote probes must run the same check plugins as the core.

## Options considered

1. **Microservices** (separate API, scheduler, engine, notifier, probe services). Clean boundaries, but every install needs orchestration, service discovery and version-skew handling from day one. Too heavy for a small install.
2. **Monolith with no internal boundaries.** Easiest to start, but cannot be split later without a rewrite.
3. **One binary, several roles.** Each role is an internal component with an explicit interface. A flag chooses which roles a process runs. In-process roles talk through Go interfaces and channels; split roles talk through PostgreSQL (see [ADR-0002](0002-postgresql-as-the-only-infrastructure-dependency.md)) or the probe protocol (see [ADR-0005](0005-probe-protocol.md)).

## Decision

Option 3. The binary is called `monitor` and has two commands:

| Command | Roles | Notes |
| --- | --- | --- |
| `monitor serve` | `api`, `runner`, `engine`, `notifier`, `ingest`, `maintenance` (partition management, rollups, retention) | `--roles` selects a subset; default is all |
| `monitor probe` | `runner` only, with a remote result sink | Connects out to the core; no database access |

Role boundaries:

- **api** — REST API, auth, audit, SSE event stream. Stateless.
- **runner** — owns a set of scheduler shards; schedules and executes checks and collectors for them. The scheduler and workers are one component, so there is no work queue between them (see [F04](../features/F04-scheduler-and-runner.md)).
- **engine** — consumes results, runs the state machine, writes state, incidents and metrics. Also serves the probe protocol ([ADR-0005](0005-probe-protocol.md)) on its own listener, because probe results go straight into the engine.
- **notifier** — delivers outbox events as signed webhooks.
- **ingest** — MQTT subscriber, HTTP push endpoint and (phase 2) SNMP trap and syslog listeners. Runs on its own listener, separate from the management API, as the security section requires.
- **maintenance** — creates and drops metric partitions, computes rollups. Runs under a PostgreSQL advisory lock so only one instance is active.

In the MVP, `runner` and `engine` always run in the same process: results pass through a channel. Splitting them is a phase 2 option, only if a load test shows the need.

## Consequences

- A small install is one process plus PostgreSQL.
- Every component needs a clean interface from the start, which slows the first weeks slightly.
- Probe and core share plugin code, so a check behaves the same wherever it runs.
- Version skew between core and probes is the main cross-process risk; the probe protocol carries a version and the core refuses incompatible probes with a clear error.
