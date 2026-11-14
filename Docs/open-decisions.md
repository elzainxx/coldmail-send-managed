# Open decisions — the living ledger (module 2, `coldmail-send-managed`)

> **OWNER: MODULE 2.** This is the LIVING ledger — the one that grows. Do not confuse it with
> [`history/open-decisions.md`](./history/open-decisions.md), the **frozen** phase-0–10 ledger with the
> same file name one directory down: that one is provenance and is never edited; this one records every
> ruling in force **since the fork** and is where a new decision is filed.

**Status (2026-11-13): ZERO OPEN.** Every decision raised in this module is ruled and closed — the
phase-0–10 ledger ended empty on 2026-08-07 (R2-5, below), and every ruling made since is a row here with
its evidence. Nothing is deferred *in this file*: work that was deliberately postponed lives in
[`future-work.md`](./future-work.md), and the steps only the owner can take in the real world live in
[`go-live-checklist.md`](./go-live-checklist.md).

How a decision enters this ledger is unchanged (CLAUDE.md Rule 2): a contradiction, ambiguity or
locked-fact conflict is **flagged, not silently fixed** — recorded here with an ADR in [`adr/`](./adr/)
when it resolves a design question, and ruled on by the owner. An open row blocks only the ambiguous slice.
Ids: `M2-FP-<n>` is a numbered item of the cross-module fix plan; `M2-CI-OD-<n>` is build, CI or
supply-chain; `M2-DOC-OD-<n>` is documentation. Ids from the frozen ledger (`G11-OD-1`, `R2-5`) keep their
names.

## Rulings in force

