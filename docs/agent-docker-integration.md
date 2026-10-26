# Integrating the monitoring agent into a Docker image

This is the step-by-step guide for adding the agent to an image you build, so the containers it produces report to the monitoring server. For what each collector reports and every option, see the [agent reference](agent.md).

`host.agent` is the name of the server-side check that receives an agent's reports. It is not tied to the monitoring server's own host: every container you monitor gets its own agent and its own `host.agent` check.

## What you need

- A monitoring server version 0.15.0 or later (it has migration `00026`).
- An API key for your tenant (`$MON_KEY`) and the server URL (`$MON_URL`).
- Access to the registry that holds `registry.plusclouds.com/monitoring-agent:<version>`.

## 1. Create the device and the check

Do this once per container (or per deployment that runs a single instance). The agent and the check must use the same `agent_id`.

```sh
mon() { curl -sS -H "Authorization: Bearer $MON_KEY" -H "Content-Type: application/json" "$@"; }

DEVICE=$(mon -X POST $MON_URL/v1/devices -d '{"name":"leo4-app-1","type":"server"}' | jq -r .id)

mon -X POST $MON_URL/v1/devices/$DEVICE/checks -d '{
  "name": "agent",
  "plugin": "host.agent",
  "is_host_check": true,
  "interval_seconds": 30,
  "failure_count": 3,
  "config": { "agent_id": "leo4-app-1", "missed_count": 3 },
  "thresholds": [
    { "metric": "mem_usage_pct", "object": "host", "warning": { "op": ">", "value": 85 }, "critical": { "op": ">", "value": 95 } }
  ]
}' | tee check.json
```

From the response keep:

- `id`: the check ID.
- `push_token`: shown once. Store it as a secret.
- `push.ingest_url`: the URL the agent posts to (`https://<ingest host>/ingest/v1/<check id>`).

`failure_count: 3` stops a slow-starting application from raising an incident while it boots. `agent_id` is unique within your tenant.

## 2. Add the agent to the Dockerfile

The agent is a static binary. Copy it from the agent image, make it the entrypoint, and keep your original command as the arguments after `--`.

```dockerfile
# Pin a version. 0.15.0 or later.
ARG AGENT_VERSION=0.15.0
FROM registry.plusclouds.com/monitoring-agent:${AGENT_VERSION} AS agent

FROM nginx:1.27-alpine
COPY --from=agent /agent /usr/local/bin/agent

# Which collectors to run, and their settings (see the reference for all).
ENV AGENT_COLLECTORS=host,nginx,process \
    AGENT_NGINX_URL=http://127.0.0.1:8081/nginx_status \
    AGENT_PROCESS_MATCH=nginx

ENTRYPOINT ["/usr/local/bin/agent", "run", "--"]
CMD ["nginx", "-g", "daemon off;"]
```

Rules that keep this safe:

- **Do not hard-code the URL or token in the image.** Pass them when the container starts (step 3).
- The agent starts your command first. If the agent is misconfigured, your service still starts and the agent logs why monitoring is off.
- If the image already has an `ENTRYPOINT`, move it into the arguments: `ENTRYPOINT ["/usr/local/bin/agent","run","--","docker-entrypoint.sh"]` and keep `CMD` as it was.
- Signals (`docker stop`) reach your process, and the container exits with your process's exit code. Restart policy keeps working as before.
- It needs no shell and no extra packages, so it also works in distroless and `scratch` images.

### Optional: switch it on only when configured

