# ISO/IEC 27001 control mapping

**Status:** Draft · **Last updated:** 2026-09-28 · **Related:** [Standards, certifications and compliance](README.md)

## Purpose

ISO/IEC 27001 certifies an organization's information security management system, not software (see [why](README.md#why-the-major-certifications-do-not-apply-to-the-engine)). This document maps the Annex A controls of ISO/IEC 27001:2022 that the engine's design affects to the feature or ADR that covers them, and lists the gaps. It serves two readers:

- **PlusClouds**, when the hosted engine is inside its ISMS scope: this is the technical evidence behind the Statement of Applicability.
- **Customers running the engine themselves**: this shows which of their controls the engine supports and which stay with them.

Controls that are purely organizational (policies, HR security, physical security, supplier agreements) are not listed. They belong to whoever operates the engine.

## Legend

- **Covered:** the design already meets the control's technical side.
- **Partial:** covered in part; the gap column says what is missing.
- **Gap:** not in the design yet.
- **Operator:** the engine cannot meet it; the organization running it must.

## Control mapping

### Access control and identity

| Control | Status | How the engine covers it | Gap and owner |
| --- | --- | --- | --- |
| A.5.15 Access control | Covered | Tenant roles `read-only`, `operator`, `admin`; platform key for PlusClouds ([F01](../features/F01-tenancy-auth-audit.md)) | — |
| A.5.16 Identity management | Covered | Users are PlusClouds users, mirrored by UUID; standalone installs use tenant API keys ([ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)) | — |
| A.5.17 Authentication information | Covered | API keys stored as SHA-256 hashes, shown once; MQTT passwords stored as Argon2id ([ADR-0007](../adr/0007-api-spec-first-openapi.md), [ADR-0013](../adr/0013-embedded-mqtt-broker.md)) | — |
| A.5.18 Access rights | Partial | `api_keys.expires_at` and `last_used_at` exist ([F01](../features/F01-tenancy-auth-audit.md)) | No way to review access in bulk. Add a stale-key listing (unused for N days, or no expiry) and a tenant-level maximum key lifetime. Owner: F01 |
| A.8.2 Privileged access rights | Partial | Platform key restricted by IP allowlist and held only in leo4 secrets ([ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)) | A leaked platform key can act as anyone in any tenant. Add an alert on any platform key use from outside the allowlisted sources, and consider short-lived credentials or mTLS for the platform caller. Owner: ADR-0012, F11 |
| A.8.3 Information access restriction | Covered | Tenant isolation in the application plus PostgreSQL row-level security ([ADR-0008](../adr/0008-tenant-isolation.md)) | — |
| A.8.5 Secure authentication | Covered | Keys and mTLS for machines; interactive user login happens in PlusClouds, which owns MFA | Standalone installs have no interactive login, so there is no MFA gap in the engine. Operator: protect admin keys |
| A.8.18 Use of privileged utility programs | Gap | `monitor admin bootstrap` and `monitor admin rewrap-credentials` run outside the API | These commands must write audit events like API writes do. Owner: F01, ADR-0006 |

### Logging and monitoring

| Control | Status | How the engine covers it | Gap and owner |
| --- | --- | --- | --- |
| A.8.15 Logging | Partial | Audit event for every write, in the same transaction, with actor, key, before and after (secrets masked), source IP; the app role cannot `UPDATE` or `DELETE` audit rows ([F01](../features/F01-tenancy-auth-audit.md)) | (1) A database superuser can still change rows: add a hash chain (each event stores the hash of the previous one per tenant) and a `monitor admin verify-audit` command. (2) Sensitive reads are not logged: add `config.export`, `credential.test`, audit log queries and failed API authentication. (3) SIEM export is listed as "later": move it to phase 2 so logs can be kept off the host. Owner: F01 |
| A.8.16 Monitoring activities | Partial | This is what the engine does. Self-metrics already include ingest auth failures and rate-limited API requests ([F11](../features/F11-self-monitoring.md)) | F11 has metrics but no alerts for security signals. Add built-in alerts for: failed API authentication spikes, rate-limit spikes per key, MQTT ACL denials, platform key used from an unexpected IP, reuse of a revoked probe certificate. Owner: F11 |
| A.8.17 Clock synchronization | Partial | Probes report clock offset; the core warns above 2 s ([ADR-0005](../adr/0005-probe-protocol.md)) | Core nodes should report their own offset against the database clock, with the same alert. Operator: run NTP on all nodes. Owner: F11 |