| Id | Ruled | Ruling | Evidence |
|---|---|---|---|
| **G11-OD-1** | 2026-07-29 | Option **B**: a Control that **loses the singleton advisory lock mid-life pauses** dispatch/reclaim/retention and re-acquires on a bounded retry (`brain_singleton_reacquire_interval_sec`) rather than exiting or carrying on. | [ADR-0011](./adr/ADR-0011-singleton-lock-loss-does-not-stop-dispatch.md); `8122b09`; [architecture](./architecture.md#advisory-lock-key-registry) |
| **G12-OD-1** | 2026-07-29 | Option **A**: `job_events`' append-only trigger gets a per-FK-column carve-out, restoring the documented hard delete of an account or worker (FK nulled, audit row kept). | [ADR-0012](./adr/ADR-0012-job-events-set-null-fks-collide-with-append-only-trigger.md); `send_managed_0024`; `21b65e6` |
| **M2-FP-5** | 2026-08-04 | There is **one `quote` body both senders accept**; in this module `user_id` is *inert* — validated only if present, never required (module 3 requires it). | `ba9312a`; [`api-contract.md`](./api-contract.md) |
| **M2-FP-5b** | 2026-08-04 | `describe` declares `cost_fields_inert`: `unit_price_credits` is a live-retunable key, and a zero must not read as a broken price lookup. | `0b42e1b` |
| **M2-FP-6/8** | 2026-08-04 | `describe` publishes its state vocabularies, error codes and pricing models, **all derived from the code, never hand-listed**. | `dde94e0`; `contract_drift` |
| **M2-FP-7b** | 2026-08-04 | A wrong verb or an unrouted path leaves through the **error envelope** (with `request_id`), not net/http's plain-text 404/405. | `e3fee99` |
| **M2-FP-10** | 2026-08-05 | `/readyz` answers in the platform's envelope without breaking the monitor that reads it (the envelope is fixed, the dependency keys are per module). | `9c74fa8`; [operator runbook](./operator-runbook.md#monitoring-and-alerting) |
| **M2-FP-12** | 2026-08-05 | The dispatch kill-switch is a **four-rung ladder** (`serving` / `accept_and_defer` / `refuse_new` / `stopped`); `dispatch_enabled` is retained and composed as a **floor**, not replaced. | [ADR-0013](./adr/ADR-0013-dispatch-kill-switch-becomes-a-graduated-rung-ladder.md); `5af51f8`; `send_managed_0025` |
| **M2-FP-13** | 2026-08-05 | **Every autonomous mutator gets an `off`/`shadow`/`enforce` ladder seeded at `shadow`**; `autopause_enabled` is composed as a **ceiling**. A new autonomous mutator cannot ship enforcing on day one (a test names any file in neither map). Consequence accepted: `retention_mode=enforce` is a **required** go-live step. | [ADR-0014](./adr/ADR-0014-every-autonomous-mutator-gets-a-shadow-ladder.md); `c300fe6`; `send_managed_0026`; [go-live item 12](./go-live-checklist.md) |
| **M2-FP-14** | 2026-08-06 | `api_version` is **derived from the served route table**, and the Control refuses to boot without one. | `80f3a69` |
| **N1/D12** | 2026-08-07 | The DSN is scrubbed out of **every** pgx error that can reach a log. The harness module's two connect failures are a registered, acknowledged exclusion. | `abc86c5`; `dsn_scrub_wiring`; [future-work §4a](./future-work.md) |
| **R2-5** | 2026-08-07 | The stale `{job_item_id}` / `?job_id=` rows in the locked phase-9 spec are **annotated, not rewritten** (a closed spec is a historical record; no mechanism can police frozen specs, so a warning at the point of use is the control). Two rows were stale, not one. | `9d7bba0`; [future-work §7](./future-work.md); [frozen ledger](./history/open-decisions.md) |
| **M2-CI-OD-1** | 2026-08-05 | **Test discovery is itself gated.** `test_enumeration` fails when a Go package with tests is not run by `run.sh`, or when a `checks/*.sh` script is not wired into `real-infra.yml`; the harness also asserts each Go package *executes* as many tests as it declares. | `96b2e48`, `2e8be72`, `168f1d6`, `fd46e57`; [architecture](./architecture.md#build-status-and-the-two-ceilings) |
| **M2-CI-OD-2** | 2026-11-07 | The two module vulnerabilities (`GO-2026-5004`, `GO-2026-5970`) are cleared **by upgrading the builder** (`golang:1.26-alpine`, `pgx` v5.10.0, `x/text` v0.43.0), not by reverting the bump. | `4640dfb`; [security-posture §3.16](./security-posture.md); [go-live item 9](./go-live-checklist.md) |
| **M2-CI-OD-3** | 2026-11-08 | Module 3's **#131 is ported**: the RAM-backed Postgres is opt-in (`DEV_RAM_PG=1`, set by CI), so a `docker stop postgres` drill on the default dev stack does not destroy the data; guarded by `postgres_durability`. Dev keys stay mode 600 but are owned by their container's uid (`tools/own-secrets.sh`, guarded by `secret_ownership`). | `4d0f62c`; outage drill on a runner 22/22, run [42499590634](https://github.com/elzainxx/coldmail-send-managed/actions/runs/42499590634) |
| **M2-CI-OD-4** | 2026-11-09 | **Every image is pinned by digest, with no exception list**, and `image_pins` rejects any unpinned reference — compose, `services/*` and `tools/*` Dockerfiles, workflow `image:` lines and a heuristic scan of shell tooling (23-case self-test). Cost accepted: a pinned builder gets no base-image patches until a reviewed commit bumps the digest. | `72881f0`, `15a6b90`; [future-work §5](./future-work.md) |
| **M2-CI-OD-5** | 2026-11-09 | Supported releases everywhere: worker `node:20-alpine` → **`node:24-alpine`** (`engines.node` ≥ 24; CI `setup-node` 24) and `alpine:3.20` → **`alpine:3.24`** (Go final stages, migrate tool, `own-secrets.sh`), digest-pinned. Worker unit tests were identical on Node 20 and 24 before the move. Owner ruling C1.3. | `96a189f`; CI run [42633655325](https://github.com/elzainxx/coldmail-send-managed/actions/runs/42633655325) |
| **M2-CI-OD-6** | 2026-11-07 | The repo **forces LF** (`.gitattributes`: `* text=auto eol=lf`, with an exact `-text` carve-out for files already stored CRLF), because a parent clone on a host with `core.autocrlf=true` otherwise checks out CRLF shell scripts that bash and gofmt reject. | `8afd970` |
| **M2-DOC-OD-1** | 2026-11-09 | **One documentation structure for all three module repos.** Root `README.md`; `Docs/` = `architecture`, `install-and-deploy`, `operator-runbook`, `config-reference`, `api-contract`, `security-posture`, `go-live-checklist`, `future-work`, `open-decisions`; `Docs/adr/` for ADRs; `Docs/history/` for specs, closed plans, superseded and merged documents. Living topical docs were merged into the canonical ones (map in [`history/MOVED-DOCS.md`](./history/MOVED-DOCS.md)); the go-live checklist shows a **real status** per item, never "unchecked by default". | owner brief C3; the commits of 2026-11-09 |

## What was deliberately NOT ruled here

These are not open decisions — each is a deferred capability or a defect whose fix is a behavior change,
registered with its reason in [`future-work.md`](./future-work.md) (and, where a person must choose, in
the owner's action list). They do not block anything:

- the largest single gap (bounce/complaint ingestion), warm-up, multi-pool binding, worker admin CRUD,
  bulk credential re-encryption, admin RBAC, rate limiting — `future-work.md` §1–§3;
- the nine security-hardening items of §4a and the known defects of §6;
- the RESERVED-marker expiry rule for `config_consumption` and the digest-bump cadence — §5.

## Provenance

- [`history/open-decisions.md`](./history/open-decisions.md) — the frozen phase-0–10 ledger, ending with
  the empty-ledger confirmation of 2026-07-28 and the two pre-deploy-gate rulings (G11-OD-1, G12-OD-1) of
  2026-07-29.
- [`adr/`](./adr/) — the formal records (0000 template; 0001–0014).
- [`history/MOVED-DOCS.md`](./history/MOVED-DOCS.md) — where each moved or merged document went.
