-- send_managed_0013_accounts_pool_uniqueness.up.sql — Phase 2 (ruling 3): per-pool account uniqueness.
-- Additive UNIQUE index on the EXISTING send_managed.accounts table — NO column is added, dropped, or
-- altered (Phase 2 modifies no Phase 1 table column). Blocks a duplicate (smtp_host,
-- smtp_username) WITHIN a pool; the SAME pair is allowed in a DIFFERENT pool (the same real
-- mailbox may legitimately be placed in more than one pool — accepted residual, spec §2.7).
-- The API pre-checks and returns a clean 409 before the DB; this index is the race backstop
-- (its 23505 is caught and translated to 409, never surfaced raw).
--
-- Population note (spec §12): on the empty/near-empty Phase-2 dataset this is a plain
-- CREATE UNIQUE INDEX (runs in the runner's txn). If ever applied to a populated table, switch
-- to CREATE UNIQUE INDEX CONCURRENTLY (cannot run in a txn) and pre-verify no existing dupes.
-- The index inherits send_managed.accounts' owner (send_managed_owner) — no ALTER needed.

CREATE UNIQUE INDEX accounts_pool_host_user_unique
    ON send_managed.accounts (pool_id, smtp_host, smtp_username);

INSERT INTO send_managed.schema_migrations (version) VALUES ('0013');