### Cryptography and data protection

| Control | Status | How the engine covers it | Gap and owner |
| --- | --- | --- | --- |
| A.8.24 Use of cryptography | Partial | Envelope encryption with the KEK outside the database, KEK rotation by rewrapping ([ADR-0006](../adr/0006-credential-encryption.md)); mTLS for probes; signed webhooks ([ADR-0009](../adr/0009-webhook-delivery.md)) | No written TLS policy. State it in one place: TLS 1.2 minimum, TLS 1.3 preferred, one cipher list for the API, MQTT on 8883, outgoing webhooks and probes. Owner: new section in design section 8 |
| A.8.11 Data masking | Covered | Secrets are write-only; audit `before`/`after` store `"[changed]"` for secrets ([F01](../features/F01-tenancy-auth-audit.md), [F02](../features/F02-devices-credentials-templates.md)) | — |
| A.8.12 Data leakage prevention | Covered | Marker-string test: no API response, log line or audit event contains a secret ([F02](../features/F02-devices-credentials-templates.md)) | — |
| A.8.10 Information deletion | Covered | Tenant soft delete, purge after a platform-level grace period; metric retention by partition drop ([ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md), [F07](../features/F07-metrics-store-and-retention.md)) | Operator: set and document the grace period and audit retention |
| A.5.34 Privacy and protection of PII | Covered | User mirror holds no email, phone or other personal data ([F01](../features/F01-tenancy-auth-audit.md)) | The audit log stores source IPs. Operator: set audit retention in line with KVKK/GDPR |

### Network and operations

| Control | Status | How the engine covers it | Gap and owner |
| --- | --- | --- | --- |
| A.8.20 Network security | Partial | Ingestion on separate listeners from the API; probes connect outbound only over mTLS; SSRF guard on HTTP checks and webhooks ([F01](../features/F01-tenancy-auth-audit.md), [ADR-0005](../adr/0005-probe-protocol.md)) | The legacy plain MQTT listener on 1883 ([F12](../features/F12-fixlean-collector-replacement.md)) is unencrypted and uses a shared password. Record it in the risk register as a time-limited exception with an end date for each migrating tenant. Owner: F12, operator |
| A.8.22 Segregation of networks | Covered | Self-metrics on a separate internal listener; per-tenant `allowed_target_networks` | Operator: place the management API and ingestion listeners in separate network zones |
| A.8.6 Capacity management | Covered | Self-metrics for lag, buffers and pools; load-test gates ([F11](../features/F11-self-monitoring.md), [ADR-0013](../adr/0013-embedded-mqtt-broker.md)) | — |
| A.8.13 Information backup | Partial | Stolen database backups do not expose credentials; installation docs must cover KEK backup ([ADR-0006](../adr/0006-credential-encryption.md)); config export exists | No documented backup and restore procedure for the engine itself. Write one (PostgreSQL backups, KEK backup, probe CA key) and test restores in CI or in a periodic drill. Owner: operations guide, operator |
| A.8.14 Redundancy | Partial | Stateless API and runner nodes; probe failover in phase 2 | Operator: PostgreSQL high availability |
| A.8.9 Configuration management | Covered | Declarative config import with dry run, full export, audit of every change | — |
| A.8.32 Change management | Covered | Every configuration change is audited; OpenAPI breaking-change diff in CI ([ADR-0011](../adr/0011-toolchain-and-repository-layout.md)) | Operator: change process for upgrades |

### Secure development and supply chain

