# The monitoring agent

The agent is one small static binary (about 11 MB, no dependencies) that you add to a Docker image at build time, or run beside it. It reports what the image is doing to the monitoring server: the container's CPU, memory, disk and network, the process that runs in it, probes of local ports and URLs, and the services inside (PostgreSQL, Redis, nginx, php-fpm, Elasticsearch, Node.js). Docker hosts can also run it as a sidecar that watches every container on the host.

It is one agent with many collectors, not one agent per service. A collector is a module inside the binary that you switch on with configuration. See [ADR-0016](adr/0016-single-agent-binary.md) for why.

The server side is the `host.agent` check. The agent pushes over HTTP to the ingest listener with the check's own token ([F08](features/F08-push-ingestion.md)). Nothing in the container listens for connections.

> New to this? Start with the [Docker integration guide](agent-docker-integration.md).

## 1. Create the check

One check per agent. The `agent_id` is a name you choose; the same name goes to the agent.

```sh
mon -X POST $MON_URL/v1/devices -d '{"name":"shop-web-1","type":"server"}'     # note the device id

mon -X POST $MON_URL/v1/devices/$DEVICE/checks -d '{
  "name": "agent",
  "plugin": "host.agent",
  "is_host_check": true,
  "interval_seconds": 30,
  "config": { "agent_id": "shop-web-1" },
  "thresholds": [
    { "metric": "mem_usage_pct",  "object": "host", "warning": { "op": ">", "value": 85 }, "critical": { "op": ">", "value": 95 } },
    { "metric": "container_restarts", "object": "*", "warning": { "op": ">", "value": 3 } }
  ]
}'
```

The response carries `push_token` (`mpush_...`) **once** and `push.ingest_url`. Keep both.

- An agent that stays silent for `missed_count` (default 3) intervals makes the check CRITICAL.
- `agent_id` is unique within the tenant. A second check with the same ID is refused (409).
- Rotate a token with `POST /v1/checks/{id}/rotate-token`; the old token stops at once.
- `host.agent` is billed with weight 2 ([F13](features/F13-usage-metering.md)).

## 2. Put the agent in your image

Copy the binary out of the published image and make it the entrypoint. Your original command follows `--`:

```dockerfile
FROM nginx:1.27-alpine
COPY --from=registry.plusclouds.com/monitoring-agent:latest /agent /usr/local/bin/agent
ENV AGENT_COLLECTORS=host,nginx
ENTRYPOINT ["/usr/local/bin/agent", "run", "--"]
CMD ["nginx", "-g", "daemon off;"]
```

At run time, give it the URL and token:

```sh
docker run -e AGENT_URL=https://monitor.example.com/ingest/v1/<check_id> \
           -e AGENT_TOKEN_FILE=/run/secrets/agent_token \
           -e AGENT_ID=shop-web-1 my-nginx
```

More examples: [nginx](../deploy/agent/examples/nginx.Dockerfile), [redis](../deploy/agent/examples/redis.Dockerfile), [node](../deploy/agent/examples/node.Dockerfile). Pin a version tag instead of `latest` in production.

What `agent run -- <command>` does:

- Starts your command first, with the same environment and stdio. Whatever is wrong with the agent's setup (missing URL, bad option), your service still starts; the agent logs the problem and monitoring stays off.
- Forwards signals (`SIGTERM`, `SIGINT`, `SIGHUP`, `SIGQUIT`, `SIGUSR1`, `SIGUSR2`) to your process, so `docker stop` works as before.
- As PID 1, collects orphaned child processes, so no zombies pile up.
- Reports the process as the object `process:main`: up, uptime, and when it exits, the exit code or the signal. It sends one last report, then exits with the same exit code, so your restart policy sees the original result.
- Needs no shell, so it works in distroless and `scratch` images too.

The agent does not restart your process. Docker's restart policy does that.

## 3. Or run it as a sidecar

One agent per Docker host that watches the host and all of its containers: [docker-compose.sidecar.yml](../deploy/agent/docker-compose.sidecar.yml).

```sh
docker run -d --name monitoring-agent --user root \
  -e AGENT_URL=... -e AGENT_TOKEN_FILE=/run/secrets/agent_token \
  -e AGENT_COLLECTORS=host,docker \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  registry.plusclouds.com/monitoring-agent:latest
```

**Security:** anything that can read the Docker socket can read the environment variables of every container on the host, secrets included. Run the sidecar from the agent image only, read-only socket, and do not put the socket into application images. The `run` mode in an application image never needs it.

## 4. Collectors

Enable collectors with `AGENT_COLLECTORS=host,redis,...` or in the config file ([deploy/agent/agent.example.yaml](../deploy/agent/agent.example.yaml)). Without either, only `host` runs. Each collector's options are config keys, or environment variables `AGENT_<TYPE>_<OPTION>` (`AGENT_REDIS_ADDR`), and `_FILE` for secrets (`AGENT_REDIS_PASSWORD_FILE`).

