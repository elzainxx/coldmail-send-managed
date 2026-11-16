-- send_managed_0027_job_state_cancelled.up.sql -- the ONE additive value the job_state enum gains
-- (CLAUDE.md Rule 5: the seven enums are additive-only forever): 'cancelled', a TERMINAL state for an
-- item the platform stopped before the dispatcher claimed it (POST /v1/cancel, send_managed_0028).
--
-- WHY THIS IS A FILE OF ITS OWN. Postgres refuses to USE a freshly added enum value inside the
-- transaction that added it (55P04 unsafe use of new value). The migration runner applies each file in
-- ONE transaction (tools/send-managed-migrate/runner.go applyOne), so everything that names 'cancelled'
-- -- the widened jobs_completed_at_valid_check, the cancel-guard trigger, the backfill -- must live in
-- a LATER file, which runs after this one has COMMITTED. That is send_managed_0028. Folding the two
-- together fails on the first apply with "unsafe use of new value "cancelled" of enum type job_state".
--
-- IF NOT EXISTS makes a re-apply after a partial failure (value added, version row not recorded) a
-- clean no-op rather than a 42710 duplicate_object. The value is appended AFTER 'bounced', so
-- enumsortorder (and therefore the order describe.vocabularies.job_state publishes) keeps the six
-- existing values exactly where they were and the new one last.
--
-- NO BEHAVIOUR CHANGES HERE. Nothing writes 'cancelled' until 0028's code path exists; the value is
-- inert on its own, which is what makes shipping it as a separate step safe.

ALTER TYPE send_managed.job_state ADD VALUE IF NOT EXISTS 'cancelled';

INSERT INTO send_managed.schema_migrations (version) VALUES ('0027');
