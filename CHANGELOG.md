# Changelog

Notable changes per release. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions are git tags. The project is pre-1.0: the API can change between minor versions. Per-milestone detail is in [docs/progress.md](docs/progress.md).

## [Unreleased]
### Added
- Monitoring agent (`cmd/agent`, F15): one static binary to copy into any Docker image (`COPY --from=registry.plusclouds.com/monitoring-agent /agent ...`, `ENTRYPOINT ["agent","run","--", ...]`) or run as a sidecar. Collectors: host/container (cgroup-aware), Docker engine, tcp/http/process probes, redis, postgres, nginx, php-fpm, elasticsearch, node.js. See `docs/agent.md` and ADR-0016.
- `host.agent` ingester plugin and push token for it (weight 2 in usage metering, to be confirmed); unique `agent_id` per server (migration 00026).
- `Dockerfile.agent`, `make build-agent`, CI builds and pushes `monitoring-agent` with every version tag.
- Grafana dashboard "Agents: hosts, containers and services".
- `elasticsearch.cluster` collector: cluster health and shards, per-node heap, CPU, disk, old-generation GC and rejected requests, and the largest indices (documents, size, health) as objects. Optional `http_basic` or `http_bearer` credential. Each node bills as `elasticsearch.cluster:node` (weight 3, TBD with billing).
- Grafana dashboard "Elasticsearch clusters".

## [0.14.1] - 2026-10-12
### Fixed
- Lint failure (unnecessary conversion) in device notifications.

## [0.14.0] - 2026-10-12
### Added
- Notification catalog and per-device notifications.

## [0.13.x] - 2026-10-10 to 2026-10-11
### Added
- `box.agent` heartbeats (0.13.0), every 5 s in every tenant (0.13.1, 0.13.3).
### Fixed
- Whoopsy relearn behavior (0.13.2).

## [0.12.x] - 2026-10-07
### Added
- PlusClouds VM agent telemetry (`vm.agent`) and its weight in usage metering.

## [0.11.0] - 2026-10-07
### Added
- Self-monitoring (`monitor.self`) and heartbeat.

## [0.10.0] - 2026-10-07
### Added
- Embedded MQTT broker and `push.mqtt` ([ADR-0013](docs/adr/0013-embedded-mqtt-broker.md)).

## [0.9.0] - 2026-10-07
### Added
- HTTP push ingest (`push.http`).

## [0.4.0 to 0.8.4] - 2026-10-04 to 2026-10-07
### Added
- Alert noise control: grouping, repeat notifications, escalation steps, dependency suppression; webhook completeness (replay, test send, limits, retention).
- Plugins: `snmp.*` (system, get, interfaces, ups, pdu, sensor), `redfish.health`, `xapi.pool` (XCP-ng with discovered devices), `camera.snapshot`, `rtsp.stream`.
- Whoopsy adaptive alerts.
- Grafana provisioning and dashboards (devices, websites, network, servers, power, cameras, XCP-ng).
- `admin rotate-platform-key`, `admin revoke-platform-keys`.

## [0.3.x] - 2026-10-04
### Added
- Metrics storage and Grafana data source (F07); usage metering for billing (`GET /v1/usage/tenants`, 0.3.5).

## [0.2.x] - 2026-10-04
### Added
- Scheduler/runner, state machine, incidents, signed Standard Webhooks delivery (M2).
- Device `status`, JSON Merge Patch on devices and checks, lookup by external ID (0.2.1).

## [0.1.x] - 2026-09-29 to 2026-10-04
First release (`v0.1.0-m1`, 2026-09-29).
### Added
- `monitor` CLI, config, migrations, tenants, users, API keys, hash-chained audit log, plugin SDK, `icmp` and `http` plugins, OpenAPI at `/docs`, Docker image and Compose deployment.
