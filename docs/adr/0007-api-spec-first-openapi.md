# ADR-0007: Spec-first OpenAPI, API keys and identifiers

**Status:** Proposed · **Date:** 2026-09-27

## Context

The API is the only way to manage the engine, and the OpenAPI spec is the source of truth for clients and documentation. The PlusClouds panel, scripts, Terraform (later) and customers' own tools all depend on it, so it must stay consistent and versioned.

## Options considered

1. **Code-first:** write handlers, generate the spec from annotations. Fast to start; the spec drifts and is only as good as the annotations.
2. **Spec-first:** write `api/openapi.yaml`, generate Go server interfaces and types, implement the interfaces. The compiler catches drift.

## Decision

Spec-first.

- **Spec:** OpenAPI 3.1 in `api/openapi.yaml`, split into files per resource and bundled in CI. Linted with Redocly CLI or Spectral against a rule set (naming, pagination, error shape).
- **Generation:** `oapi-codegen` (Apache 2.0) produces a strict server interface and models for the standard `net/http` router. Generated code is committed so builds do not need the generator.
- **Router:** Go standard library `net/http.ServeMux` with method and path patterns. Middleware is plain `func(http.Handler) http.Handler`.
- **Errors:** RFC 9457 Problem Details (`application/problem+json`) with a stable `type` URI per error.
- **Pagination:** opaque cursor (`?cursor=…&limit=…`, max 500), `next_cursor` in the response.
- **Idempotency:** `Idempotency-Key` header on every `POST`; keys and responses are stored for 24 h per tenant. Resources with external IDs also get `PUT /v1/<resource>/by-external-id/{external_id}` for upserts ([ADR-0012](0012-plusclouds-identity-and-external-ids.md)).
- **Identifiers:** UUIDv7 for every public ID (time-ordered, index friendly, safe to expose). Internal high-volume tables (metric series and groups) use `bigint` and are never exposed as primary identifiers.
- **Authentication:**
  - API keys in the format `mon_<8-char prefix>_<32-byte random, base62>`. Stored as SHA-256 of the full key (keys are high-entropy, so a slow hash is not needed) plus the prefix for lookup. Shown once at creation.
  - Each key has a tenant, a role (`read-only`, `operator`, `admin`), optional expiry and optional IP allowlist.
  - Sent as `Authorization: Bearer <key>`.
  - A **platform key** type, owned by no tenant, provisions tenants and users and acts on any tenant for a named user via `X-Tenant-External-ID` and `X-Actor-External-ID`. Only the PlusClouds API (leo4) uses it. Every use is audited with both identities. See [ADR-0012](0012-plusclouds-identity-and-external-ids.md).
- **Rate limits:** token bucket per key, in memory per API node. Limits are soft across nodes; acceptable for protecting the service, not for billing.
- **Event stream:** `GET /v1/events` as Server-Sent Events, filtered to the key's tenant, fed by `LISTEN/NOTIFY` ([ADR-0002](0002-postgresql-as-the-only-infrastructure-dependency.md)). Supports `Last-Event-ID` for a short replay window (last 10 minutes).

## Consequences

- API changes start as a spec change, which makes them reviewable by the panel team before any code is written.
- The generator shapes the Go code; if `oapi-codegen` cannot express something, we write that handler manually and keep it in the spec.
- PlusClouds calls the engine server to server with the platform key ([ADR-0012](0012-plusclouds-identity-and-external-ids.md)). Forwarded user tokens (JWT) remain a phase 3 option if the panel ever calls the engine from the browser.
