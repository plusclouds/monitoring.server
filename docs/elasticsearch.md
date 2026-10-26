# Monitoring Elasticsearch

The `elasticsearch.cluster` plugin reads an Elasticsearch cluster through its REST API and reports the cluster, its nodes and its largest indices. It is a collector: one check per cluster produces many objects, each with its own metrics, thresholds, state and incidents.

Status: implemented in `plugins/elasticsearch`, covered by tests against a fake cluster. Not yet run against a real cluster. Unreleased (see the [changelog](../CHANGELOG.md)).

## Set it up

1. Create a device for the cluster. The address is any node: a host, `host:port` or an `http(s)://` URL. Without a scheme or port, the check uses `https` and 9200.
2. Optional: create a credential and assign it in the `auth` role.
   - `http_basic`: username and password of a user with the `monitor` cluster privilege.
   - `http_bearer`: a bearer token.
   - Elasticsearch API keys (`Authorization: ApiKey ...`) are not supported yet.
3. Create a check with plugin `elasticsearch.cluster`. The default interval is 1 minute; the minimum is 30 seconds.

```
POST /v1/devices/{device_id}/checks
{
  "name": "elasticsearch",
  "plugin": "elasticsearch.cluster",
  "config": { "index_pattern": "logs-*", "max_indices": 50 }
}
```

## Config

| Field | Default | Meaning |
| --- | --- | --- |
| `scheme` | `https` | Used when the device address is not a URL |
| `port` | `9200` | Used when the device address has no port |
| `verify_certificate` | `true` | Set `false` for a private CA the engine does not trust |
| `index_pattern` | `*` | Indices to report, as an Elasticsearch pattern such as `logs-*,metrics-*` |
| `include_system_indices` | `false` | Report indices whose name starts with a dot |
| `max_indices` | `100` | Most indices reported, the largest first. `0` reports none. Maximum 1000 |

## What it reports

Objects:

| Key | Name | Status |
| --- | --- | --- |
| `cluster` | cluster name | green OK, yellow WARNING, red CRITICAL |
| `node:<node id>` | node name | OK (use thresholds on its metrics) |
| `index:<name>` | index name | from the index health; a closed index has none |

The device status is the cluster status. Metrics are one fixed set per object; the ones that do not apply to an object kind are empty.

| Metric | Unit | Objects | Meaning |
| --- | --- | --- | --- |
| `health_code` | 1 | cluster, index | 0 green, 1 yellow, 2 red |
| `nodes`, `data_nodes` | 1 | cluster | Nodes in the cluster |
| `active_shards` | 1 | cluster | Active shards |
| `unassigned_shards` | 1 | cluster | Unassigned shards |
| `relocating_shards`, `initializing_shards` | 1 | cluster | Shards moving or starting |
| `pending_tasks` | 1 | cluster | Tasks waiting for the master |
| `heap_used_percent` | percent | node | JVM heap in use |
| `cpu_percent` | percent | node | CPU use (OS value, else the process value) |
| `disk_used_percent` | percent | node | Data path use |
| `gc_old_ms_per_s` | ms/s | node | Old-generation GC time, from counters kept between runs |
| `rejected_per_s` | 1/s | node | Search and write requests rejected by a full queue |
| `docs` | 1 | index | Documents |
| `store_bytes` | By | index | Size on disk |

Rates are empty on the first run, and after a node restarts (its counters go down).

Suggested thresholds: `health_code` at least 2 critical (1 warning), `unassigned_shards` above 0 warning, `heap_used_percent` above 85 warning, `disk_used_percent` above 85 warning and 90 critical, `rejected_per_s` above 0 warning.

## Failures

| Situation | Status |
| --- | --- |
| No answer, timeout, connection refused | CRITICAL |
| HTTP 503 (no master) or another error status | CRITICAL |
| 401: login rejected | UNKNOWN, check the credential |
| 403: account may not read the cluster | UNKNOWN, grant the `monitor` privilege |
| Certificate verification failed | UNKNOWN, set `verify_certificate` to `false` for a private CA |
| Address refused by the network policy | UNKNOWN |

If the index list cannot be read, the run still succeeds with the cluster and nodes, and the output says why.

## Billing

Each node bills as `elasticsearch.cluster:node` (weight 3, TBD with PlusClouds billing). The cluster check itself has weight 0; indices are not billed. Weights are in `deploy/config.example.yaml`.

## Dashboard

`deploy/grafana/dashboards/elasticsearch.json` ("Elasticsearch clusters"): clusters and nodes tables, cluster health, unassigned shards, pending tasks, node count, node heap, CPU and disk, old-generation GC, rejected requests, index size, and indices that are not green.

## API calls made

Three or four `GET` requests per run, all read-only, on one node:

- `/_cluster/health`
- `/_nodes/stats/jvm,os,process,fs,thread_pool` (filtered to the fields used)
- `/_cat/indices/<pattern>?format=json&bytes=b`
- `/?filter_path=version.number` (the cluster version, shown as the device's firmware)

## Files

| Path | What |
| --- | --- |
| `plugins/elasticsearch/cluster.go` | Manifest, config, collection, status mapping |
| `plugins/elasticsearch/client.go` | HTTP client, address handling, authentication |
| `plugins/elasticsearch/cluster_test.go` | Tests against a fake cluster |
| `plugins/all/all.go` | Registration |
| `internal/config/defaults.go`, `deploy/config.example.yaml` | Usage weights |
| `internal/api/api_integration_test.go` | Plugin list |
| `deploy/grafana/dashboards/elasticsearch.json` | Dashboard |

## Not built

- Elasticsearch API key authentication.
- Per-node child devices (nodes are objects of the cluster device).
- Index lifecycle, snapshot and ingest pipeline metrics.
- OpenSearch is not tested; its APIs are close but not the same.
