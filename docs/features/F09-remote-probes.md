# F09: Remote probes

**Status:** Draft · **Phase:** MVP (enrollment, assignment, buffering, one probe per device); phase 2 (failover groups, reassignment) · **Related:** [ADR-0005](../adr/0005-probe-protocol.md)

## Summary

A probe is the `monitor` binary in probe mode, installed inside a remote or private network. It enrolls once, then pulls its assigned checks and pushes results, using only outbound connections.

## Behavior

### Lifecycle

1. Admin creates a probe: `POST /v1/probes` with name and `site_id` ([ADR-0014](../adr/0014-device-model-containment-dependencies-sites.md)). The response contains a one-time enrollment token (valid 24 h).
2. Operator installs the probe (static binary with a systemd unit, or container) and runs `monitor probe --core <url> --enroll-token <token>`.
3. The probe generates a key pair, sends a CSR with the token, and receives a client certificate. The key never leaves the probe host. The token is now spent.
4. The probe connects with mTLS, receives its assignment set and starts running checks.
5. Certificates renew automatically. `DELETE /v1/probes/{id}` revokes the probe.

**Preshared enrollment** replaces steps 1 and 2 for automated rollouts. The core's config lists reusable enrollment tokens, each bound to a tenant, with an optional default site, maximum number of probes, expiry and allowed source networks. A probe configured with such a token and a name enrolls on first start and is created in that tenant. A preshared token can only create new probes: it never re-enrolls a probe that has already enrolled, so a leaked token cannot take over an existing probe and the credentials assigned to it. Probes enrolled this way are not added to failover groups automatically. Every use is audited with the token's name.

### Assignment

- Devices have `probe_id`; all checks and collectors of the device run on that probe.
- Phase 2: `probe_group_id` instead of a single probe. The core assigns each device to one healthy probe in the group, spreading load, and reassigns when a probe goes silent.

### Health

- Heartbeat every 10 s with version, uptime, in-flight checks, check lag, buffer depth, clock offset.
- A probe is **silent** after 60 s without heartbeat: its checks go UNKNOWN, a probe incident opens, and child incidents are suppressed under it (the probe acts as the root of its devices in dependency suppression).
- Probe self-metrics are forwarded to the core and stored as metrics of a device representing the probe, so probe health is graphable like everything else.

### Local status server

The probe runs its own HTTPS server so an operator on the site network can check it directly. This matters most when the core is unreachable, because then the core's view of the probe (`/v1/probes/{id}/health`) is stale and the local server is the only source of truth.

- **Direction.** The core never connects to this server. The probe stays outbound-only toward the core; the server is for people and tools on the probe's own network, and can be disabled.
- **Listener.** Loopback by default (`127.0.0.1:9443`); the operator binds it to a LAN address to reach it from other machines. An optional source network allowlist restricts it further.
- **TLS.** Always on, except on a loopback address where it may be turned off. Without a configured certificate, the probe creates a self-signed one in its state directory on first start and logs its SHA-256 fingerprint. A certificate from the site's own CA can be configured instead. The probe's mTLS client certificate from the core's probe CA is not reused: it is a client certificate, and the core does not know the probe's local addresses.
- **Authentication.** `GET /healthz` is open. Everything else needs `Authorization: Bearer <status token>`. The token is read from a file or generated on first start into the state directory (mode 0600). It is local to the probe and grants read access to this server only.
- **No secrets.** Credentials, check configs and assignment contents beyond names and addresses are never returned. The same marker-string test as [F02](F02-devices-credentials-templates.md) covers this server.

| Endpoint | Content |
| --- | --- |
| `GET /healthz` | Process alive |
| `GET /readyz` | Enrolled, certificate valid, assignment received at least once |
| `GET /status` | JSON (HTML table when the client asks for `text/html`), see below |
| `GET /status/checks` | Per check: device, plugin, interval, last run, last status, duration, last error. Filters: `status`, `plugin`, `device` |
| `GET /metrics` | Prometheus exposition of the same values the heartbeat carries, plus runner metrics from [F04](F04-scheduler-and-runner.md) |

`/status` fields:

| Group | Fields |
| --- | --- |
| Identity | probe ID and name, site, version, protocol version, host name, uptime |
| Core connection | state (`connected`, `reconnecting`, `unenrolled`), core URL, connected since, last successful push, last heartbeat acknowledged, reconnect count, last error |
| Certificate | serial, not after, next renewal |
| Assignment | assignment version, devices, checks and collectors by plugin, last update |
| Runner | in-flight per pool, check lag p50/p99, executions in the last 5 minutes by status, skipped runs, timeouts |
| Buffer | results pending, bytes used and limit, oldest pending result age |
| Time | clock offset against the core (from the last heartbeat) |
| Host | CPU, memory, goroutines, open file descriptors |
| Recent errors | last 50 errors with time and source (connection, plugin, buffer) |

`monitor probe status` prints the same data from the local server using the token in the state directory, for use over SSH.

### Resource profile

Target: 1,000 checks/minute on 1 vCPU and 256 MB RAM; runs on small ARM hardware (Raspberry Pi class) for small sites.

### Privileges

ICMP needs raw sockets or unprivileged ICMP (`net.ipv4.ping_group_range`). The systemd unit grants `CAP_NET_RAW` only; the container image documents `--cap-add NET_RAW`. SNMP trap reception (phase 2) on UDP 162 needs `CAP_NET_BIND_SERVICE` or a port redirect.

## API

`/v1/probes` (create → returns token, list, get, delete, `{id}/rotate-token` to re-enroll), `/v1/probes/{id}/assignments`, `/v1/probes/{id}/health`.

## Acceptance criteria

- A probe behind NAT with only outbound HTTPS allowed enrolls and runs checks.
- Unplugging the probe's uplink for 30 minutes loses no results: they arrive after reconnection with original timestamps, update metrics, and do not open incidents retroactively.
- Stopping the probe opens one probe-down incident within 70 s, not one incident per device.
- An enrollment token cannot be used twice.
- A preshared token enrolls new probes up to its `max_probes`, and is rejected for a name that belongs to an already enrolled probe.
- With the uplink unplugged, `/status` on the probe shows `reconnecting`, the growing buffer and the age of the oldest pending result.
- Every endpoint except `/healthz` returns `401` without the status token, and no response contains a credential secret.

## Open questions

- Should a probe be able to run push ingestion (local MQTT broker at a remote site)? Proposed for phase 2: yes, ingester plugins can run on probes and forward like any other result.
