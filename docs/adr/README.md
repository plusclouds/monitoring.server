# Architecture decision records

Each ADR records one infrastructure or technology decision: the context, the options considered, what was chosen and what it costs. The [design document](../monitoring%20server%20design.md) records *what* the system does; ADRs record *how* it is built and why.

All records below are **Proposed**. They become **Accepted** once reviewed. To change an accepted decision, write a new ADR that supersedes it rather than editing the old one.

| ADR | Decision | Status |
| --- | --- | --- |
| [0001](0001-single-binary-with-roles.md) | One Go binary with switchable roles; probe is the same binary | Proposed |
| [0002](0002-postgresql-as-the-only-infrastructure-dependency.md) | PostgreSQL is the only required dependency; no message broker or cache in the MVP | Proposed |
| [0003](0003-metrics-storage-layout.md) | Metrics layout in plain PostgreSQL: series table, grouped raw samples, narrow rollups, partitioned by retention class and day | Proposed |
| [0004](0004-compiled-in-plugin-registry.md) | Plugins are compiled into the binary and registered at init; external `exec` plugins later | Proposed |
| [0005](0005-probe-protocol.md) | Probes talk to the core over Connect (gRPC-compatible) with mTLS, outbound only, with a local result buffer | Proposed |
| [0006](0006-credential-encryption.md) | Envelope encryption with AES-256-GCM and a pluggable key provider | Proposed |
| [0007](0007-api-spec-first-openapi.md) | Spec-first OpenAPI 3.1 with generated server stubs; API keys; UUIDv7 public IDs | Proposed |
| [0008](0008-tenant-isolation.md) | Tenant isolation in the application layer plus PostgreSQL row-level security | Proposed |
| [0009](0009-webhook-delivery.md) | Transactional outbox and Standard Webhooks signing for notifications | Proposed |
| [0010](0010-dependency-license-policy.md) | Dependency license allowlist enforced in CI | Proposed |
| [0011](0011-toolchain-and-repository-layout.md) | Toolchain, repository layout and engineering conventions | Proposed |
| [0012](0012-plusclouds-identity-and-external-ids.md) | PlusClouds owns accounts and users; the engine keeps a minimal mirror; external IDs on every linkable resource | Proposed |
| [0013](0013-embedded-mqtt-broker.md) | Embedded MQTT broker (mochi-mqtt) in the ingest role; external-broker client mode kept | Proposed |
| [0014](0014-device-model-containment-dependencies-sites.md) | Devices form a containment tree; dependencies are a separate graph; sites are a table; collector parts are objects, not devices | Proposed |

Use [0000-template.md](0000-template.md) for new records.
