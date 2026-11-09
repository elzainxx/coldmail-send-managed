# ADR-0001: Single repo, multiple services

- **Status:** accepted
- **Date:** 2026-07-22
- **Supersedes:** —

## Context
Locked deployment decision (build plan): a Go Brain + Node.js sender boxes + shared SQL
migrations, docker-compose locally, separate hosts in prod, scale by adding box containers.

## Decision
One repository. `services/brain/` (Go), `services/box/` (Node.js), `db/migrations/` as the
central self-tracking migration home, `tools/` for ops CLIs, `tests/harness/` for the
real-infra suite, `docs/` living docs, `Docs/` locked specs. The canonical
`docker-compose.yml` is at the repo root and is the single dev==CI stack.

## Consequences
Brain and box build/deploy independently (own manifests). The Brain/box security boundary is
structural: the box ships no DB driver and no KMS SDK. CODEOWNERS gates the migrations and
the operating rules.

## Links
CLAUDE.md Rule 6; `Docs/phase-0-groundwork.md` §1.
