# F04: Scheduler and runner

**Status:** Draft · **Phase:** MVP (single node), phase 2 (multi-node shard leases proven under failure) · **Related:** [ADR-0001](../adr/0001-single-binary-with-roles.md), [ADR-0002](../adr/0002-postgresql-as-the-only-infrastructure-dependency.md)

## Summary

The runner decides when each check runs and runs it. Scheduling and execution live in one component so there is no queue between them. Work is split into fixed shards that runner nodes lease from PostgreSQL. Remote probes run the same runner over their assigned checks.

## Load at the design target

10,000 devices with an average of 5 checks and a mix of intervals gives roughly 1,000 executions per second, with a small set of 5-second checks. Most execution time is waiting on the network, so a runner is I/O bound and uses goroutines, not processes.

## Behavior

### Shards

- 1,024 fixed virtual shards. A check's shard is `hash(check_id) mod 1024`, which never changes.
- Checks assigned to a probe are excluded from core shards; the probe's assignment set is its own shard.
- Each runner node leases shards in `scheduler_shards` (lease 15 s, renewed every 5 s). On startup or when a node disappears, remaining nodes take expired shards, aiming for an even split. A node that cannot renew its lease stops running those shards immediately (fencing by lease expiry and a lease epoch number written with every result).

### Timing

- **Deterministic jitter:** each check's phase is `hash(check_id) mod interval`. A 60 s check always runs at the same second of the minute, across restarts and nodes. This spreads load evenly and keeps graphs regular.
- **Timing wheel** in memory per node, driven by a monotonic clock. After a restart the node runs each check at its next phase, not immediately, to avoid a thundering herd.
- **Missed runs are skipped, not queued.** If a check is still running when its next run is due, the next run is skipped and counted (`runner_skipped_total`). A check that is regularly skipped shows as "overrunning" in its state.
- **Run now:** `POST /checks/{id}/run-now` sends a `NOTIFY` that the owning node picks up; the result goes through the normal pipeline.

### Execution

- Global worker limit per node (default 2,000 concurrent executions), per-plugin limit from the manifest, and a **per-target limit** (default 4 concurrent operations against one address) to protect switches and BMCs from being overloaded.
- Slow operations (SNMP table walks, collector runs) have a separate pool from fast checks (ping, TCP, HTTP), so one slow device cannot delay fast checks; this is the design's "fast polls separate from slow table walks".
- Timeout per check (default 80 % of interval, max 60 s for checks, 5 min for collectors).
- Credentials are decrypted just before the call and dropped afterwards.
- Results go to the engine through an in-process channel with a bounded buffer. If the buffer is full, the runner blocks rather than drops (backpressure shows as rising check lag).

### Config changes

- Runners load their shards' checks at lease time, then apply changes on `NOTIFY config_changed` with the check ID. A full resync runs every 5 minutes as a safety net.

## Self-metrics

`runner_check_lag_seconds` (actual start minus scheduled start, histogram), `runner_executions_total{plugin,status}`, `runner_skipped_total{plugin}`, `runner_inflight{pool}`, `runner_shards_owned`, `runner_poll_cycle_seconds{plugin}`.

## Acceptance criteria

- 10,000 simulated devices, ~1,000 executions/s on one node: p99 check lag under 1 s, CPU under 4 cores.
- Killing one of three runner nodes: its shards resume on the others within 20 s; no check runs on two nodes at once (verified by lease epoch in results).
- A device that never responds affects only its own checks: other checks' lag does not change.
- After restart, no burst: executions per second in the first minute stay within 20 % of steady state.

## Decided

- **5-second intervals** are allowed only for cheap checks (`icmp`, `tcp`, `http`). Every other plugin sets a higher `MinInterval` in its manifest (SNMP table walks and Redfish at least 60 s), and the tenant's `min_check_interval_seconds` applies on top. The API rejects intervals below either limit.
