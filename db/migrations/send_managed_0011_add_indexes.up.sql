-- send_managed_0011_add_indexes.up.sql — Phase 1: performance indexes (dispatcher claim/reclaim,
-- eligibility, reporting). This migration is the CANONICAL, executable index list — where it
-- differs from 02-claim-and-indexes.md in name/shape, THIS file wins (02 documents rationale).
-- Standard CREATE INDEX (tables are fresh/empty in Phase 1); CONCURRENTLY is for future
-- migrations on populated tables. job_events/send_logs indexes were created with their tables
-- (0008/0009). Indexes inherit the table's owner (send_managed_owner), so no ALTER needed.

-- === send_managed.jobs =========================================================================
-- Claim hot path: WHERE state='queued' AND next_attempt_at <= NOW() ORDER BY next_attempt_at,
-- created_at. Partial (queued is a tiny hot subset); created_at is the deterministic FIFO
-- tiebreaker for equal next_attempt_at. No lease_expires_at predicate (queued rows have NULL
-- lease per jobs_lease_valid_check), so the planner can use this for the claim query as written.
CREATE INDEX jobs_claim_idx
    ON send_managed.jobs (next_attempt_at, created_at)
    WHERE state = 'queued';

-- Lease-expiry reclaim: WHERE state IN ('assigned','sending') AND lease_expires_at < NOW().
-- Reconciliation note (this file is canonical over 02-claim-and-indexes.md, which documents a
-- single-column (lease_expires_at) index): `state` is kept as a trailing key column here even
-- though the partial WHERE already pins state to the two-value set — a deliberate, harmless
-- widening that keeps the index self-describing and lets a planner satisfy a `state`-projecting
-- reclaim variant index-only. `lease_expires_at` leads, so the `< NOW()` range scan the reclaim
-- query needs is served identically to the drafted single-column shape.
CREATE INDEX jobs_reclaim_idx
    ON send_managed.jobs (lease_expires_at, state)
    WHERE state IN ('assigned', 'sending') AND lease_expires_at IS NOT NULL;

-- Per-user state filtering (concurrency enforcement, per-user listings).
CREATE INDEX jobs_user_id_state_idx
    ON send_managed.jobs (user_id, state);

-- Campaign progress reporting.
CREATE INDEX jobs_campaign_id_state_idx
    ON send_managed.jobs (campaign_id, state);

-- Worker-death reclaim: find jobs assigned to a dead worker.
CREATE INDEX jobs_assigned_box_id_idx
    ON send_managed.jobs (assigned_box_id)
    WHERE assigned_box_id IS NOT NULL;

-- === send_managed.accounts =====================================================================
-- Eligibility: pool + score, excluding hard-ineligible rows.
CREATE INDEX accounts_eligibility_idx
    ON send_managed.accounts (pool_id, account_status_score)
    WHERE paused = FALSE AND quarantined = FALSE AND disabled = FALSE;

-- Daily reset batch job: find accounts with stale counters.
CREATE INDEX accounts_daily_reset_day_idx
    ON send_managed.accounts (daily_reset_day);

-- Per-user account queries (admin).
CREATE INDEX accounts_user_id_idx
    ON send_managed.accounts (user_id);

-- Credential rotation queries (find rows on an old key version).
CREATE INDEX accounts_credential_key_id_idx
    ON send_managed.accounts (credential_key_id);

-- === send_managed.pools ========================================================================
CREATE INDEX pools_user_id_kind_idx
    ON send_managed.pools (user_id, kind);

CREATE INDEX pools_owner_user_id_idx
    ON send_managed.pools (owner_user_id)
    WHERE owner_user_id IS NOT NULL;

CREATE INDEX pools_fallback_pool_id_idx
    ON send_managed.pools (fallback_pool_id)
    WHERE fallback_pool_id IS NOT NULL;

-- === send_managed.boxes ========================================================================
CREATE INDEX boxes_liveness_idx
    ON send_managed.boxes (status, last_heartbeat_at);

CREATE INDEX boxes_user_id_idx
    ON send_managed.boxes (user_id);

INSERT INTO send_managed.schema_migrations (version) VALUES ('0011');
