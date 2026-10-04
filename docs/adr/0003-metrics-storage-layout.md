# ADR-0003: Metrics storage layout in plain PostgreSQL

**Status:** Accepted · **Date:** 2026-09-27 · **Implemented:** M3, 2026-10-04

## Context

The default metrics backend is plain PostgreSQL with engine-managed partitions and rollups; retention is set through the API per metric class and rollup level. Grafana reads the tables directly.

Baseline load: 10,000 devices × 20 metrics every 30 s = **200,000 series** and **~6,700 values/s**, or **~576 million values/day**. The layout decides whether plain PostgreSQL is viable at all.

### Size estimate per layout (raw data, per day)

| Layout | Rows/day | Approx. bytes/row incl. overhead | Heap/day | Index/day | Total/day |
| --- | --- | --- | --- | --- | --- |
| **Narrow:** one row per value `(series_id, ts, value)` | 576 M | ~52 | ~30 GB | ~16 GB | **~46 GB** |
| **Grouped:** one row per check result `(group_id, ts, values float8[])`, 20 values | 29 M | ~230 | ~6.7 GB | ~0.8 GB | **~7.5 GB** |

Estimates use PostgreSQL's 24-byte tuple header, 4-byte line pointer and 24-byte one-dimensional array header, before `fillfactor` and bloat. They must be confirmed by the load test.

## Options considered

1. **Narrow rows for everything.** Simplest SQL for Grafana, but ~46 GB/day of raw data makes plain PostgreSQL unattractive at the design target.
2. **Grouped raw rows, narrow rollups.** A check or collector result already produces a fixed set of metrics at one timestamp, so storing it as one row with an array of values is ~6× smaller. Rollups are far fewer rows and stay narrow for easy querying.
3. **Require TimescaleDB.** Solves size with compression, but conflicts with the decision that no backend is required that some users cannot run.

## Decision

Option 2.

```sql
-- One per (source check/collector, object) producing a fixed metric layout.
CREATE TABLE metric_groups (
    id               bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id        uuid     NOT NULL,
    retention_class  smallint NOT NULL,
    layout_version   int      NOT NULL,      -- bumps when slots change
    device_id        uuid     NOT NULL,
    source_id        uuid     NOT NULL,      -- check or collector
    object_key       text     NOT NULL       -- '' or interface index, VM UUID, ...
);

-- One per distinct metric; labels identify it. Tenant is repeated for isolation.
CREATE TABLE metric_series (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id    uuid     NOT NULL,
    group_id     bigint   NOT NULL REFERENCES metric_groups(id),
    slot         smallint NOT NULL,          -- index into metric_samples.values
    name         text     NOT NULL,          -- e.g. 'if_in_octets_rate'
    unit         text,
    labels       jsonb    NOT NULL,          -- device, check, interface, vm_uuid, ...
    UNIQUE (group_id, slot)
);

-- Raw samples: list-partitioned by retention class, sub-partitioned by day.
CREATE TABLE metric_samples (
    retention_class  smallint    NOT NULL,
    group_id         bigint      NOT NULL,
    ts               timestamptz NOT NULL,
    vals             float8[]    NOT NULL    -- NaN = not collected this run ("values" is reserved)
) PARTITION BY LIST (retention_class);
-- e.g. metric_samples_c1 PARTITION OF metric_samples FOR VALUES IN (1)
--      PARTITION BY RANGE (ts); then one child per day.

-- Rollups: narrow, one row per series per bucket, same partitioning scheme.
CREATE TABLE metric_rollup_5m (
    retention_class  smallint    NOT NULL,
    series_id        bigint      NOT NULL,
    ts               timestamptz NOT NULL,
    min float8, max float8, sum float8, count int
) PARTITION BY LIST (retention_class);
-- metric_rollup_1h has the same shape.
```

Rules:

