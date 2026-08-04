-- send_managed_0014_create_account_events.down.sql — drop the account_events audit sink + its
-- trigger/function/policy (dev/CI only; forward-only in prod). Reverse creation order. The
-- REVOKE/GRANT are undone implicitly when the table is dropped. No Phase 1 object is touched.
DROP POLICY IF EXISTS account_events_sender_module_rw ON send_managed.account_events;
DROP TRIGGER IF EXISTS trg_account_events_reject_update ON send_managed.account_events;
DROP FUNCTION IF EXISTS send_managed.fn_account_events_reject_update();
DROP TABLE IF EXISTS send_managed.account_events;

DELETE FROM send_managed.schema_migrations WHERE version = '0014';
