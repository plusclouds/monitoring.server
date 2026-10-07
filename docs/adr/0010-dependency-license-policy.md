# ADR-0010: Dependency license policy

**Status:** Proposed · **Date:** 2026-09-27

## Context

The engine is MIT-licensed and will be embedded in the PlusClouds panel and run by customers. The design says no dependency may force a restrictive license on users. Go builds static binaries, so every linked dependency is distributed with the binary.

## Decision

- **Allowed for linked Go dependencies:** MIT, BSD-2-Clause, BSD-3-Clause, Apache-2.0, ISC, MPL-2.0 (file-level copyleft, compatible with distribution as long as MPL files stay MPL), EPL-2.0 only when dual-licensed with a permissive option (for example Eclipse Paho's EDL-1.0).
- **Not allowed:** GPL, LGPL (static linking makes compliance awkward), AGPL, SSPL, BUSL, Elastic License, Commons Clause, and any "source-available" license.
- **Runtime services** the engine talks to over the network (PostgreSQL, TimescaleDB, ClickHouse, Grafana) are not linked and are covered by the storage decision in the design: each must have a free option that users can run without restrictions on their own use.
- **Enforcement:** CI runs `google/go-licenses check ./... --allowed_licenses=…` on every pull request and fails on anything outside the list. A `THIRD_PARTY_NOTICES` file is generated at release.

### Candidate libraries (to verify at adoption)

| Area | Library | License (as published; verify at adoption) |
| --- | --- | --- |
| PostgreSQL | `jackc/pgx` | MIT |
| SNMP | `gosnmp/gosnmp` | BSD-2-Clause |
| Redfish | `stmcginnis/gofish` | BSD-3-Clause. Not adopted: `redfish.health` uses its own small read-only client (M4) |
| ICMP | `prometheus-community/pro-bing` | MIT |
| DNS | `miekg/dns` | BSD-3-Clause |
| RTSP | `bluenviron/gortsplib` | MIT. Adopted as v5 (v4 is deprecated), with `bluenviron/mediacommon` (MIT) and `pion` RTP/RTCP/SDP (MIT) |
| VMware | `vmware/govmomi` | Apache-2.0 |
| MQTT client (external-broker mode) | `eclipse/paho.golang` | EPL-2.0 / EDL-1.0 |
| MQTT broker (embedded) | `mochi-mqtt/server` | MIT |
| Probe RPC | `connectrpc.com/connect` | Apache-2.0 |
| API codegen | `oapi-codegen/oapi-codegen` | Apache-2.0 |
| JSON Schema | `invopop/jsonschema` | MIT |
| Migrations | `pressly/goose` | MIT |
| Self-metrics | `prometheus/client_golang` | Apache-2.0 |
| IPMI | to be selected (candidates: `bougou/go-ipmi`) | verify |
| Proxmox | to be selected | verify |

## Consequences

- A useful library with a restrictive license means writing our own code or finding an alternative.
- License changes upstream are caught at the next dependency update rather than after release.
