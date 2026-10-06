# F07: Metrics store, rollups and retention

**Status:** Implemented (MVP, PostgreSQL backend) · **Phase:** MVP (PostgreSQL backend); phase 2 (TimescaleDB); phase 3 (ClickHouse) · **Related:** [ADR-0003](../adr/0003-metrics-storage-layout.md)

## Summary

Metrics from checks, collectors and push ingestion are written in batches to the metrics store, rolled up into 5-minute and 1-hour buckets, and removed according to retention policies set through the API. Grafana reads them through stable SQL views; the API offers a query endpoint for the panel.

## Behavior

### Write path

1. Engine receives a result with a metric array aligned to the plugin's layout.
2. Series and group IDs are resolved from an in-memory cache (loaded lazily, keyed by device, source, object and layout version). New groups and series are inserted with `ON CONFLICT DO NOTHING` and cached.
3. Rows are buffered and flushed with `COPY` every 1 s or 5,000 rows.
4. On flush failure the buffer retries with backoff and keeps up to 60 s of data in memory; beyond that it drops the oldest rows and counts them (`metrics_dropped_total`). Metrics never block state processing.

### Rollups

- 5-minute rollups from raw, hourly rollups from 5-minute, each storing `min`, `max`, `sum`, `count` (average = sum / count).
- Computed by the `maintenance` role (singleton via advisory lock) for closed buckets, with a watermark table so a restart resumes where it stopped.
- Rates are rolled up like gauges (the stored value is already a rate).

### Retention

- `retention_policies`: `(metric_class, level, duration)` where level is `raw`, `5m` or `1h`. Partitions are shared across tenants, so retention is **platform-wide per class**, not per tenant; tenants can only choose which class their checks use (subject to their plan).
- Changing a policy takes effect on the next maintenance run (hourly): partitions entirely older than the duration are dropped.
- There are no hardcoded defaults; the installer asks for values and suggests raw 7 days, 5 m 90 days, 1 h 2 years for `standard`.

### Query API

`GET /v1/metrics/query` with `series` selector (device, check, name, labels), `from`, `to`, `step`, `agg` (`avg`, `min`, `max`, `sum`). The engine chooses raw, 5 m or 1 h automatically from `step` and the retention available. Maximum 10,000 points per series per response.

`GET /v1/metrics/series` lists series for a device or check (for building graphs in the panel).

### Grafana

- A PostgreSQL data source pointed at the views `metrics_raw`, `metrics_5m`, `metrics_1h`, `metric_series_v`, plus `devices_v`, `check_states_v`, `incidents_v` for tables and status panels.
- A starter dashboard pack in `deploy/grafana/` (MVP: device overview, interface traffic, server hardware, XCP-ng host and VM, website checks). The full pack is phase 3.

## Acceptance criteria

- Sustained 7,000 values/s for 24 h with flush p99 under 500 ms and no dropped rows.
- Measured disk growth within 50 % of the estimate in [ADR-0003](../adr/0003-metrics-storage-layout.md).
- Changing `standard/raw` retention from 7 to 3 days removes the older partitions within one maintenance run.
- A Grafana panel over 30 days reads from rollups and renders in under 2 s for one device.

## Decided

- **Retention per plan** is done with retention classes, not per-tenant policies, because partitions are shared across tenants. A plan that needs longer or shorter retention maps to its own class (for example `standard-30d`, `standard-1y`), and the tenant's `metric_classes` limit ([F01](F01-tenancy-auth-audit.md)) decides which classes its checks may use.

## Implementation status (M3)

What exists and what was deferred is recorded in [progress](../progress.md#m3--metrics-f07). Differences from the text above:

- **Sample arrays.** The array column is `vals`, because `values` is a reserved word.
- **Retention class.** A metric goes to its plugin's class when the tenant's `metric_classes` allows it, else `standard`, else the tenant's first allowed class.
- **Query alignment.** Buckets start at multiples of `step` since 2000-01-01 UTC. The query fills buckets the rollups have not reached yet from the finer level.
- **Standalone installs** set retention with `monitor admin retention`, because they have no platform key.

## Statistics from raw samples (2026-10-07)

Percentiles and standard deviations need every sample, which rollups do not keep (they hold min, max, sum and count). They are computed from raw samples only, so they are exact, and a window older than raw retention is refused (422 on `from`), never approximated. Long-range percentiles (a mergeable sketch in the rollups) were not wanted.

- `GET /v1/metrics/query`: `agg` also takes `stddev` (sample, n-1) and percentiles `p<n>` with up to three decimals (`p50`, `p95`, `p99.9`), per bucket, by linear interpolation between the closest ranks. `resolution` other than `raw` is refused for them. `moving_window=N` replaces every point with the mean of the last N points (any `agg`).
- `GET /v1/metrics/summary`: per metric over the whole window (default the last hour): `count`, `min`, `max`, `avg`, `stddev`, `p50`, `p95`, `p99`, extra `percentile=` values, `last`, `last_at`, and with `window=N` the mean and standard deviation of the last N samples (the Whoopsy! band). `exact: true`, `resolution: raw`. Same selector as the query (`device_id` or `check_id`, repeated `name`, `object` for collector objects).
- A request reads at most 2,000,000 raw samples (422 beyond).
