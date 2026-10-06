# Implementation progress

**Last updated:** 2026-10-05 · **Latest release tag:** `v0.4.3`

This page records which milestones are done and what comes next. The milestone definitions (contents and demo) live in the [feature specs index](features/README.md#suggested-milestones); update this page when a milestone's status changes.

## Milestones

| Milestone | Features | Status | Notes |
| --- | --- | --- | --- |
| M1 — skeleton | Repo layout, CI, migrations, F01, F03, `http` and `icmp` plugins | **Done** | Merged in PR #2; released as `v0.1.0-m1` to `v0.1.3-m1` |
| M2 — first loop | F02, F04, F05, F06 (single node) | **Done** | Merged in PRs #3 and #4; released as `v0.2.0-m2`, client fixes in `v0.2.1`. Deferred items below |
| M3 — metrics | F07, Grafana data source and first dashboards | **Done** | Merged in PR #6; released as `v0.3.0-m3`, client decisions in `v0.3.1`. Deferred items below |
| M3.5 — usage metering | F13: check periods, hourly close, `GET /v1/usage/tenants` for PlusClouds billing (pull only) | **Done** | Released as `v0.3.5-m3.5`. Counting rules await PlusClouds' confirmation |
| M4 — alert noise and targets | F06 grouping and repeat notifications, F05 dependency suppression, then F10: SNMP, Redfish/IPMI, RTSP, XCP-ng, UPS/PDU | **In progress**: alert noise done (`v0.4.0-m4a`), webhooks completed (`v0.4.1`), SNMP checks built; multi-object collectors (interfaces, PDU, sensors, Redfish, XCP-ng) and RTSP next |
| M5 — push and probes | F08 (embedded MQTT broker, [ADR-0013](adr/0013-embedded-mqtt-broker.md)), F09 basic, F11 | Not started | |
| M6 — FixLean shadow | F12 steps 0–2 | Not started | Depends on M5 |
| Gate 1 | Load test at 10,000 simulated devices, security review | Not started | Live on our own datacenter |

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

### M3 — metrics (F07)

| Area | What exists |
| --- | --- |
| Storage | Raw samples are stored grouped: one row per result and retention class, with a `float8[]` of values (NaN means not collected). Rollups are narrow: one row per series and bucket, with `min`, `max`, `sum` and `count`. Both are list-partitioned by retention class, then range-partitioned: raw and 5-minute rollups by day, hourly rollups by month. Migration `00008` |
| Write path | The engine hands every result, late results included, to a non-blocking writer. The writer batches rows and writes them with `COPY` (5,000 rows or 1 s). Group and series IDs are cached. A row with no partition creates the partition and is retried. When the database fails, rows are kept for up to `max_buffered`; older ones are dropped and counted in `metrics_dropped_total` |
| Layouts and classes | A plugin layout change (name, unit or kind) starts a new group with a new `layout_version`, so old rows are never reinterpreted. Each metric goes to its plugin's retention class if the tenant's `metric_classes` allows it; otherwise to `standard`; otherwise to the tenant's first class |
| Maintenance role | One active node (advisory lock). Jobs:<br>• create partitions ahead<br>• 5-minute rollups every minute<br>• hourly rollups every 10 minutes, from the 5-minute rollups<br>• retention every hour<br>Rollups use watermarks and resume after downtime in 6-hour chunks. Each run recomputes one bucket before the watermark. Retention drops whole partitions through `SECURITY DEFINER` functions. Self-metrics: errors, partitions created and dropped, rollup lag |
| Retention | Classes `high-frequency`, `standard` and `capacity`, with no built-in durations. Set through `PUT /v1/metrics/retention-classes/{name}` (platform key) or `monitor admin retention` (standalone installs). New classes, such as `standard-1y`, are created the same way |
| Query API | `GET /v1/metrics/series` and `GET /v1/metrics/query`. The query picks raw, 5-minute or hourly data from `step`, and moves to a coarser level when retention no longer covers `from`. Recent buckets the rollups have not reached are filled from the finer level. Series are merged across layout changes. At most 10,000 points per series and 200 series per request |
| Grafana | Views `metrics_raw`, `metrics_5m`, `metrics_1h`, `metric_series_v`, `devices_v`, `check_states_v` and `incidents_v`, readable by the optional read-only role `monitor_grafana` (platform operator only; it sees every tenant). `deploy/grafana` holds the provisioning and two dashboards, *Device overview* and *Website checks*. The development compose has a `grafana` profile |

Verified with integration tests (`TestMetricsStoreAndQuery`, `TestRetention`, and `TestM2Demo`, which now also checks the stored metrics) and by hand in the Docker stack: live HTTP check, API query, 5-minute rollup, every dashboard query through Grafana 13.0.2, and the `admin retention` command.

### Deferred from M3

| Item | Spec | Why it waits |
| --- | --- | --- |
| TimescaleDB and ClickHouse backends | ADR-0003 | `metrics.backend: timescale` is refused at start; `auto` uses plain PostgreSQL |
| Load test: 7,000 values/s for 24 h, disk growth against the estimate | F07, ADR-0003 | Planned for Gate 1, along with the 10,000-device run |
| Recomputing rollups for data more than one bucket late | ADR-0003 | Only probe buffers (M5) produce such data |
| Removing metric groups of deleted checks | F07 | Their samples expire with retention; the group and series rows stay |
| Per-check choice of retention class | F07 | Classes come from the plugin and the tenant's allowed classes |
| Tenant-facing Grafana | F07 | Customers graph through the API; the Grafana views show every tenant |
| Dashboards for interfaces, server hardware and XCP-ng | F07 | Need the M4 plugins |
| Removing retention classes | F07 | Not needed yet; a class with no policy keeps its data |

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

### M4, part 1 — alert noise (F05, F06)

| Area | What exists |
| --- | --- |
| Dependency suppression | Incidents explained by an open upstream host-check incident open as `suppressed`, with `root_incident_id` and `root_device_id`. Upstream means the device's own host check, its containers and its dependencies, transitively. Notifications of incidents with anything upstream wait `notifier.dependency_grace` (30 s) and are checked again. Resolving a root releases its incidents, host checks first: they re-link to another open root or are announced with `monitoring.incident.opened`. Migration `00011` |
| Grouping | Routes take `group_by` and `group_wait_seconds`. A group of one is the plain event; a larger group is one event with `data.objects` and `data.group` |
| Repeats | `repeat_interval_seconds` re-sends open, unacknowledged incidents to that route as `monitoring.incident.renotify` |

Verified by `TestDependencySuppression` (switch and server, release under the server's own ping), `TestRouteGrouping` and `TestRepeatInterval`.

Not built yet: a per-check dependency grace, and suppressing incidents already notified when a root opens later.

### Webhooks completed (after M4 part 1)

Escalation steps with schedules (`monitoring.incident.escalated`), `POST /v1/alert-routes/test`, bulk replay of an endpoint's failed deliveries, `data.links.incident` from `notifier.incident_url_template`, `data.check.thresholds`, and the `suppressed` filter on incidents. The release also adds:
- tenant limits `max_webhooks` (20) and `max_alert_routes` (50);
- a check of the network policy when a webhook URL is saved;
- `notifier.delivery_retention` (30 days) for finished deliveries and routed events.

Migrations `00012` and `00013`. Verified by `TestEscalationSteps`, `TestAlertRouteTest`, `TestBulkReplay`, `TestScheduleNext`, `TestWebhookLimits`, `TestWebhookURLPolicy` and `TestDeliveryRetention`. F06 is complete except the tenant device-limit and heartbeat events (heartbeat comes with F11 in M5).

### M4, part 2a — SNMP checks (F10)

| Plugin | What it does |
| --- | --- |
| `snmp.system` | Uptime, CPU and memory from vendor profiles (HOST-RESOURCES, Net-SNMP, Cisco, Juniper, MikroTik, Fortinet, HPE), picked from `sysObjectID`, with fallback to HOST-RESOURCES. Reports restarts |
| `snmp.get` | One OID as a gauge or a counter rate, with scale and an optional expected value |
| `snmp.ups` | UPS-MIB (RFC 1628) and APC PowerNet: charge, runtime, load, voltages, on battery, battery replacement |

Shared: SNMPv3 (`authPriv`, SHA-256/AES by default) and v2c credentials, the network policy checked on the resolved address, and counter rates with 32-bit wrap, restart and glitch handling. Verified against snmpsim fixtures over v2c and v3.

Not built yet: the plugins that report many objects per run (`snmp.interfaces`, `snmp.pdu`, `snmp.sensor`, `redfish.health`, `xapi.*`) need collector support in the runner, engine and metrics writer; the restart inventory event comes with it.

### v0.4.3 — replacing lost keys

`monitor admin rotate-platform-key [--keep-old]`, `monitor admin revoke-platform-keys --keep ID` and `monitor admin create-admin-key --tenant ID`. Bootstrap prints keys once and stores only their hashes, so a lost key is replaced, not recovered. Each command writes an audit event. Verified by `TestAdminKeyCommands`.

### M3.5 — usage metering (F13)

| Area | What exists |
| --- | --- |
| Periods | `check_periods` and `device_periods` are kept by triggers on `checks`, `devices` and `tenants`. A check is billable while it is enabled and its tenant is active. The platform tenant is never billed. Deletes, interval changes, suspend, resume, delete and restore all close or open periods in the same transaction. Counting starts at the deploy of migration `00010` |
| Weights | `usage.weights` and `default_weight` in the config. The maintenance node records changes, effective from the next full hour, with an audit event (actor `file`). A plugin's first weight applies to all time |
| Hourly close | The maintenance node closes each UTC hour `platform.usage_close_delay` (5 min) after it ends, into `usage_hours` and `usage_hour_plugins`, catching up missed hours in order. Closed hours are never rewritten; `monitor admin usage-recompute` writes a new revision. Rows past `platform.usage_retention` are deleted |
| API | `GET /v1/usage/tenants` (platform key; the billing contract), `GET /v1/usage/hours` (the tenant's own hours), `GET /v1/usage/current` and `GET /v1/usage/weights`. Open hours return 422 `usage-hour-open` |

Verified by `TestUsagePeriods` and `TestUsageHours`, and in the Docker stack: weights recorded at start, `current` and `weights` served, migration applied.

Not built: `runs_on` (remote probes, M5), the manifest's `Billable` flag (no companion plugin yet; a config weight of 0 does the same) and partitioned usage tables (retention deletes rows).

### v0.3.1 — decisions with the PlusClouds client (2026-10-04)

| Change | Why |
| --- | --- |
| `POST /v1/tenants/by-external-id/{id}/restore` undeletes a tenant within `platform.tenant_purge_grace` (default 30 days). After the grace the maintenance role purges it: all data except the audit log is removed, the tenant row stays as a `purged` tombstone, and the external ID is free for a new tenant. Migration `00009` | Returning customers, and deleted tenants no longer keep data forever |
| The `operator` role configures what is monitored: sites, devices, checks, credentials, webhooks and alert routes. `admin` keeps API keys, members and the audit log | PlusClouds reserves `admin` for the service owner; customers are read-only or operator and must configure their own monitoring |
| Alert routes match `check_ids` and `device_ids` | Customers send one alarm, or one host, to its own webhook |
| `PATCH` (JSON Merge Patch) for sites, by ID and by external ID | Same as devices and checks |
| The stale sweep skips suspended tenants | Their checks stop on purpose; their state now stays as it was instead of turning UNKNOWN |

Decided, no code: customers own their webhooks and see the server-generated secret once (no client-supplied or platform-wide secret). PlusClouds sets limits (10 devices and 10 checks for now) at provisioning and disables extra checks itself on a downgrade. Private targets wait for remote probes (M5); no operator deny-list changes for PlusClouds' internal ranges for now. Bulk device sync is not needed yet.

## Verification

- `scripts/ci.sh lint`, `scripts/ci.sh test` and `scripts/ci.sh security` pass on `feat/m2-first-loop` as of 2026-10-04.
- Database integration tests start PostgreSQL with testcontainers. Without Docker they are skipped, unless `MONITOR_REQUIRE_DB=1` is set.
- The Go toolchain on a developer machine must match `go.mod` (Go 1.27). Otherwise use `scripts/ci.sh`, which needs only Docker.

## Next steps

1. Deploy `v0.4.1` or later and set retention on the live server: `monitor admin retention standard --raw-days 7 --rollup-5m-days 90 --rollup-1h-days 730`, and the same for `high-frequency` and `capacity`. Until a policy is set, data is kept forever.
2. M4: collector support (per-object metrics and state), then `snmp.interfaces`, `snmp.pdu`, `snmp.sensor`, `redfish.health`, `rtsp.stream` and the XCP-ng collectors, with their dashboards.
3. Later (requested 2026-10-05): replication of devices and hosts between several monitoring servers. Recorded in the [feature index](features/README.md#phase-2-and-later); spec and milestone to be decided.
