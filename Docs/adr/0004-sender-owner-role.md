# ADR-0004: Dedicated non-superuser sender_owner role (OD-13)

- **Status:** accepted
- **Date:** 2026-07-22

## Context
`FORCE ROW LEVEL SECURITY` does not constrain a superuser or the table owner if that owner is
a superuser. Without a dedicated owner, `FORCE RLS` on `sender.accounts` is decorative, and
credential secrecy rests solely on ciphertext.

## Decision (RULED)
Create `sender_owner` — **NOLOGIN, non-superuser** — as the OWNER of the `sender` schema and
all its objects. The app role `sender_module` is **LOGIN**, holds only the enumerated grants,
and never owns anything. Migrations create objects as/for `sender_owner` (the bootstrap
migration sets ownership); the runner connects as an admin/owner-capable role (OD-1) and
assigns ownership to `sender_owner`.

## Consequences
`FORCE RLS` on `sender.accounts` genuinely enforces against `sender_module`. The migration
admin credential (OD-1) is distinct from `sender_module` and sourced out-of-band. A real
superuser still bypasses RLS — but the runtime app role does not, which is the property that
matters. Credential secrecy still also rests on encryption (defense in depth).

## Links
CLAUDE.md Rule 6; `Docs/phase-0-groundwork.md` §5.8; open-decisions.md OD-1, OD-13.
