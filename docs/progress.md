# Implementation progress

**Last updated:** 2026-10-04 · **Latest release tag:** `v0.1.3-m1`

This page records which milestones are done and what comes next. The milestone definitions (contents and demo) live in the [feature specs index](features/README.md#suggested-milestones); update this page when a milestone's status changes.

## Milestones

| Milestone | Features | Status | Notes |
| --- | --- | --- | --- |
| M1 — skeleton | Repo layout, CI, migrations, F01, F03, `http` and `icmp` plugins | **Done** | Merged in PR #2; released as `v0.1.0-m1` to `v0.1.3-m1` |
| M2 — first loop | F02, F04, F05, F06 (single node) | Not started | Next milestone |
| M3 — metrics | F07, Grafana data source and first dashboards | Not started | |
| M4 — targets | F10: SNMP, Redfish/IPMI, RTSP, XCP-ng, UPS/PDU | Not started | |
| M5 — push and probes | F08 (embedded MQTT broker, [ADR-0013](adr/0013-embedded-mqtt-broker.md)), F09 basic, F11 | Not started | |
| M6 — FixLean shadow | F12 steps 0–2 | Not started | Depends on M5 |
| Gate 1 | Load test at 10,000 simulated devices, security review | Not started | Live on our own datacenter |
| Unassigned | F13 usage metering (MVP scope) | Not started | Needs a milestone; it depends on F04, so M2 or M3 at the earliest |

## Done

### M1 — skeleton (merged, PR #2)

- `monitor` CLI, process config loading and validation, status server.
- Database schema and migrations (`migrations/00001_tenancy.sql`, `00002_platform_api.sql`): installation, tenants, users, tenant memberships, API keys, audit events with a hash chain.
- REST API for F01: `/v1/me`, `/v1/tenant`, `/v1/api-keys`, `/v1/audit`, and the PlusClouds provisioning endpoints (`/v1/tenants…`, members and users by external ID) from [ADR-0012](adr/0012-plusclouds-identity-and-external-ids.md), including atomic just-in-time users and memberships.
- Plugin SDK and registry (`pkg/plugin`), `GET /v1/plugins`, and the `http` and `icmp` plugins (F03).
- Admin bootstrap command.

### After M1, on branch `feat/api-docs` (pushed, not merged)

- API reference served at `/docs`; OpenAPI spec at `/v1/openapi.*`.
- Postman collection and local environment (`api/postman/`).
- Docker image and Docker Compose deployment from the published image, with the development database on a private network.
- `admin bootstrap --if-needed` for automated deployments; database roles created on every start.
- CI on the self-hosted runner via `scripts/ci.sh`; images pushed on version tags and tagged `latest`.
- OpenResty site config for monitoring.plusclouds.com.
- Fix: a raised rate limit applies at once.

## Verification

- `scripts/ci.sh test` runs `go test -race ./...` and the build inside `golang:1.27`; it passes on `feat/api-docs` as of 2026-10-04.
- Database integration tests start PostgreSQL with testcontainers. Without Docker they are skipped, unless `MONITOR_REQUIRE_DB=1` is set.
- The Go toolchain on a developer machine must match `go.mod` (Go 1.27). Otherwise use `scripts/ci.sh`, which needs only Docker.

## Next steps

1. Open a pull request for `feat/api-docs` and merge it into `master`.
2. M2, in dependency order:
   1. F02: migrations and API for sites, devices, credentials and templates. Credentials use envelope encryption ([ADR-0006](adr/0006-credential-encryption.md)). Follow the device model in [ADR-0014](adr/0014-device-model-containment-dependencies-sites.md).
   2. F04: scheduler and runner on a single node.
   3. F05: state machine, thresholds and incidents.
   4. F06: outbox, webhook delivery and alert routes.
3. Assign F13 (usage metering) to a milestone.

## Changes to the docs since the first draft

ADR-0014 (device model, containment, dependencies, sites), ADR-0015 (tenant configuration source), F13 (usage metering), F14 (LLM monitoring, phases 2 and 3) and [docs/compliance](compliance/README.md) were added after the initial design set.
