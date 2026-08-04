-- send_managed_0006_create_boxes.down.sql — drop send_managed.boxes (dev/CI only; forward-only in prod).
-- CASCADE nullifies dependent FKs (jobs.assigned_box_id, job_events.box_id).
DROP TABLE IF EXISTS send_managed.boxes CASCADE;
DELETE FROM send_managed.schema_migrations WHERE version = '0006';
