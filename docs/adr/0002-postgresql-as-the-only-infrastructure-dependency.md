# ADR-0002: PostgreSQL is the only required infrastructure dependency

**Status:** Proposed · **Date:** 2026-09-27

## Context

Several parts of the system need coordination: which node schedules which checks, which node delivers which webhook, which node runs rollups, and how the API tells live clients about state changes. The common answer is a broker (NATS, Kafka, RabbitMQ) or Redis. Every extra dependency makes the engine harder to run for customers and in the PlusClouds panel, and "simple to run" is a design principle.

Load at the design target: about 6,700 metric values per second and roughly 1,000 check executions per second. State *changes*, incidents and webhook events are orders of magnitude rarer.

## Options considered

1. **PostgreSQL only.** Coordination via lease rows, `SELECT … FOR UPDATE SKIP LOCKED`, advisory locks and `LISTEN/NOTIFY`.
2. **PostgreSQL + NATS JetStream.** A good fit for result streaming and fan-out, but one more stateful service to run, secure, back up and monitor.
3. **PostgreSQL + Redis.** Fast leases and pub/sub, but Redis licensing has changed in recent years and it adds a second stateful service.

## Decision

Option 1 for the MVP and phase 2. PostgreSQL provides:

| Need | Mechanism |
| --- | --- |
| Runner shard ownership | `scheduler_shards` table with `owner_node`, `lease_until`; nodes renew every few seconds and take over expired leases |
| Singleton jobs (maintenance, rollups) | `pg_try_advisory_lock` on a fixed key |
| Webhook delivery queue | Outbox table polled with `FOR UPDATE SKIP LOCKED` ([ADR-0009](0009-webhook-delivery.md)) |
| Live updates to API nodes (SSE) | `LISTEN/NOTIFY` on a per-tenant channel; payload carries only IDs, clients fetch details |
| Config change propagation to runners | `NOTIFY config_changed` plus a periodic full resync, so a missed notification only delays a change |
| Schema migrations | Run at startup under an advisory lock |

The metrics store sits behind an interface ([ADR-0003](0003-metrics-storage-layout.md)), so it can move to TimescaleDB or ClickHouse without affecting coordination.

## Consequences

- One stateful service to operate, back up and secure. Managed PostgreSQL works.
- `LISTEN/NOTIFY` does not work through transaction-mode PgBouncer; nodes hold one dedicated session connection for it.
- Result traffic never goes through PostgreSQL as a queue: results stay in process between runner and engine.
- Revisit if runner and engine must be split into separate processes, or if a load test shows PostgreSQL coordination as the bottleneck. NATS is the first candidate then.
