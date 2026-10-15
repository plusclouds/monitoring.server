# Deploying the monitoring server on a new VM (standalone)

This guide takes a fresh Linux VM to a running monitoring server with no PlusClouds services involved. You end up with a PostgreSQL database, the engine, a TLS endpoint, and an admin API key you can use right away.

"Standalone" means:

- You run the server and its database yourself.
- You have your own tenant and your own admin API key. There is no PlusClouds panel, no platform key, no PlusClouds NATS, no PlusClouds container registry.
- You build the container image from source. The published image lives in a private PlusClouds registry.

When you are done, continue with the [usage guide](../usage-guide.md) and the [API guide](../api/README.md).

> This procedure was assembled from the repository's compose file, configuration reference and bootstrap code. Run it on a throwaway VM first and report anything that differs.

## 1. What you need

| Item | Requirement |
| --- | --- |
| VM | Linux x86-64 or arm64. 2 vCPU, 4 GB RAM and 20 GB disk handle a few hundred devices. Metrics grow with check count and retention, so size the disk for that. |
| Software | Docker Engine with Docker Compose **2.23 or newer** (the compose file uses inline configs), `git`, `openssl`, `curl`. |
| Network, inbound | `443/tcp` for the API (through a TLS proxy). `8883/tcp` only if you use the MQTT broker. |
| Network, outbound | Reach to the things you monitor (ICMP, HTTP, SNMP, Redfish and so on), and to your webhook receivers. |
| DNS | A name for the server, for example `monitor.example.com`, if you want valid TLS. |

Check the versions:

```sh
docker --version
docker compose version   # must be v2.23 or newer
```

## 2. Get the source and build the image

```sh
git clone https://github.com/plusclouds/monitoring.server.git
cd monitoring.server
docker build \
  --build-arg VERSION="$(git describe --tags --always)" \
  --build-arg COMMIT="$(git rev-parse HEAD)" \
  -t monitoring:local .
```

If you received the source as an archive, build from its root the same way (leave out the git arguments and pass `--build-arg VERSION=1.0.0`).

The build produces a static binary on a distroless image. It needs network access to download Go modules. Confirm it works:

```sh
docker run --rm monitoring:local version
```

## 3. Configure the stack

All deployment files are in `deploy/compose`. Two files are all it needs: `docker-compose.yml` and `.env`.

```sh
cd deploy/compose
cp .env.example .env
```

Fill in `.env`:

```sh
# Passwords: letters and digits only (they are placed in connection URLs). Hex is safe.
sed -i "s|^POSTGRES_SUPERUSER_PASSWORD=.*|POSTGRES_SUPERUSER_PASSWORD=$(openssl rand -hex 24)|" .env
sed -i "s|^MONITOR_OWNER_PASSWORD=.*|MONITOR_OWNER_PASSWORD=$(openssl rand -hex 24)|"           .env
sed -i "s|^MONITOR_APP_PASSWORD=.*|MONITOR_APP_PASSWORD=$(openssl rand -hex 24)|"               .env
sed -i "s|^MONITOR_SYSTEM_PASSWORD=.*|MONITOR_SYSTEM_PASSWORD=$(openssl rand -hex 24)|"         .env

# Key-encryption key for stored device credentials: 32 random bytes, base64.
sed -i "s|^MONITOR_KEK=.*|MONITOR_KEK=$(openssl rand -base64 32)|" .env
```

Then edit these values by hand:

| Variable | Set it to |
| --- | --- |
| `MONITOR_IMAGE` | `monitoring` (the image you built) |
| `MONITOR_VERSION` | `local` |
| `MONITOR_PUBLIC_URL` | The URL clients will use, for example `https://monitor.example.com`. It appears in API links and push-ingest URLs. |
| `MONITOR_IDENTITY_SOURCE` | Optional. Any short label such as `local`. It tags external IDs; standalone installs rarely use it. |
| `MONITOR_TRUSTED_PROXIES` | The address of your TLS proxy as a CIDR, for example `172.17.0.1/32` (see section 6). The default trusts all private ranges. |