| Collector | Object | What it reports | Needs |
| --- | --- | --- | --- |
| `host` | `host` | CPU, load, memory, swap, fullest disk, network, uptime, OOM kills. Inside a container with cgroup limits, CPU and memory are the container's own, against its limits. | nothing |
| `docker` | `container:<name>` | running, health, restarts, exit code, CPU, memory, network, uptime; the engine as `docker:engine` | Docker socket |
| `tcp` | `tcp:<name>` | port accepts connections, connect time | `addr` |
| `http` | `http:<name>` | status code, response time, optional body match; redirects are reported, not followed | `url` |
| `process` | `process:<name>` | number of matching processes (reads `/proc`) | `match` |
| `redis` | `service:<name>` | clients, memory, ops/s, hit ratio, evictions, keys, blocked clients, replica link | `addr`, optional password |
| `postgres` | `service:<name>` | connections vs limit, lock waits, longest query, cache hit, transactions/s, deadlocks, size, XID age, replica lag | a role with `pg_monitor` |
| `nginx` | `service:<name>` | active and waiting connections, requests/s, dropped connections | `stub_status` location |
| `phpfpm` | `service:<name>` | active, idle, queued workers, `max_children` reached, slow requests, accepted connections/s | `pm.status_path` |
| `elasticsearch` | `service:<name>` | cluster status (yellow warns, red is critical), unassigned shards, response time | `url`, optional login |
| `nodejs` | `service:<name>` | health endpoint, plus event-loop lag, heap, handles, memory, CPU from `prom-client` defaults | `url` and/or `metrics_url` |

A service that does not answer is reported as CRITICAL with `up = 0` on its own object; it is not an agent error. A collector that fails because of the agent's own setup (unreadable `/proc`) is reported as an UNKNOWN object `collector:<name>`.

**Docker collector and stopped containers.** With `label: monitor.enable=true`, only labelled containers are watched, and they are reported when stopped (critical after a failure, a warning after a clean exit). Without a label, all running containers are watched, and a container the agent saw running stays reported when it stops, so a crash alerts. Containers already stopped when the agent started are left out. A container labelled `monitor.ignore=true` is never reported.

**PostgreSQL role:**

```sql
CREATE ROLE monitor LOGIN PASSWORD '...';
GRANT pg_monitor TO monitor;
```

**Redis** is read over a plain TCP connection (`INFO`); there is no TLS yet, so use loopback or a tunnel.

### Thresholds

The agent reports facts; the server decides. Set thresholds on the check. `object` is `*` (every object that has the metric; objects without it are skipped, so `container_restarts` only ever applies to containers) or one object's key (`host`, `container:web`, `service:redis`). The metric names are in `GET /v1/plugins/host.agent`. Metrics that are not set for an object stay empty; an unknown metric name from a newer agent is dropped and counted in the check output.

## 5. Configuration reference

| Environment | File key | Default |
| --- | --- | --- |
| `AGENT_URL` | `url` | required: the check's `ingest_url` |
| `AGENT_TOKEN`, `AGENT_TOKEN_FILE` | `token`, `token_file` | required |
| `AGENT_ID` | `agent_id` | the hostname; must equal the check's `config.agent_id` |
| `AGENT_INTERVAL` | `interval` | `30s` (minimum `5s`) |
| `AGENT_CA_FILE` | `ca_file` | system roots |
| `AGENT_COLLECTORS` | `collectors` | `host` |
| `AGENT_CONFIG` | | `/etc/monitor-agent/agent.yaml` if it exists |

Keep `interval` at or below the check's `interval_seconds`.

## 6. Commands

```
agent run -- <command> [args]   wrap the image's process (use as ENTRYPOINT)
agent daemon                    sidecar or standalone (the image default)
agent check [--config f]        collect twice, one second apart, print the report as JSON; sends nothing
agent version
```

Use `agent check` inside a container to test a collector's options before you create the check:

```sh
docker exec -e AGENT_COLLECTORS=host,redis my-container agent check
```

## 7. Delivery

- A report is the current state, sent every interval as one JSON POST (limit 64 KiB; when a report is too big, the last objects are left out and the first object's output says how many).
- Network errors, `429` and `5xx` are retried twice with a pause, honouring `Retry-After` up to 10 seconds. `401`, `400`, `409` and the like are final; the next interval sends a fresh report. Old reports are never queued behind new ones.
- The counters `agent_push_errors` and `agent_collect_errors` are metrics of the `host` object.
- Answers: `202` accepted, `401` bad token, `400` malformed report or wrong `agent_id`, `409` check disabled, `413` too big, `429` too fast.

## 8. Building

```sh
make build-agent                                      # bin/agent
docker build -f Dockerfile.agent -t monitoring-agent .   # about 10 MB
```

CI builds and pushes `registry.plusclouds.com/monitoring-agent:<version>` and `:latest` with every version tag, next to the server image.

Linux only (containers). Adding a collector means one package under `internal/agent/collectors/` that calls `agent.Register` in `init`, one import in `internal/agent/collectors/all`, and, if it reports a metric name the catalog does not have yet, one line appended to the catalog in `plugins/hostagent/hostagent.go` (append only; slot numbers must not change).
