# Implementation progress

**Last updated:** 2026-10-04 · **Latest release tag:** `v0.2.1`

This page records which milestones are done and what comes next. The milestone definitions (contents and demo) live in the [feature specs index](features/README.md#suggested-milestones); update this page when a milestone's status changes.

## Milestones

| Milestone | Features | Status | Notes |
| --- | --- | --- | --- |
| M1 — skeleton | Repo layout, CI, migrations, F01, F03, `http` and `icmp` plugins | **Done** | Merged in PR #2; released as `v0.1.0-m1` to `v0.1.3-m1` |
| M2 — first loop | F02, F04, F05, F06 (single node) | **Done** | Merged in PRs #3 and #4; released as `v0.2.0-m2`, client fixes in `v0.2.1`. Deferred items below |
| M3 — metrics | F07, Grafana data source and first dashboards | Not started | Next milestone |
| M4 — targets | F10: SNMP, Redfish/IPMI, RTSP, XCP-ng, UPS/PDU | Not started | |
| M5 — push and probes | F08 (embedded MQTT broker, [ADR-0013](adr/0013-embedded-mqtt-broker.md)), F09 basic, F11 | Not started | |
| M6 — FixLean shadow | F12 steps 0–2 | Not started | Depends on M5 |
| Gate 1 | Load test at 10,000 simulated devices, security review | Not started | Live on our own datacenter |
| Unassigned | F13 usage metering (MVP scope) | Not started | Needs a milestone; the runner now exists, so M3 is the earliest |

## Done

### M1 — skeleton (merged, PR #2)

- `monitor` CLI, process config loading and validation, status server.
- Database schema and migrations: installation, tenants, users, tenant memberships, API keys, audit events with a hash chain.
- REST API for F01, including the PlusClouds provisioning endpoints from [ADR-0012](adr/0012-plusclouds-identity-and-external-ids.md).
- Plugin SDK and registry (`pkg/plugin`), `GET /v1/plugins`, and the `http` and `icmp` plugins (F03).

### After M1 (merged in PR #3)

- API reference at `/docs`, OpenAPI spec at `/v1/openapi.*`, Postman collection.
- Docker image and Docker Compose deployments; `admin bootstrap --if-needed`.
- CI on the self-hosted runner via `scripts/ci.sh`; images pushed on version tags.
- OpenResty site config for monitoring.plusclouds.com.

### M2 — first loop (merged in PR #4, `v0.2.0-m2`)

| Feature | What exists |
| --- | --- |
| F02 | Sites, devices (containment via `parent_id`), dependency graph with cycle checks and `impact`, credentials with envelope encryption ([ADR-0006](adr/0006-credential-encryption.md)), checks validated against the plugin schema and tenant limits, `POST /devices/{id}/test`, upserts by external ID. Migration `00003` |
| F04 | Runner: fixed phase per check, skip-not-queue, fast/slow pools and per-target limit, one active runner with hot standbys (advisory lock), reload on `NOTIFY config_changed`, `run-now`. Migration `00004` |
| F05 | Pure state machine (failure/recovery counts, thresholds with hysteresis and `for`, flap detection, UNKNOWN handling), engine writing state, incidents and CloudEvents in one transaction, stale-result sweeper, incident API (list, get, acknowledge, comment, resolve). Migration `00005` |
| F06 | Webhook endpoints with encrypted signing secrets and rotation, ordered alert routes, router from events to deliveries, Standard Webhooks signing, retries with backoff, per-incident ordering, `410` disables the endpoint, SSRF policy at delivery, delivery log, replay, test send. Migration `00006` |

`monitor serve` now starts the `api`, `runner`, `engine` and `notifier` roles. The M2 demo runs as `TestM2Demo` in `internal/api`: a device with an HTTP check goes down, a signed `monitoring.incident.opened` webhook arrives and verifies with the endpoint's secret, and `monitoring.incident.resolved` follows on recovery. The same flow was checked by hand in the Docker stack.

### v0.2.1 — gaps found by the PlusClouds client

| Change | Why |
| --- | --- |
| Every device response has `status`: `availability` (up, down, unknown, unmonitored, disabled, from the host check), `since`, `health` (worst open incident), `open_incidents`; `GET /devices?availability=` filters on it | A host list needed one state call per check |
| `PATCH /devices/{id}`, `PATCH /devices/by-external-id/{id}` and `PATCH /checks/{id}` as JSON Merge Patch (RFC 7396) | `PUT` replaces the object, so partial updates needed read, merge and write, which races |
| Reading a tenant without writing it: `GET /tenants?external_source=plusclouds&external_id=…` (documented) | A dedicated `GET /tenants/by-external-id/{id}` conflicts with `GET /tenants/{tenant_id}/members` in the router |
| Idempotent creates: clients upsert with `PUT /devices/by-external-id/<their own record id>` | No server change |

Migration `00007` adds `check_state.availability_since`.

### Deferred from M2

| Item | Spec | Why it waits |
| --- | --- | --- |
| Route grouping (`group_by`, `group_wait`) and repeat notifications (`repeat_interval`) | F06 | Every event is delivered on its own for now; grouping changes the payload (`data.objects`) and needs its own tests |
| Templates and `POST /templates/{id}/apply` | F02 | Needs the M4 plugins to be useful |
| `POST /credentials/{id}/test` | F02 | Needs protocol plugins (SNMP, Redfish, XAPI) from M4; `POST /devices/{id}/test` covers HTTP and ping today |
| `Idempotency-Key` on `POST` and bulk device create | ADR-0007, F02 | Upserts by external ID already give idempotency for PlusClouds |
| Dependency suppression | F05 (phase 2) | The graph and `impact` exist; the engine does not suppress yet |
| Multi-node shard leases | F04 (phase 2) | MVP runs one active runner with standbys |
| Persisting plugin state across restarts | F03 | Kept in memory per check; a restart costs one interval of rate data |
| Postman collection for the new endpoints | — | The OpenAPI spec and `/docs` are current |

## Verification

- `scripts/ci.sh lint`, `scripts/ci.sh test` and `scripts/ci.sh security` pass on `feat/m2-first-loop` as of 2026-10-04.
- Database integration tests start PostgreSQL with testcontainers. Without Docker they are skipped, unless `MONITOR_REQUIRE_DB=1` is set.
- The Go toolchain on a developer machine must match `go.mod` (Go 1.27). Otherwise use `scripts/ci.sh`, which needs only Docker.

## Next steps

1. M3: F07 metrics store (grouped raw samples, rollups, retention by partition), Grafana views and first dashboards. The engine already has each result's metrics in hand.
2. Assign F13 (usage metering) to a milestone; check-hours can be counted from runner results.