Leave `MONITOR_MQTT_*`, `MONITOR_NATS_*` and `MONITOR_HEARTBEAT_URL` alone for now. NATS is only for PlusClouds VM agents and does not apply to you.

**Back up `.env` now**, especially `MONITOR_KEK`. Without that key, every stored device credential (SNMP communities, BMC passwords) is unrecoverable.

### Let the server monitor private networks

By default a tenant may not check private addresses. HTTP checks, ICMP checks and webhooks to RFC 1918, loopback and link-local ranges are refused. On a typical internal network you want the opposite. Allow your ranges **before** you bootstrap, because the standalone tenant takes its limits from these defaults when it is created.

Create `deploy/compose/docker-compose.override.yml`:

```yaml
services:
  monitor:
    environment:
      MONITOR_PLATFORM__TENANT_DEFAULTS__ALLOWED_TARGET_NETWORKS: "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
  bootstrap:
    environment:
      MONITOR_PLATFORM__TENANT_DEFAULTS__ALLOWED_TARGET_NETWORKS: "10.0.0.0/8,172.16.0.0/12,192.168.0.0/16"
```

Narrow the list to the networks you really monitor. The cloud metadata address `169.254.169.254` stays blocked regardless (`outbound.deny_networks`).

Other tenant defaults, such as `max_devices` (1000), `max_checks` (10000) and `min_check_interval_seconds` (30), can be changed the same way. The environment name is `MONITOR_PLATFORM__TENANT_DEFAULTS__` plus the key in upper case. [deploy/config.example.yaml](../../deploy/config.example.yaml) lists every setting.

## 4. Start the server and create your admin key

Do **not** run a plain `docker compose up -d`. The compose file's `bootstrap` service creates a *platform* key for the PlusClouds API. You want a local tenant with an admin key instead, so start the engine alone first, then bootstrap by hand.

```sh
docker compose up -d monitor        # starts PostgreSQL, the role/database setup and the engine
docker compose ps                   # wait until monitor is "healthy"
docker compose logs monitor | tail  # migrations applied, listeners up
```

Create the installation, a platform tenant for self-monitoring, and your own tenant with its first admin key:

```sh
docker compose run --rm bootstrap admin bootstrap --standalone --tenant "My Company"
```

The output looks like this:

```
installation id:    0192f7c4-...
platform tenant id: 0192f7c4-...
tenant id:          0192f7c4-...

Admin API key (shown once):
mon_ab12cd34_...
```

Store the admin key in a password manager. **It is shown once.** If you lose it:

```sh
docker compose run --rm bootstrap admin create-admin-key --tenant <tenant id>
```

Bootstrap runs only once per database. A second run fails with "already bootstrapped", which is safe.

After this, `docker compose up -d` is safe to run any time. Its `bootstrap` service sees the installation exists and does nothing.

### Smoke test (HTTP, on the VM)

```sh
export MON_URL=http://127.0.0.1:8443
export MON_KEY=mon_...        # the admin key

curl -s $MON_URL/healthz
curl -s -H "Authorization: Bearer $MON_KEY" $MON_URL/v1/me
curl -s -H "Authorization: Bearer $MON_KEY" $MON_URL/v1/tenant
```

`/v1/me` returns your tenant and role. `/v1/tenant` shows the limits, including `allowed_target_networks`. The API reference is served at `http://127.0.0.1:8443/docs`.

## 5. Make ICMP work (if ping checks fail)

The container runs as a non-root user. ICMP checks then need unprivileged ICMP sockets. If an ICMP check reports a permission error, allow the container's group to open them by adding to `docker-compose.override.yml`:

```yaml
services:
  monitor:
    sysctls:
      net.ipv4.ping_group_range: "0 2147483647"
```

and run `docker compose up -d monitor`. Alternatively use `cap_add: [NET_RAW]`. HTTP, SNMP, Redfish and other checks need nothing special.

## 6. Put TLS in front

The API listens on plain HTTP on `127.0.0.1:8443`, and HTTP push ingest on `127.0.0.1:9443`. Never expose these directly. Terminate TLS with a reverse proxy on the VM, routing `/` to 8443 and `/ingest/` to 9443.

