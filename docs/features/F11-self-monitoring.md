# F11: Monitoring the monitor

**Status:** Draft · **Phase:** MVP · **Related:** design section 9

## Summary

If the engine dies, nothing alerts. The engine exposes its own health, alerts on its own degradation through the normal pipeline where it can, and proves it is alive to something outside itself where it cannot.

## Behavior

### Self-metrics

Prometheus exposition at `GET /metrics` on an internal listener (separate port, no tenant data). Key series:

| Area | Metrics |
| --- | --- |
| Runner | check lag histogram, executions by plugin and status, skipped runs, in-flight by pool, shards owned, poll-cycle duration |
| Engine | results/s, state write latency, result channel depth, incidents opened/resolved |
| Metrics store | flush latency, rows written, rows dropped, buffer depth, rollup lag, partitions ahead |
| Notifier | outbox depth, oldest pending event age, deliveries by status, retries |
| Ingest | messages/s by source, unmatched messages, auth failures |
| Probes | connected probes, silent probes, buffer depth per probe, clock offset |
| API | requests by route and status, latency, rate-limited requests |
| Process | Go runtime, database pool usage, build info |

The same values are also stored as metrics of a built-in "monitor" device in the platform tenant, so Grafana and the API show them without running Prometheus.

### Internal alerts

Built-in checks on the "monitor" device, alerting through normal routes:

- Check lag p99 above 5 s for 5 minutes.
- Outbox oldest event older than 5 minutes (notifications are stuck).
- Rows dropped by the metrics store in the last 5 minutes.
- Partitions for tomorrow not created.
- Rollup lag above 30 minutes.
- Probe silent (from [F09](F09-remote-probes.md)).
- Webhook endpoint failing for more than 1 hour.
- Core node clock offset against the database clock above 2 s (probes already report theirs).

### Security alerts

Built-in checks on the "monitor" device for security signals, alerting through the same routes:

- Failed API authentication above a rate per source IP or per key prefix.
- Rate-limited requests above a rate per key.
- MQTT authentication failures and ACL denials above a rate per source IP or credential ([ADR-0013](../adr/0013-embedded-mqtt-broker.md)).
- Platform key used, or attempted, from a source outside its IP allowlist ([ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)).
- A revoked probe certificate presented again ([ADR-0005](../adr/0005-probe-protocol.md)).
- Audit chain verification failure ([F01](F01-tenancy-auth-audit.md)), from a daily `verify-audit` run by the `maintenance` role.

### Dead man's switch

The engine cannot report its own death, so it sends a heartbeat that something external expects:

- A configured list of heartbeat URLs (for example healthchecks.io, a second monitor instance, or an n8n webhook with its own timeout logic) receives a signed `monitoring.heartbeat` event every 60 s.
- The heartbeat is sent only if the core loop is healthy: the engine processed results in the last minute, the notifier delivered or had nothing to deliver, and the database is reachable. A process that is up but stuck therefore stops heartbeating.
- For HA installs, the heartbeat is sent by whichever node holds the `maintenance` lock, and includes the node count.

### Health endpoints

- `GET /healthz` — process is alive (for container restarts).
- `GET /readyz` — database reachable, migrations applied, role-specific readiness (runner owns its shards, notifier connected). Used by load balancers.
- `GET /status` — this node's view for an operator: roles, shards owned, pool usage, buffer and outbox depth, database pool, recent errors. JSON, or an HTML page in a browser. Same shape as the probe's status server ([F09](F09-remote-probes.md)).

`/healthz` and `/readyz` are open. `/status` and `/metrics` require a status token from the process config; on a loopback address the token may be left out.

## As built (M5, part 3: `v0.11.0`)

- **monitor.self.** The engine process ensures a `monitor` device (`managed_by: self-monitoring`, type `server`) in the platform tenant with an `engine health` check (plugin `monitor.self`, host check, weight 0) and reports it every minute through the engine. Metrics: `check_lag_p99_seconds` (from the runner's lag histogram over 5 minutes), `results_per_minute`, `outbox_oldest_seconds` and `outbox_depth` (pending deliveries), `metrics_dropped_5m`, `rollup_lag_seconds` (5-minute rollup watermark), `partitions_missing` (classes without tomorrow's raw partition), `webhook_endpoints_failing` (undelivered for over an hour without a success since), `clock_offset_seconds` (node against `clock_timestamp()`), `api_auth_failures_5m`, `api_rate_limited_5m`, `mqtt_auth_failures_5m`, `mqtt_connections`. Database values are installation-wide; the others come from this process's registry, so in a split deployment they cover the engine node.
- **Built-in alerts** are the check's default thresholds: lag p99 > 5 s for 5 minutes (warning), oldest outbox event > 300 s (critical), dropped rows > 0, rollup lag > 1800 s, missing partitions > 0 (critical), failing webhook endpoints > 0, clock offset outside ±2 s, more than 100 API or MQTT authentication failures or 500 rate-limited requests in 5 minutes (warnings). They can be tuned like any check's thresholds; a deleted check is recreated with the defaults. Alerts go to the platform tenant's routes.
- **Heartbeat.** A job of the maintenance lock holder, every `heartbeat.interval` (60 s), to each target: `POST` of `{"id", "type": "monitoring.heartbeat", "timestamp", "data": {"node_id", "version", "last_self_result_at"}}` with `webhook-id`, `webhook-timestamp`, `webhook-signature` (with `secret_file`) and `Authorization: Bearer` (with `token_file`). Withheld (logged, counted in `selfmon_heartbeats_total{outcome="withheld"}`) unless the database answers, every loop that reports liveness in this process turned within 2 minutes (the notifier), and the engine applied its own `monitor.self` result within 3 minutes. Stopping PostgreSQL or the engine therefore stops it within about 3 minutes.
- **Tests.** `TestSelfMonitoring` (device and check, measurements, an alert, recreation, a signed heartbeat, withheld when unhealthy), quantile and liveness unit tests.
- **Not yet:** probe metrics (with F09), the security signals that need new sources (platform key outside its allowlist, revoked probe certificates, audit chain verification), and per-node devices for HA installs.

## Acceptance criteria

- Stopping PostgreSQL stops heartbeats within 2 minutes.
- Pausing the notifier (simulated hang) stops heartbeats within 2 minutes while `/healthz` still returns 200.
- A synthetic overload (runner limit set to 10) raises the check-lag alert.

## Decided

- **Production heartbeat:** PlusClouds' production engine sends its heartbeat to an independent second engine in a different datacenter, which watches the first, and to a hosted dead man's switch service. Either one alerting is enough to wake someone up.
