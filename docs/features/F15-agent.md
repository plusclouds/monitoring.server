# F15: Monitoring agent for Docker images

**Status:** Draft, phase 1 built (see "As built") · **Depends on:** F03, F05, F07, F08 · **Decision:** [ADR-0016](../adr/0016-single-agent-binary.md) · **Guide:** [agent.md](../agent.md)

## Summary

A small static binary, added to a Docker image while it is built (or run as a sidecar), that reports the container or host, the containers on a Docker engine, local probes and the services inside the image to the server over HTTP push. The server side is the `host.agent` ingester plugin.

## Scope

**Phase 1 (built)**

- `cmd/agent` with `run`, `daemon`, `check`, `version`.
- Collectors: `host`, `docker`, `tcp`, `http`, `process`, `redis`, `postgres`, `nginx`, `phpfpm`, `elasticsearch`, `nodejs`.
- `host.agent` plugin, push token, unique `agent_id`, usage weight, Grafana dashboard.
- `Dockerfile.agent` and the CI image `monitoring-agent`.

**Phase 2 (not built)**

- Tenant batch endpoint `POST /ingest/v1/batch` ([F08](F08-push-ingestion.md)) and agent self-enrollment with a one-time token, so one agent can cover many hosts without a check per agent.
- More collectors: MySQL/MariaDB, MongoDB, RabbitMQ, memcached, Kafka, HAProxy, Traefik, MinIO, a Prometheus scrape, and an exec collector (Nagios exit codes, [ADR-0004](../adr/0004-compiled-in-plugin-registry.md)).
- Redis TLS; `unix:` sockets for more collectors.
- Kubernetes: a DaemonSet example and pod labels.
- A signed release binary besides the image.

## Behavior

- One report per interval (default 30 s, minimum 5 s): a JSON document with `agent_id`, `seq`, `ts`, `agent_version` and a list of objects. Each object has `key`, `kind`, `name`, `status` (`ok`, `warning`, `critical`, `unknown`), `output`, `labels` and `metrics` (name to number).
- The plugin maps metric names to a fixed catalog of slots shared by all object kinds. Unknown names are dropped and counted in the check output. The check's status is the worst object status; thresholds are evaluated per object by the engine.
- Silence is CRITICAL after `missed_count` intervals (last-seen rule). `agent_id` in the report must equal the check's, so a token cannot report for another agent.
- `agent run -- cmd` starts the process first, whatever the agent's own configuration; it forwards signals, reaps orphans as PID 1, reports the process and its exit code, and exits with the process's exit code.

## Data

`host.agent` check config: `agent_id` (required, unique per tenant, case-insensitive), `missed_count` (1 to 100, default 3). Objects: `host`, `container:<name>`, `docker:engine`, `process:main`, `process:<name>`, `tcp:<name>`, `http:<name>`, `service:<name>`, `collector:<name>` (agent-side problem). Migration `00026_host_agent.sql` adds the unique index.

## API

No new endpoints. `POST /v1/devices/{id}/checks` with `plugin: host.agent` returns `push_token` once; `POST /v1/checks/{id}/rotate-token` rotates it; reports go to `POST /ingest/v1/{check_id}`.

## Acceptance criteria

- A report becomes host, container and service objects with their own state and incidents (`TestHostAgent`).
- A report with another agent's ID, a bad token or a malformed body is refused; a rotated token stops the old one; silence is CRITICAL.
- `agent run` passes the exit code and signals through and starts the process when the agent is misconfigured.
- The image is under 25 MB and runs without a shell.

## Open questions

- Billing: one unit per agent regardless of what it watches (weight 2, TBD with billing) or per watched object.
- Batch endpoint shape for a sidecar that watches hundreds of containers (the report is cut at 64 KiB today).

## As built

Phase 1 in `v0.15.0`: see [docs/progress.md](../progress.md). Verified by `TestHostAgent`, the unit tests under `internal/agent/...` and `plugins/hostagent`, and a run against real redis, postgres, nginx and Docker containers.
