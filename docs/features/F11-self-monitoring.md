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

### Dead man's switch

The engine cannot report its own death, so it sends a heartbeat that something external expects:

- A configured list of heartbeat URLs (for example healthchecks.io, a second monitor instance, or an n8n webhook with its own timeout logic) receives a signed `monitor.heartbeat` event every 60 s.
- The heartbeat is sent only if the core loop is healthy: the engine processed results in the last minute, the notifier delivered or had nothing to deliver, and the database is reachable. A process that is up but stuck therefore stops heartbeating.
- For HA installs, the heartbeat is sent by whichever node holds the `maintenance` lock, and includes the node count.

### Health endpoints

- `GET /healthz` — process is alive (for container restarts).
- `GET /readyz` — database reachable, migrations applied, role-specific readiness (runner owns its shards, notifier connected). Used by load balancers.

## Acceptance criteria

- Stopping PostgreSQL stops heartbeats within 2 minutes.
- Pausing the notifier (simulated hang) stops heartbeats within 2 minutes while `/healthz` still returns 200.
- A synthetic overload (runner limit set to 10) raises the check-lag alert.

## Open questions

- Where should the PlusClouds production heartbeat go? Proposed: an independent second monitor instance in a different datacenter watching the first, plus a hosted dead man's switch service.
