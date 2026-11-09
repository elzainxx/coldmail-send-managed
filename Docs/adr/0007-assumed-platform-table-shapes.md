# ADR-0007: Assumed platform-table shapes + pre-flight existence check (OD-8)

- **Status:** accepted (assumption to confirm before Phase 1 live)
- **Date:** 2026-07-22

## Context
The module reads two platform-owned tables read-only: `public.users` and
`public.job_items`. Their exact column shapes are owned by the platform team and not yet
confirmed. Phase 1 migrations will create FKs to `public.users(id)` and
`public.job_items(id)`.

## Decision (RULED)
Build a pre-flight check (`db/preflight/check_platform_tables.sql`) that verifies the two
tables EXIST and fails fast with a clear message if not. It checks EXISTENCE only, not column
shape (a shape check would break every time the platform legitimately adds a column). The
assumed shapes are documented for reference:
- `public.users(id uuid pk, email, is_active, created_at)`
- `public.job_items(id uuid pk, campaign_id, user_id, recipient_email, rendered_subject,
  rendered_body, reply_to, status, created_at)`

In dev, `deploy/pg-init/` seeds fixture tables of these shapes (clearly marked dev fixtures,
never the real platform schema) so the pre-flight passes locally.

## Consequences
Confirming the exact columns with the platform team is a **standing action item before Phase
1 migrations run against the real shared DB**. It does not block the Phase 0 or Phase 1
build. The FK-bearing migrations fail loudly (via pre-flight) rather than with an opaque
`relation does not exist` if the platform schema is absent.

## Links
`Docs/phase-0-groundwork.md` §5.7; open-decisions.md OD-8.