- **Partitioning follows retention.** Retention is set per metric class, and dropping whole partitions is the only cheap delete. Raw samples and rollups are therefore partitioned first by retention class (list), then by day (range). Retention runs as `DROP TABLE` on expired partitions, never `DELETE`.
- **Partitions are created ahead.** The `maintenance` role keeps partitions for the next 3 days and fails loudly (self-metric plus alert) if creation fails.
- **Writes use `COPY`** in batches of up to 5,000 rows or 1 s, whichever comes first, through `pgx.CopyFrom`.
- **Indexes:** raw partitions get a btree on `(group_id, ts)`; rollup partitions on `(series_id, ts)`. No other indexes.
- **Layout changes** (a plugin adds a metric) create a new `layout_version` and new group rows, so old rows are never reinterpreted.
- **Counters** are stored as computed rates, not raw counter values. Rate calculation, wrap and reset handling happen in the plugin before storage (see [F10](../features/F10-mvp-check-catalog.md)).
- **Grafana access** goes through SQL views: `metrics_raw` unnests the arrays into `(ts, series_id, name, labels, value)`; `metrics_5m` and `metrics_1h` join rollups with series. Dashboards query the views, never the base tables, so the layout can change.

Rollups are computed by the `maintenance` role every minute for closed 5-minute buckets and every 10 minutes for closed hourly buckets, reading raw samples (for 5 m) and 5 m rollups (for 1 h). Late data older than one closed bucket is accepted into raw storage and triggers a recompute of that bucket.

### Other backends

The engine talks to metrics only through a Go interface (`metrics.Store`: `Write(batch)`, `Query(q)`, `ApplyRetention(policy)`, `EnsurePartitions(now)`).

- **TimescaleDB** (detected via `pg_extension` at startup): the same logical schema as hypertables with compression and continuous aggregates. The views stay identical, so dashboards do not change.
- **ClickHouse** (phase 3): narrow `MergeTree` table with TTL per retention class and materialized views for rollups.

## Consequences

- Plain PostgreSQL stays viable at the design target: roughly 7.5 GB/day raw plus about 4.4 GB/day for 5-minute rollups (200,000 series × 288 buckets) and 0.4 GB/day for hourly rollups. Operators size raw retention in days and 5-minute retention in weeks.
- Grafana queries on raw data pay an `unnest` cost; long-range panels should use the rollup views. The dashboard pack will use `$__interval` to pick the right view.
- Arrays with `NaN` placeholders mean a sparse metric (one collected every tenth run) wastes some space. Acceptable; revisit if a plugin needs very sparse metrics.
- **Load-test gate:** before MVP sign-off, run 10,000 simulated devices for 24 hours on a reference PostgreSQL server and record insert latency, disk growth and rollup lag. If disk growth exceeds the estimate by more than 50 %, revisit this ADR.

## Implementation notes (M3)

These follow from building it; the decision itself is unchanged.

- **Partition periods.** Raw samples and 5-minute rollups are partitioned by day, hourly rollups by month. Names carry the UTC start: `metric_samples_c2_d20261004` and `metric_rollup_1h_c2_m202610`.
- **Partition functions.** `metrics_ensure_partitions(from, until)` and `metrics_drop_expired()` are `SECURITY DEFINER` functions owned by the schema owner, so the system role creates and drops partitions without owning the tables. Dropping takes a 5-second lock timeout, so writers never queue behind a long dashboard query.
- **Missing partitions.** The writer creates a missing partition itself when `COPY` reports one, so writes do not depend on the maintenance role being up.
- **Layout versions.** `layout_version` is a hash of the class and of each slot's name, unit and kind, so a layout change gets a new group without anyone having to bump a version.
- **Late data.** Each rollup run recomputes one bucket before its watermark. Data later than that is not rolled up yet (see progress, deferred from M3).
- **Grafana access.** The views run with the owner's rights and show every tenant. They are granted only to the optional role `monitor_grafana`, which is read-only and has a statement timeout.
