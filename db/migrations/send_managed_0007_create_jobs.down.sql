-- send_managed_0007_create_jobs.down.sql — drop send_managed.jobs (dev/CI only; forward-only in prod).
-- CASCADE drops dependent rows in send_managed.job_events and send_managed.send_logs.
DROP TABLE IF EXISTS send_managed.jobs CASCADE;
DELETE FROM send_managed.schema_migrations WHERE version = '0007';
