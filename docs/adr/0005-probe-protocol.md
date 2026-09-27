# ADR-0005: Probe protocol

**Status:** Proposed · **Date:** 2026-09-27

## Context

Remote probes run checks inside private networks. The design requires that probes only make outbound connections, enroll with a one-time token and then use mutual TLS, pull their assigned checks, and push results back. Probes must keep working through short core outages and fail over to another probe when they go silent.

## Options considered

1. **REST/JSON over HTTPS**, reusing the public API. Easy to debug, but results are high-volume and streaming assignment updates would need long polling.
2. **gRPC (`google.golang.org/grpc`)**. Efficient, streaming, typed with protobuf. Needs HTTP/2 end to end, which some corporate proxies break.
3. **Connect (`connectrpc.com/connect`)**. Same protobuf services, speaks the gRPC protocol and a plain HTTP/1.1-compatible protocol from one handler, and works with the standard `net/http` stack. Apache 2.0.

## Decision

Option 3, with protobuf definitions in `proto/probe/v1`.

| RPC | Type | Purpose |
| --- | --- | --- |
| `Enroll` | unary, token-authenticated | Exchange a one-time token and a CSR for a client certificate signed by the core's probe CA |
| `WatchAssignments` | server stream (long-poll fallback over HTTP/1.1) | Full assignment set on connect, then versioned diffs; includes check configs and the credentials those checks need |
| `PushResults` | unary, batched | Up to 1,000 results or 1 s per call; each result carries its own timestamp and a sequence number |
| `Heartbeat` | unary, every 10 s | Probe version, load, buffer depth, clock offset |

Rules:

- **Probes schedule locally.** The assignment set is the probe's shard: the probe runs the same runner as the core, so checks keep running when the core is unreachable.
- **Local buffer.** Results that cannot be pushed go to an on-disk ring buffer (default 512 MB) and are sent oldest-first after reconnect. The engine treats results older than 2× the check interval as late: they are stored as metrics but do not open or resolve incidents.
- **Probe CA.** The core keeps its own small CA for probe certificates (private key encrypted with the key provider from [ADR-0006](0006-credential-encryption.md)). Certificates are valid for 30 days and renewed automatically at 2/3 of their lifetime. Revoking a probe deletes its registration; the core rejects its certificate by serial on the next call.
- **Credentials on probes** are held only in memory and only for assigned checks.
- **Silence and failover.** If a probe misses heartbeats for 60 s (configurable), its checks go to UNKNOWN, a probe-down incident opens with those checks suppressed underneath it, and, if the probe is in a failover group, the core reassigns its checks to the next healthy probe in the group.
- **Versioning.** The protocol version is in every request header. The core accepts the current and previous minor version and answers others with a clear upgrade error.

## Consequences

- Probes work behind NAT and restrictive firewalls, including proxies that only speak HTTP/1.1.
- The core must run and protect a small CA; this is new operational surface.
- Protobuf adds a code-generation step (`buf generate`).
- Clock skew on probes corrupts timestamps; the heartbeat reports the offset and the core raises a warning above 2 s. Probes should run NTP, as the design already requires.
