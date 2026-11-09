# ADR-0006: Brain is the sole decryptor; box has no DB/KMS

- **Status:** accepted
- **Date:** 2026-07-22

## Context
Load-bearing security property (CLAUDE.md Rule 6): KMS Decrypt is Brain-only; the box holds
credentials in memory only, zeroed on exit, and has no DB or KMS access. The IAM separation
(platform/operators hold no Decrypt) is not code-checkable — it is a deployment sign-off.

## Decision
Structural + verified enforcement in Phase 0:
- The box service ships **no DB driver and no KMS SDK** (empty `dependencies` in
  `package.json`); this is a review-blocking invariant.
- Container mounts are least-privilege: the box mounts ONLY its own mTLS leaf; the Brain
  alone mounts the KMS key material. A harness test asserts the box **cannot read** the KMS
  key file.
- The dev KMS is a local key file loaded only by the Brain; prod is a real cloud KMS whose
  Decrypt principal is the Brain role only (verified at deploy sign-off, item S15).

## Consequences
A box compromise cannot decrypt credentials — it never holds the key and cannot reach it.
Phase 0 proves the containment (the box-can't-read-KMS test); real Decrypt is Phase 2.

## Links
CLAUDE.md Rule 6; `Docs/phase-0-groundwork.md` §6.1, §4.5; open-decisions.md OD-14.