If the same image runs in places that should not report, wrap the agent in a small script that falls back to the plain command when no URL is set. This is what the leo4 image does ([`.docker/agent-entrypoint.sh`](https://github.com/plusclouds/leov4)):

```sh
#!/bin/sh
if [ -z "$AGENT_URL" ]; then
    exec "$@"
fi
exec /usr/local/bin/agent run -- "$@"
```

```dockerfile
COPY agent-entrypoint.sh /usr/local/bin/agent-entrypoint.sh
ENTRYPOINT ["/usr/local/bin/agent-entrypoint.sh"]
CMD ["your", "original", "command"]
```

### Choosing collectors for the image

| The image runs | Use |
| --- | --- |
| Any application | `host` (CPU, memory, disk, network of the container) and `process` or `http` for its own health |
| nginx | `nginx`, with `location /nginx_status { stub_status; }` on a loopback-only port |
| PHP-FPM | `phpfpm`, with `pm.status_path = /status` in the pool; use `addr: unix:/run/php/php-fpm.sock` or `127.0.0.1:9000` |
| PostgreSQL | `postgres`, with a role that has `pg_monitor` |
| Redis | `redis` |
| Elasticsearch | `elasticsearch` |
| Node.js | `nodejs`, with a health URL and optionally `prom-client` metrics |
| supervisord and workers | `process` entries such as `match: queue:work` |

More than one instance of a type needs a config file with distinct `name`s (environment variables only cover one instance per type). Copy [agent.example.yaml](../deploy/agent/agent.example.yaml), add it to the image and set `AGENT_CONFIG` to its path.

## 3. Give the container its settings at run time

Three values per container. The token belongs in a secret, not in an environment variable of the image.

| Setting | Value |
| --- | --- |
| `AGENT_URL` | `push.ingest_url` from step 1 |
| `AGENT_TOKEN_FILE` | path of a mounted secret holding `push_token` (or `AGENT_TOKEN` for development) |
| `AGENT_ID` | the `agent_id` from step 1 |

**docker run**

```sh
docker run -d --name leo4-app-1 \
  -e AGENT_URL=https://monitor.example.com/ingest/v1/<check id> \
  -e AGENT_ID=leo4-app-1 \
  -e AGENT_TOKEN_FILE=/run/secrets/agent_token \
  -v $PWD/agent_token:/run/secrets/agent_token:ro \
  my-image
```

**Docker Compose**

```yaml
services:
  app:
    image: my-image
    environment:
      AGENT_URL: https://monitor.example.com/ingest/v1/<check id>
      AGENT_ID: leo4-app-1
      AGENT_TOKEN_FILE: /run/secrets/agent_token
    secrets: [agent_token]
secrets:
  agent_token:
    file: ./agent_token
```

**Kubernetes**

```yaml
env:
  - { name: AGENT_URL, value: "https://monitor.example.com/ingest/v1/<check id>" }
  - { name: AGENT_ID,  value: "leo4-app-1" }
  - name: AGENT_TOKEN
    valueFrom: { secretKeyRef: { name: leo4-agent, key: token } }
```

**One check per container.** Two containers must not share a token and `agent_id`: their reports would overwrite each other. For a Deployment with several replicas, create one check per replica until the batch endpoint exists, or use the sidecar mode (step 5) to cover them from the host.

## 4. Verify

Before the first deploy, test the collectors inside a running container. This sends nothing:

```sh
docker exec -e AGENT_COLLECTORS=host,nginx my-container agent check
```

You should see one JSON report with an object per collector. A service that does not answer shows `"status": "critical"` with the reason in `output`.

After the container starts with its settings:

```sh
# The check turns OK within one interval.
mon $MON_URL/v1/checks/$CHECK/state | jq '{phase, last_output}'

# The reported objects: host, services, probes, process:main.
mon $MON_URL/v1/checks/$CHECK/objects | jq -r '.items[] | [.key, .status] | @tsv'
```

Then open the Grafana dashboard "Agents: hosts, containers and services".

## 5. Watching the Docker host instead (sidecar)

When you cannot or do not want to change the images, run one agent per Docker host, next to your containers:

```sh
docker run -d --name monitoring-agent --user root --restart unless-stopped \
  -e AGENT_URL=... -e AGENT_ID=docker-host-1 -e AGENT_TOKEN_FILE=/run/secrets/agent_token \
  -e AGENT_COLLECTORS=host,docker \
  -v /var/run/docker.sock:/var/run/docker.sock:ro \
  -v $PWD/agent_token:/run/secrets/agent_token:ro \
  registry.plusclouds.com/monitoring-agent:0.15.0 daemon
```

It reports each container's state, health, restarts, exit code, CPU, memory and network. It cannot see inside a container (no nginx or PHP-FPM metrics); for those, bake the agent into the image.

Anything that can read the Docker socket can read every container's environment, secrets included. Run this container from the agent image only and keep the socket read-only. See [deploy/agent/docker-compose.sidecar.yml](../deploy/agent/docker-compose.sidecar.yml).

## 6. Alerts

The agent reports facts; thresholds live on the check (step 1 shows one). Useful starting points:

| Metric | Object | Warning | Critical |
| --- | --- | --- | --- |
| `mem_usage_pct` | `host` | > 85 | > 95 |
| `disk_usage_pct` | `host` | > 80 | > 90 |
| `container_restarts` | `*` | > 3 | |
| `phpfpm_max_children_reached` | `service:phpfpm` | > 0 | |
| `connections_usage_pct` | `service:postgres` | > 80 | > 95 |

Update thresholds with `PATCH /v1/checks/{id}`. An agent that goes silent for `missed_count` intervals makes the check CRITICAL on its own, so a dead container or a broken network path alerts without any threshold.

## Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| No data, check shows silence | Wrong `AGENT_URL`, or the ingest listener is not reachable from the container. Run `docker logs` and look for `report not delivered` |
| `server refused the report: 401` | Wrong or rotated token. Rotate with `POST /v1/checks/{id}/rotate-token` and update the secret |
| `400` | `AGENT_ID` does not match the check's `config.agent_id` |
| `409` | The check is disabled |
| `429` | Interval below the server's per-check limit. Use 30 s |
| Service object critical, `connection refused` | The collector's address is wrong from inside the container, or the service is not up yet |
| nginx: `enable it with stub_status` | Add the `stub_status` location and make it reachable on the URL the agent uses |
| php-fpm: `not a php-fpm status page` | `pm.status_path` is not set in the pool |
| `monitoring disabled` in the logs, app runs | Agent config error. The message says which setting; fix and restart |
| Container exit code differs from before | It should not: the agent exits with the wrapped process's code. Check that the command after `--` is the original one |