Caddy (automatic Let's Encrypt certificates), `/etc/caddy/Caddyfile`:

```
monitor.example.com {
    request_body {
        max_size 4MB
    }
    handle /ingest/* {
        reverse_proxy 127.0.0.1:9443
    }
    handle {
        reverse_proxy 127.0.0.1:8443 {
            transport http {
                response_header_timeout 120s
            }
        }
    }
}
```

nginx: [deploy/nginx/monitoring.plusclouds.com.conf](../../deploy/nginx/monitoring.plusclouds.com.conf) is a complete example. Replace the host name, certificate paths and upstream addresses.

Then:

1. Set `MONITOR_PUBLIC_URL=https://monitor.example.com` in `.env`.
2. Set `MONITOR_TRUSTED_PROXIES` to the proxy's address as the engine sees it. With a proxy on the VM host, that is the Docker bridge gateway, usually `172.17.0.1/32` or the gateway of the `monitoring_frontend` network. Check with `docker network inspect monitoring_frontend`. The engine uses `X-Forwarded-For` only from these addresses, for the audit log, API key IP allowlists and rate limits.
3. `docker compose up -d monitor`.
4. Open the firewall for 80 and 443 only. Keep 8443 and 9443 bound to loopback (`MONITOR_API_BIND=127.0.0.1`, the default).

Test from outside:

```sh
curl -s https://monitor.example.com/healthz
curl -s -H "Authorization: Bearer $MON_KEY" https://monitor.example.com/v1/me
```

## 7. Optional components

### MQTT broker for sensors

The engine embeds an MQTT broker for IoT devices. It needs a certificate for a DNS-only name (a CDN or HTTP proxy cannot carry MQTT).

1. Point `mqtt.example.com` at the VM. Open `8883/tcp`.
2. Put the certificate and key in `deploy/compose/tls/` as `mqtt.crt` and `mqtt.key`, readable by uid 65532. [mqtt-cert-hook.sh](../../deploy/compose/mqtt-cert-hook.sh) copies a Let's Encrypt certificate there on renewal. The broker reloads changed files within a minute.
3. In `.env`: `MONITOR_MQTT_ENABLED=true`.
4. `docker compose up -d monitor`.
5. Create device credentials through the API (see the [usage guide](../usage-guide.md#mqtt-sensors)).

Plain port 1883 (`MONITOR_MQTT_PLAIN_ENABLED`) exists only to migrate old firmware without TLS. Leave it off.

### Grafana dashboards

Grafana reads PostgreSQL through a read-only role with starter dashboards for devices, websites, interfaces, hardware, power, hypervisors and cameras.

1. Set `MONITOR_GRAFANA_PASSWORD` in `.env` (hex) and run `docker compose up -d`. The setup service lets the `monitor_grafana` role log in.
2. Run Grafana in the same compose project or on a host that can reach the database. The database is on an internal Docker network and is not published. Follow [deploy/grafana/README.md](../../deploy/grafana/README.md) for the provisioning mounts and variables.

The Grafana views show **every tenant**. Treat that Grafana as an operator tool; give end users access through the API only.

### Heartbeat (dead man's switch)

The server can ping an external URL every minute while it is healthy, so you hear about it when the server itself dies. With healthchecks.io or any similar service, put the ping URL in `.env`:

```
MONITOR_HEARTBEAT_URL=https://hc-ping.com/<uuid>
```

### Remote probes

Probes (the same binary, outbound-only) for remote sites are described in [F09](../features/F09-remote-probes.md). The `probe_service` is disabled in the compose deployment of this release, so a standalone install runs all checks from the server itself.

## 8. Operations

### Backups

Back up two things and test the restore:

1. **The database.** Online dump:
   ```sh
   docker compose exec -T postgres pg_dump -U postgres -Fc monitor > monitor-$(date +%F).dump
   ```
   Restore into an empty database with `pg_restore`. Or snapshot the `monitoring_pgdata` volume while the stack is stopped.
2. **`.env`** (especially `MONITOR_KEK`) and any files in `deploy/compose/tls`.

The database holds the configuration, state, incidents, audit log and metrics. The `.env` file holds the key that opens the stored credentials. One is useless without the other.

### Upgrades

```sh
cd monitoring.server
git pull
docker build --build-arg VERSION="$(git describe --tags --always)" \
             --build-arg COMMIT="$(git rev-parse HEAD)" -t monitoring:local .
cd deploy/compose
docker compose up -d
```

Migrations run when the engine starts (under a database lock). Take a backup before upgrading. Read the release notes in [docs/progress.md](../progress.md) first.

### Logs and health

```sh
docker compose logs -f monitor
docker compose exec monitor /usr/local/bin/monitor health   # exit 0 when ready
```

The node status server listens on loopback inside the container (port 9090): `/healthz`, `/readyz`, `/status` and Prometheus `/metrics`. To scrape it, bind it to a reachable address with a status token and TLS; see `self_monitoring` in the config reference. The engine also stores its own health on a built-in `monitor` device in the platform tenant.

### Rotating the key-encryption key

[ADR-0006](../adr/0006-credential-encryption.md) designs rotation as: add a second key, make it active, restart, re-wrap with `monitor admin rewrap-credentials`, then remove the old key. That command does not exist in this release, so do not remove or replace `MONITOR_KEK` on a server that holds credentials. Keep the key safe instead (see Backups).

### Data retention

Metrics are kept in classes with a retention per level (raw samples, 5-minute rollups, hourly rollups). Inspect or change them from the command line, since standalone installs have no platform key for the API:

```sh
docker compose run --rm monitor admin retention
docker compose run --rm monitor admin retention standard --raw-days 14 --rollup-5m-days 90 --rollup-1h-days 730
```

The audit log is kept for one year by default (`platform.audit_retention`). Verify its integrity with `monitor admin verify-audit`.

### Reset everything

This deletes all data:

```sh
docker compose down -v
```

## 9. Troubleshooting

| Symptom | Cause and fix |
| --- | --- |
| `docker compose` complains about `configs` or `content` | Compose is older than 2.23. Upgrade it. |
| `set MONITOR_KEK in .env` (or another variable) | A required variable is empty. Fill it in. |
| `monitor` restarts, logs mention the database | A `MONITOR_*_PASSWORD` has characters other than letters and digits, or `POSTGRES_SUPERUSER_PASSWORD` was changed after the volume was created. Restore the original, or `down -v` on a new install. |
| `already bootstrapped` | Expected on a second bootstrap. Use `create-admin-key` if you lost the key. |
| `docker compose up -d` printed a platform key | You ran `up` for the whole stack before the standalone bootstrap. The installation now has a platform key and no tenant. Either `down -v` and start again as in section 4, or create the tenant through the API with that platform key: `PUT /v1/tenants/by-external-id/{id}`, then create an admin key with `POST /v1/api-keys` while sending `X-Tenant-External-ID` (see the [API guide](../api/README.md#platform-key-operations)). |
| Check output says the address is not allowed | The target is in a blocked range. Allow it in `allowed_target_networks` (section 3), or patch the tenant limits with a platform key. |
| `401 unauthenticated` | Wrong, revoked or expired key, or a missing `Bearer ` prefix. |
| `429 rate-limited` | The key exceeded `api_rate_per_minute` (default 1200 for tenants). Wait for `Retry-After` and slow down. |
| Audit log shows the proxy's IP for every request | `MONITOR_TRUSTED_PROXIES` does not include your proxy. |
| Webhook test fails with a network error | The receiver URL resolves into a blocked range, or the VM cannot reach it. Allow the range (section 3) and check outbound firewalls. |

## 10. Security checklist

- TLS on the public endpoint; ports 8443/9443/9090 not reachable from outside.
- `MONITOR_TRUSTED_PROXIES` set to your proxy only.
- `.env` readable by root only (`chmod 600 .env`), backed up with the KEK stored separately from database backups.
- Admin key in a password manager. Create `operator` or `read-only` keys for people and automation, and restrict them with `ip_allowlist` and `expires_at` where you can ([API guide](../api/README.md#api-keys)).
- `allowed_target_networks` limited to what you monitor.
- Database not published. Do not use `deploy/docker-compose.db-port.yml` in production.
- Host patched; Docker images updated regularly.
