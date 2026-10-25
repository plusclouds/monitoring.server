# ADR-0016: One agent binary with collectors for Docker images

**Status:** Accepted · **Date:** 2026-10-25

## Context

We want to monitor what runs in Docker: the container, the process in it, and the services (PostgreSQL, Redis, nginx, php-fpm, Elasticsearch, Node.js, more later). The agent must be added to an image while the image is built, and must also be usable beside the images to watch the Docker engine.

The server already has push ingestion with a token per check ([F08](../features/F08-push-ingestion.md)) and two single-purpose agents' receivers (`box.agent`, `vm.agent`). It has no generic agent, and the exec-plugin idea of [ADR-0004](0004-compiled-in-plugin-registry.md) is for probes only.

## Options considered

1. **One binary per service** (`agent-redis`, `agent-postgres`, ...). An image that runs two services carries two binaries and two tokens; every release is N pipelines; the host and container metrics are repeated in each.
2. **One binary with collectors** compiled in and switched on by configuration. One layer to copy, one token and one check per image or host, one release. The binary is larger by the size of its collectors.
3. **A binary that loads plugins at run time** (exec or Go plugins). Needs a shell or shared libraries that distroless and `scratch` images do not have; ADR-0004 keeps exec plugins out of tenant-facing code.

## Decision

Option 2: `cmd/agent`, collectors in `internal/agent/collectors/*`, registered like the server's plugins.

- **Transport:** HTTP push to the existing `/ingest/v1/{check_id}` with the check's push token. The container opens no port. A new ingester plugin, `host.agent`, turns a report into objects (host, containers, probes, services). No change to ingest.
- **Wire format:** one JSON report per interval with a list of objects, each with a key, kind, status, output and named metrics. The server owns one metric catalog (append only); an unknown name is dropped and counted, so a newer agent does not break an older server.
- **Adding it to an image:** `COPY --from=registry.plusclouds.com/monitoring-agent /agent /usr/local/bin/agent` and `ENTRYPOINT ["agent","run","--", ...]`. The agent starts the image's own process first, forwards signals, reaps orphans as PID 1 and passes the exit code on; it never stops a service from starting because of its own configuration errors. A static binary with no shell works in distroless images.
- **Sidecar:** the same image with `daemon` and the Docker socket mounted read-only, to watch every container on a host. The socket gives read access to all containers' environment variables, so this runs only in the agent's own image, never inside application images.
- **Dependencies:** stdlib wherever the protocol is simple (Docker API over the unix socket, Redis `INFO`, FastCGI, stub_status, Prometheus text); `pgx` (already linked by the server, MIT) for PostgreSQL. All fit [ADR-0010](0010-dependency-license-policy.md).
- **Not in this decision:** a tenant batch endpoint and self-enrollment of agents (one check per agent for now, created through the API), mTLS, agent self-update.

## Consequences

- One billable unit per agent (`host.agent`, weight 2, to be confirmed with billing), whatever it watches.
- Each collector is a small package with a fake-server test; new services are additive.
- A sidecar for a host with 200 containers sends 200 objects every interval; the report is cut at 64 KiB, so very large hosts need the batch endpoint or label filtering (`monitor.enable=true`).
- Agent and server versions can differ: the report has a version (`v`), and metrics are matched by name.
