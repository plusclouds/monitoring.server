# Standards, certifications and compliance

**Status:** Draft · **Last updated:** 2026-09-28 · **Related:** [ISO/IEC 27001 control mapping](iso27001-control-mapping.md), [design section 8](../monitoring%20server%20design.md#8-security), [ADR-0010](../adr/0010-dependency-license-policy.md), [ADR-0011](../adr/0011-toolchain-and-repository-layout.md)

## Summary

The engine is software, and most well-known certifications (ISO/IEC 27001, SOC 2, ISO 9001, PCI DSS and others) certify an **organization** or a **service**, not a piece of software. No one can certify this repository against them, and we should never claim that the engine "is ISO 27001 certified" or "is SOC 2 compliant".

What matters instead:

1. **Organizations that run the engine get audited.** PlusClouds, when it hosts the engine as a service, and customers who run it themselves. The engine should make their audits easy: a tamper-evident audit log, access reviews, encryption, secure defaults, evidence exports.
2. **The engine implements standards.** Data formats, protocols and units follow published standards, so its output fits other systems and survives audits.
3. **The project can earn project-level attestations.** A few programs do assess open-source projects themselves (OpenSSF Best Practices, SLSA, OpenChain). These fit, and they cost little.
4. **Some regulations apply to the product.** The EU Cyber Resilience Act is a regulation, not a certification, but it can place obligations on whoever supplies the software commercially.

## Organization certifications vs product certifications

| Kind | What is assessed | Examples | Can this engine hold it? |
| --- | --- | --- | --- |
| Management system certification | An organization's processes, within a declared scope | ISO/IEC 27001, 27701, 20000-1, 22301, ISO 9001 | No. PlusClouds can, with the hosted engine inside its scope |
| Service attestation | A service organization's controls, reported on by an auditor | SOC 2, CSA STAR, C5 | No. PlusClouds can, for the hosted service |
| Product security certification | One specific product version against a protection profile or requirement set | Common Criteria (ISO/IEC 15408), FIPS 140-3, IEC 62443-4-2 | In theory yes, but not planned (see below) |
| Project attestation | An open-source project's practices | OpenSSF Best Practices Badge, SLSA, OpenChain self-certification | Yes, and recommended |

## Why the major certifications do not apply to the engine

| Certification | What it certifies | Why it is not a target for the engine | Where it does matter |
| --- | --- | --- | --- |
| **ISO/IEC 27001** | An information security management system (ISMS): scope, risk assessment, Statement of Applicability, Annex A controls, internal audit, management review | Certifies an organization's management system. Software has no risk treatment process, policies or management review, so there is nothing for an auditor to certify | PlusClouds, if the hosted engine is inside its ISMS scope; customers running it. The engine is also evidence for control A.8.16 (monitoring activities). See the [control mapping](iso27001-control-mapping.md) |
| **ISO/IEC 27017 / 27018** | Cloud security controls / protection of personal data in public clouds, as extensions of a 27001 ISMS | Same as 27001: they extend an organization's ISMS | PlusClouds as a cloud provider. The engine helps here by keeping no personal data in its user mirror ([F01](../features/F01-tenancy-auth-audit.md)) |
| **ISO/IEC 27701** | A privacy information management system | Organization-level. The engine processes very little personal data (PlusClouds UUIDs, source IPs in the audit log) | PlusClouds, alongside KVKK and GDPR |
| **SOC 2** (AICPA) | A CPA firm's report on a service organization's controls against the Trust Services Criteria, for a point in time (Type I) or a period (Type II) | Assesses a running service and the company behind it, not a codebase. A self-hosted install has no "service organization" | PlusClouds, if it sells the hosted engine to customers who ask for SOC 2 reports |
| **ISO 9001** | A quality management system | Organization-level; says nothing about the software itself | PlusClouds as a company |
| **ISO/IEC 20000-1** | An IT service management system | Organization-level. The engine's incidents are monitoring incidents, not ITSM records | PlusClouds operations. Aligning terms (event, incident, acknowledge, resolve) makes later integration with a service desk easier |
| **ISO 22301** | A business continuity management system | Organization-level | PlusClouds operations. The engine supports it through HA, backups and the dead man's switch ([F11](../features/F11-self-monitoring.md)) |
| **PCI DSS** | Entities that store, process or transmit cardholder data | The engine never handles cardholder data | If a customer deploys the engine inside or connected to a cardholder data environment, it becomes an in-scope system component (security-impacting). The customer's assessor then looks at its access control, logging and hardening |
| **HIPAA** | Not a certification; a US law for covered entities and business associates | The engine is not designed to hold protected health information, and nothing should put PHI into metric labels or device names | Only if a US healthcare customer runs it, as part of their own compliance |
| **CSA STAR, BSI C5** | Cloud service providers | Organization and service level | PlusClouds, if customers ask for them |
| **Common Criteria** (ISO/IEC 15408) | One product version, evaluated by an accredited lab against a protection profile | Evaluations take many months, cost a lot, and cover a single version, which does not fit a fast-moving open-source project. There is no widely used protection profile for monitoring engines. Mostly required for government procurement | Revisit only if a public-sector contract requires it |
| **FIPS 140-3** | Cryptographic modules | The engine does not implement cryptography. It uses Go's standard library, and Go ships a native cryptographic module that is being validated under FIPS 140-3 | If a customer needs FIPS mode, build with Go's FIPS 140-3 support (`GOFIPS140`) and point to the Go module's validation status, without a validation of our own. Check the current CMVP status before promising this |
| **IEC 62443-4-1 / 4-2** | Secure development lifecycle and security capabilities of industrial automation components | The engine monitors facility devices but is not an industrial control component, and MVP targets are IT infrastructure | Revisit when Modbus and BACnet (phase 2 and 3) bring industrial and OT customers who ask for it |
| **EN 18031 / Radio Equipment Directive** | Radio equipment | The engine is software without radio hardware | Not applicable |

### Short answer for customers

> The monitoring engine is open-source software, and certifications such as ISO/IEC 27001 and SOC 2 apply to organizations, not software. The engine is built to support your certification: every change is written to an audit log, credentials are encrypted at rest and never returned by the API, tenants are isolated down to the database, and all traffic is encrypted. When PlusClouds hosts the engine for you, it runs under PlusClouds' own security management system.

Only use the last sentence once PlusClouds has confirmed that the hosted engine is in scope of its certifications.

## Project-level attestations we should pursue

These assess the project itself, are free or cheap, and give customers verifiable evidence.

| Attestation | What it asks for | Work needed | When |
| --- | --- | --- | --- |
| [OpenSSF Best Practices Badge](https://www.bestpractices.dev/) (passing level) | Documented build, tests, vulnerability reporting process, secure development knowledge, static analysis | `SECURITY.md`, contribution guide, enable `gosec` in `golangci-lint`; most other criteria are met by [ADR-0011](../adr/0011-toolchain-and-repository-layout.md) | MVP |
| [OpenSSF Scorecard](https://scorecard.dev/) | Automated checks on the repository: branch protection, pinned actions, token permissions, signed releases | Run the Scorecard GitHub Action; fix findings | MVP |
| [SLSA](https://slsa.dev/) build level 2 or higher | Signed build provenance from a hosted build platform | GoReleaser with GitHub artifact attestations or `slsa-github-generator`; sign binaries and images with Sigstore `cosign` | Before the first public release |
| SBOM in SPDX (ISO/IEC 5962) | A machine-readable list of every component in each release | Generate an SPDX SBOM with GoReleaser (via `syft`) and attach it to every release and image | Before the first public release |
| OpenChain license compliance (ISO/IEC 5230) and security assurance (ISO/IEC 18974) | An organization's open-source license and vulnerability process | [ADR-0010](../adr/0010-dependency-license-policy.md) and `govulncheck` already cover most of the technical side. Conformance is claimed by PlusClouds as an organization, and self-certification is allowed | Phase 3, when offered to customers |

## Regulations that may apply to the product

Regulations are not certifications, but unlike most certifications they can apply to the software itself or to whoever supplies it. **Each item here needs legal review; this document does not replace it.**

- **EU Cyber Resilience Act (Regulation (EU) 2024/2847).** Applies to products with digital elements placed on the EU market. Free and open-source software developed or supplied outside a commercial activity is excluded, but the regulation can apply when PlusClouds supplies the engine commercially, for example bundled with a paid offering. Vulnerability and incident reporting obligations apply from 11 September 2026, and the main obligations from 11 December 2027. Open questions: whether PlusClouds is a manufacturer or an open-source steward for this engine, and whether the product falls into an Annex III category (for example, network management systems), which would raise the conformity assessment requirements. The vulnerability handling work in the next section is needed in either case.
- **NIS2 (Directive (EU) 2022/2555).** Applies to organizations (cloud and managed service providers among them), not to software. Customers subject to NIS2 will ask for the same evidence as for 27001.
- **KVKK and GDPR.** Apply to whoever processes personal data. The engine is designed to hold very little of it: the user mirror has no email or phone ([F01](../features/F01-tenancy-auth-audit.md)), tenant deletion purges data after a grace period ([ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)). The audit log keeps source IPs, which are personal data, so audit retention must be set and documented. LLM content capture ([F14](../features/F14-llm-monitoring.md)) is the exception: when a tenant turns it on, prompts and responses may contain personal data. It is off by default, redacted at ingestion, encrypted, and kept under its own retention, but the tenant becomes responsible for a lawful basis and for deletion requests.
- **Turkish public sector and critical infrastructure.** Customers in these sectors may follow the Presidential Digital Transformation Office's *Information and Communication Security Guide*. It is organization-level; the engine's audit log, encryption and access control provide the evidence they will ask for.

## Vulnerability handling (needed for every path above)

ISO/IEC 29147 (vulnerability disclosure) and ISO/IEC 30111 (vulnerability handling) are guidance standards, not certifications, but ISO 27001 (A.8.8), the OpenSSF badge, OpenChain 18974 and the CRA all expect what they describe:

- A `SECURITY.md` with a private reporting channel (GitHub private vulnerability reporting) and a response time target.
- An internal process: triage, fix, coordinated release, advisory (GitHub Security Advisories, with a CVE when warranted).
- Supported versions stated explicitly, with how long each release line receives security fixes.
- `govulncheck` in CI (already in [ADR-0011](../adr/0011-toolchain-and-repository-layout.md)) plus automated dependency updates.

## Standards the engine implements

These are followed in the code and apply regardless of who runs the engine. They should be settled before the OpenAPI spec is written ([ADR-0007](../adr/0007-api-spec-first-openapi.md)).

| Standard | Applies to |
| --- | --- |
| ISO 8601, RFC 3339 profile | Every timestamp in the API, webhooks, audit events and exports: UTC with a `Z` suffix. Durations in one form everywhere (seconds as integers, or ISO 8601 durations such as `PT30S`) |
| ISO/IEC 20922 (MQTT 3.1.1) | The embedded broker and MQTT client ([ADR-0013](../adr/0013-embedded-mqtt-broker.md)). MQTT 5 is an OASIS standard without an ISO equivalent |
| ISO/IEC 9834-8 / RFC 9562 (UUID) | All identifiers, including PlusClouds external IDs ([ADR-0012](../adr/0012-plusclouds-identity-and-external-ids.md)) |
| ISO/IEC 80000 and IEC 80000-13 (SI units, binary prefixes) | Metric units: store base units (bytes, seconds, bits per second, degrees Celsius); keep KiB and kB distinct in any display |
| ISO 3166-1 alpha-2 | Country or location fields on sites, probes and devices |
| ISO/IEC 10646 (UTF-8) | All text in the API, database and payloads |
| ISO/IEC 5962 (SPDX) | Release SBOMs |
| RFC 9457 | API errors ([ADR-0007](../adr/0007-api-spec-first-openapi.md)) |
| DMTF Redfish (DSP0266), SNMP (RFC 3411–3418), OpenAPI 3.1 | Protocols and API description |

## Open questions

- [ ] Does PlusClouds hold, or plan to obtain, ISO/IEC 27001 (and 27017/27018)? Will the hosted engine be inside that scope?
- [ ] Will PlusClouds offer SOC 2 reports for the hosted service, or only ISO certificates?
- [ ] Cyber Resilience Act: is PlusClouds a manufacturer or an open-source steward for this engine, and is the product in an Annex III category? Needs legal review before phase 3.
- [ ] Which release lines receive security fixes, and for how long?