| Control | Status | How the engine covers it | Gap and owner |
| --- | --- | --- | --- |
| A.8.4 Access to source code | Gap | Public repository | Branch protection on `master` with required review and required CI checks; restricted maintainers. Owner: repository settings |
| A.8.8 Management of technical vulnerabilities | Partial | `govulncheck` in CI ([ADR-0011](../adr/0011-toolchain-and-repository-layout.md)) | `SECURITY.md`, private reporting, advisory process, automated dependency updates (see [vulnerability handling](README.md#vulnerability-handling-needed-for-every-path-above)). Owner: ADR-0011 |
| A.8.19 Installation of software on operational systems | Gap | Releases built with GoReleaser | Sign binaries and images (`cosign`), publish SLSA provenance and SPDX SBOMs so operators can verify what they install. Owner: ADR-0011 |
| A.8.25 Secure development life cycle | Partial | Spec-first API, ADRs, lint and tests in CI | Add threat modeling to feature specs that add a listener or handle credentials (F08, F09, F12). Owner: feature spec template |
| A.8.26 Application security requirements | Covered | Design section 8 and the per-feature acceptance criteria | — |
| A.8.27 Secure system architecture | Covered | Least privilege on devices, isolated ingestion, outbound-only probes, encrypted credentials | — |
| A.8.28 Secure coding | Partial | `golangci-lint` with a committed config | Enable `gosec` in the `golangci-lint` config. Owner: ADR-0011 |
| A.8.29 Security testing | Gap | Unit, integration and protocol simulator tests | Fuzz tests for every parser that accepts untrusted input (MQTT payload profiles, HTTP push bodies, SNMP traps, probe messages); an external penetration test before phase 3. Owner: F03, F08, F12 |
| A.8.30 Outsourced development | Operator | — | Contributions from outside go through the same review and CI |
| A.5.21 ICT supply chain | Covered | Dependency license policy and allowlist ([ADR-0010](../adr/0010-dependency-license-policy.md)) | SBOM per release (see A.8.19) |

## Clauses 4–10 and organizational controls

These are the organization's work and cannot be met by the engine: ISMS scope, risk assessment and treatment, Statement of Applicability, policies, awareness, internal audit, management review, supplier management, incident response process (A.5.24–A.5.28), and business continuity (A.5.29–A.5.30).

The engine's own risks to enter in the risk register:

| Risk | Treatment in the design |
| --- | --- |
| Platform key leak gives access to every tenant | IP allowlist, secrets storage in leo4, alerting on unexpected use (gap above) |
| Loss of every KEK makes all stored credentials unrecoverable | KEK backup in installation docs ([ADR-0006](../adr/0006-credential-encryption.md)) |
| Legacy plain MQTT listener during FixLean migration | Off by default, enabled per migrating tenant, time-limited exception ([F12](../features/F12-fixlean-collector-replacement.md)) |
| Engine holds credentials for nearly every device | Encryption at rest, write-only API, read-only device accounts (design section 8) |
| Monitoring stops without anyone noticing | Dead man's switch to an external endpoint ([F11](../features/F11-self-monitoring.md)) |

## Gap summary by owner

**Update 2026-09-28:** every engine-side gap below is now written into its owner document (F01, F11, ADR-0011, design section 8, F03, F08, F12). The control status changes to **Covered** once the feature is implemented and tested. Operator items stay with the operator.

| Owner | Items |
| --- | --- |
| F01 | Audit hash chain and verify command; audit sensitive reads and failed authentication; SIEM export in phase 2; stale-key review and maximum key lifetime; audit events from `monitor admin` commands |
| F11 | Security alerts (failed auth, rate limits, MQTT ACL denials, platform key misuse, revoked probe certificates); core node clock offset |
| ADR-0011 | `gosec`, `SECURITY.md`, dependency updates, signed releases, SLSA provenance, SPDX SBOM, branch protection |
| Design section 8 | Written TLS policy |
| F03, F08, F12 | Fuzz tests for untrusted input parsers |
| Operations guide | Backup and restore procedure with tested restores |
| Operator | NTP, network zones, PostgreSQL HA, retention settings, risk register, all organizational controls |
