-- send_managed_0009_create_send_logs.down.sql — drop send_logs + its trigger/function (dev/CI only;
-- forward-only in prod).
DROP TRIGGER IF EXISTS trg_send_logs_reject_update ON send_managed.send_logs;
DROP FUNCTION IF EXISTS send_managed.fn_send_logs_reject_update();

DROP TABLE IF EXISTS send_managed.send_logs CASCADE;

DELETE FROM send_managed.schema_migrations WHERE version = '0009';
