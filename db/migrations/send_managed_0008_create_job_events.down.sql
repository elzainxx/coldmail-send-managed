-- send_managed_0008_create_job_events.down.sql — drop job_events + its triggers/functions (dev/CI
-- only; forward-only in prod). Drop triggers on send_managed.jobs first, then functions, then the
-- table (CASCADE). The REVOKE is undone implicitly when the table is dropped.
DROP TRIGGER IF EXISTS trg_job_events_on_state_change ON send_managed.jobs;
DROP TRIGGER IF EXISTS trg_job_events_on_insert ON send_managed.jobs;
DROP FUNCTION IF EXISTS send_managed.fn_job_events_on_state_change();
DROP FUNCTION IF EXISTS send_managed.fn_job_events_on_insert();

DROP TRIGGER IF EXISTS trg_job_events_reject_update ON send_managed.job_events;
DROP FUNCTION IF EXISTS send_managed.fn_job_events_reject_update();

DROP TABLE IF EXISTS send_managed.job_events CASCADE;

DELETE FROM send_managed.schema_migrations WHERE version = '0008';
