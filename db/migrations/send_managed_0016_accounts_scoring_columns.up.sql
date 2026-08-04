-- send_managed_0016_accounts_scoring_columns.up.sql — Phase 5 (§9, P5-6 + P5-8, APPROVED).
-- Two additive columns on the EXISTING send_managed.accounts table. Additive-only: each adds one
-- column with a constant/NULL default; NO existing column, constraint, or enum is changed, so
-- the locked jobs_attempt_count_check, accounts_daily_sent_check, the state machine, and the
-- error_class enum are all untouched (CLAUDE.md Rule 5, additive-only).
--   consecutive_failures : feeds the status score (§3); reset 0 on success, +1 on a penalizing
--                          failure, untouched by bounce_permanent.
--   throttle_paused_at   : precise write-independent throttle clock (§4.3); stamped NOW() on a
--                          throttle pause, cleared to NULL on un-pause. Expiry =
--                          NOW() >= throttle_paused_at + throttle_pause_seconds (config).
--
-- Grants (ADR-0008 trap): both columns land on send_managed.accounts, which send_managed_app already holds
-- full SELECT/INSERT/UPDATE/DELETE on (send_managed_0012). Table-level DML covers all columns, so NO new
-- GRANT is required and none is issued — but the schema-wide TestGrantsInvariant must STAY green
-- (it enumerates table-class privileges, which an ADD COLUMN never changes). No new table, no new
-- grant, no privilege drift.
--
-- Population note: both ADD COLUMNs are metadata-only on Postgres 11+ (a constant DEFAULT 0 and a
-- NULL-defaulted column need no table rewrite), so this is fast even on a populated table. Existing
-- rows read consecutive_failures=0 (correct neutral start) and throttle_paused_at=NULL (no account
-- is throttle-paused at migration time — pause_reason='throttle' is only ever set by Phase 5, which
-- also stamps the timestamp, so there is no back-fill gap).

ALTER TABLE send_managed.accounts
    ADD COLUMN consecutive_failures INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN throttle_paused_at   TIMESTAMPTZ;              -- NULL when not throttle-paused

-- No supporting index required in V1 (§9): consecutive_failures is recomputed per-outcome on the
-- one account (no filter query); throttle_paused_at is read only for the account the eligibility
-- predicate already located by pool_id. Add later only if a score-sweep or throttle-expiry scan
-- is ever introduced.

INSERT INTO send_managed.schema_migrations (version) VALUES ('0016');
