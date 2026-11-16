-- send_managed_0027_job_state_cancelled.down.sql -- FORWARD-ONLY for the value itself (a documented
-- no-op): Postgres cannot DROP an enum value, and the job_state enum is additive-only forever
-- (CLAUDE.md Rule 5) -- once a deployment has stored 'cancelled' rows or job_events rows naming it, the
-- value cannot be removed without rewriting those rows and the type, which is not a down migration.
--
-- What this down DOES do is un-record the version, so the chain is walkable in reverse (the lifecycle
-- test steps 0028 -> 0027 -> ... -> 0000). Re-applying 0027 afterwards is a clean no-op, because the up
-- uses ADD VALUE IF NOT EXISTS. The label is deliberately left in place, and the lifecycle test asserts
-- exactly that (it would be a lie for a down to claim otherwise).
--
-- The bootstrap down (0000) drops the whole schema CASCADE, which is the only thing that ever removes
-- the type -- and it takes every row with it.

DELETE FROM send_managed.schema_migrations WHERE version = '0027';
