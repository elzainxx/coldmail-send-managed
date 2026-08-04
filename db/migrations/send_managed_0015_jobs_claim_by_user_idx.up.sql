-- send_managed_0015_jobs_claim_by_user_idx.up.sql — Phase 4 (P4-9, ruling 2): the per-user claim index.
-- Additive partial index on the EXISTING send_managed.jobs table. Backs the per-user claim (§1.1):
--   WHERE state='queued' AND user_id=:U ORDER BY next_attempt_at, created_at.
-- It COEXISTS with the Phase 1 jobs_claim_idx (next_attempt_at, created_at) WHERE state='queued'
-- (which still serves the global active-user scan A2 and the reclaim loop) — it does NOT replace
-- it. No column is added, dropped, or altered; no constraint changes. This is the ONLY Phase-4
-- schema delta (spec §12).
--
-- CONCURRENTLY note (spec §12): CREATE INDEX CONCURRENTLY cannot run inside a transaction block.
-- By Phase 4 send_managed.jobs may hold real rows, so CONCURRENTLY avoids blocking the dispatcher's
-- claims during the build. The migration runner detects "CONCURRENTLY" in this file and applies it
-- OUTSIDE its transaction wrapper, executing each statement individually (auto-commit) — so the
-- file must carry NO plpgsql/dollar-quoted bodies (it does not) and its own bookkeeping INSERT is
-- the last statement. On a fresh/empty DB a CONCURRENTLY build is equivalent to a plain one.
-- The index inherits send_managed.jobs' owner (send_managed_owner) — no ALTER needed.
CREATE INDEX CONCURRENTLY IF NOT EXISTS jobs_claim_by_user_idx
    ON send_managed.jobs (user_id, next_attempt_at, created_at)
    WHERE state = 'queued';

INSERT INTO send_managed.schema_migrations (version) VALUES ('0015');
