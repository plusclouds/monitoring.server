# ADR-0011: Toolchain, repository layout and conventions

**Status:** Proposed · **Date:** 2026-09-27

## Context

Before the first line of code, the team needs agreement on layout and tooling so that work on separate features fits together.

## Decision

### Toolchain

- **Go:** latest stable release at project start, pinned in `go.mod` (`go` and `toolchain` directives). Upgrade within one minor release of upstream.
- **Build:** `CGO_ENABLED=0` static binaries for linux/amd64, linux/arm64 (probes on small hardware), and later windows/amd64 for probes. Release with GoReleaser; container images based on `gcr.io/distroless/static` plus a debug variant.
- **Database migrations:** `goose` with SQL files embedded via `embed.FS`, run at startup under an advisory lock; `monitor migrate status|up|down` for operators.
- **Logging:** standard library `log/slog`, JSON output, with `tenant_id`, `device_id`, `check_id`, `request_id` as standard attributes. Secrets are wrapped in a type whose `LogValue` returns `"[redacted]"`.
- **Self-metrics:** Prometheus exposition at `/metrics` on the internal listener ([F11](../features/F11-self-monitoring.md)).
- **Configuration:** one YAML file for process-level settings (listen addresses, database DSN, key provider, roles), overridable by environment variables named `MONITOR_` plus the key path in upper case with `__` between levels (`database.app.dsn` → `MONITOR_DATABASE__APP__DSN`). Secrets are read from files through `*_file` keys. Unknown keys fail startup. Platform-wide policies (tenant purge grace, audit and usage retention, limits for just-in-time tenants, heartbeat targets) live in the same file in the MVP; an API for them comes only if PlusClouds needs to change them at runtime. Everything tenant-facing (devices, checks, retention) lives in the database and is managed through the API. The annotated reference is `deploy/config.example.yaml`, and `deploy/probe.example.yaml` for probes.
- **Lint and test:** `golangci-lint` with a committed config; `go test -race`; integration tests with `testcontainers-go` against real PostgreSQL (plain and TimescaleDB images); protocol simulators for SNMP (snmpsim), Redfish (DMTF Redfish mockup server) and MQTT (Mosquitto) in CI.
- **CI:** GitHub Actions — lint, unit, integration, license check ([ADR-0010](0010-dependency-license-policy.md)), OpenAPI lint and breaking-change diff, `govulncheck`.

### Repository layout

```text
cmd/monitor/            main package: serve, probe, migrate, admin commands
api/                    OpenAPI spec (source of truth) and bundling config
proto/probe/v1/         probe protocol definitions
pkg/plugin/             public plugin SDK: Check, Collector, Ingester, Target, Result
internal/
  app/                  role wiring, config loading, lifecycle
  api/                  HTTP handlers (implement generated interfaces), middleware
  auth/                 API keys, roles, tenant context
  audit/
  store/                config/state/incident repositories (pgx)
  metrics/              metrics.Store interface and backends: postgres, timescale
  scheduler/            shard leases, timing wheel, jitter
  runner/               executes plugins, concurrency limits, result sink
  engine/               state machine, incidents, dependencies, maintenance windows
  notifier/             outbox dispatcher, webhook signing
  ingest/               HTTP push, MQTT, (later) traps and syslog
  probe/                probe client and core-side probe service
  crypto/               envelope encryption, key providers, probe CA
  maintenance/          partitions, rollups, retention
plugins/                built-in plugins, one package each (icmp, tcp, http, tls, dns, snmp, redfish, ipmi, rtsp, xapi, ...)
migrations/             SQL migrations
deploy/                 Dockerfile, docker-compose for local dev, example configs
docs/                   design, feature specs, ADRs
```

### Conventions

- All timestamps are UTC `timestamptz` in the database and RFC 3339 in the API.
- Feature work starts from a spec in `docs/features/`; infrastructure choices get an ADR.
- Conventional Commits for commit messages; changelog generated at release.

## Consequences

- Everything in `internal/` can change freely; `pkg/plugin`, the OpenAPI spec and the probe protocol are the compatibility surfaces.
- Committing generated code keeps builds simple but requires a CI check that generation is up to date.
