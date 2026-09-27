# ADR-0006: Credential encryption

**Status:** Proposed · **Date:** 2026-09-27

## Context

The engine stores credentials for nearly every monitored device (SNMPv3, Redfish, IPMI, XAPI, vCenter, MQTT). The design requires encryption at rest with the key outside the database, write-only secrets in the API, decryption only in the worker that uses them, and a rotation plan.

## Options considered

1. **Encrypt with one static key from config.** Simple, but rotating the key means re-encrypting every secret in one step, and there is no path to an external KMS.
2. **PostgreSQL `pgcrypto`.** The key would travel to the database in queries. Rejected.
3. **Envelope encryption with a pluggable key provider.** Each secret gets its own data key; data keys are wrapped by a key-encryption key (KEK) held outside the database.

## Decision

Option 3.

- **Algorithm:** AES-256-GCM (Go standard library `crypto/aes` + `crypto/cipher`) for both data and key wrapping. Random 96-bit nonces.
- **Per secret:** a random 256-bit data key encrypts the secret. The stored row holds `ciphertext`, `nonce`, `wrapped_dek`, `kek_id`.
- **Associated data:** `tenant_id || credential_id || credential_type` is passed as GCM additional data, so an attacker with database write access cannot move a ciphertext to another tenant or credential and have it decrypt.
- **Key provider interface** (`Wrap(dek) / Unwrap(wrapped, kekID)`):
  - MVP: `file` provider — KEK read from a file or environment variable at startup, base64, 32 bytes. Multiple KEKs can be configured; the newest encrypts, all decrypt.
  - Later: HashiCorp Vault / OpenBao Transit, and cloud KMS providers.
- **Rotation:** adding a new KEK and running `monitor admin rewrap-credentials` re-wraps data keys only; secrets themselves are not re-encrypted. The old KEK can be removed when no row references its `kek_id`.
- **Where decryption happens:** only in the `runner` role (core or probe) at check time, and in the `api` role for `POST /credentials/{id}/test`. Decrypted values live in memory only and are never logged, returned by the API, or written to audit events (audit records only "secret changed").
- **Probe CA key** ([ADR-0005](0005-probe-protocol.md)) and **webhook signing secrets** ([ADR-0009](0009-webhook-delivery.md)) use the same mechanism.

## Consequences

- Stealing a database backup alone does not expose credentials.
- Losing every KEK makes all stored credentials unrecoverable; installation docs must cover KEK backup.
- A compromised runner host exposes the credentials it uses; least-privilege device accounts (design section 8) limit the damage.
