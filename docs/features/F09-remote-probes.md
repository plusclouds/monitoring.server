# F09: Remote probes

**Status:** Draft · **Phase:** MVP (enrollment, assignment, buffering, one probe per device); phase 2 (failover groups, reassignment) · **Related:** [ADR-0005](../adr/0005-probe-protocol.md)

## Summary

A probe is the `monitor` binary in probe mode, installed inside a remote or private network. It enrolls once, then pulls its assigned checks and pushes results, using only outbound connections.

## Behavior

### Lifecycle

1. Admin creates a probe: `POST /v1/probes` with name and site. The response contains a one-time enrollment token (valid 24 h).
2. Operator installs the probe (static binary with a systemd unit, or container) and runs `monitor probe --core <url> --enroll-token <token>`.
3. The probe generates a key pair, sends a CSR with the token, and receives a client certificate. The key never leaves the probe host. The token is now spent.
4. The probe connects with mTLS, receives its assignment set and starts running checks.
5. Certificates renew automatically. `DELETE /v1/probes/{id}` revokes the probe.

### Assignment

- Devices have `probe_id`; all checks and collectors of the device run on that probe.
- Phase 2: `probe_group_id` instead of a single probe. The core assigns each device to one healthy probe in the group, spreading load, and reassigns when a probe goes silent.

### Health

- Heartbeat every 10 s with version, uptime, in-flight checks, check lag, buffer depth, clock offset.
- A probe is **silent** after 60 s without heartbeat: its checks go UNKNOWN, a probe incident opens, and child incidents are suppressed under it (the probe acts as the root of its devices in dependency suppression).
- Probe self-metrics are forwarded to the core and stored as metrics of a device representing the probe, so probe health is graphable like everything else.

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

## Open questions

- Should a probe be able to run push ingestion (local MQTT broker at a remote site)? Proposed for phase 2: yes, ingester plugins can run on probes and forward like any other result.
