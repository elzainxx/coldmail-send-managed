-- send_managed_0012_grants_and_rls.down.sql — reverse grants + RLS (dev/CI only; forward-only in
-- prod). Drops the RLS policies, disables RLS, and revokes the table/sequence grants this
-- migration issued. Does NOT touch the two platform-table SELECT grants — those are owned by
-- the bootstrap (send_managed_0000) and reversed only by its down.
DROP POLICY IF EXISTS sender_module_full_access ON send_managed.send_logs;
DROP POLICY IF EXISTS sender_module_full_access ON send_managed.job_events;
DROP POLICY IF EXISTS sender_module_full_access ON send_managed.config;
DROP POLICY IF EXISTS sender_module_full_access ON send_managed.jobs;
DROP POLICY IF EXISTS sender_module_full_access ON send_managed.boxes;
DROP POLICY IF EXISTS sender_module_full_access ON send_managed.accounts;
DROP POLICY IF EXISTS sender_module_full_access ON send_managed.pools;

ALTER TABLE send_managed.send_logs   DISABLE ROW LEVEL SECURITY;
ALTER TABLE send_managed.job_events  DISABLE ROW LEVEL SECURITY;
ALTER TABLE send_managed.config      DISABLE ROW LEVEL SECURITY;
ALTER TABLE send_managed.jobs        DISABLE ROW LEVEL SECURITY;
ALTER TABLE send_managed.boxes       DISABLE ROW LEVEL SECURITY;
ALTER TABLE send_managed.accounts    NO FORCE ROW LEVEL SECURITY;
ALTER TABLE send_managed.accounts    DISABLE ROW LEVEL SECURITY;
ALTER TABLE send_managed.pools       DISABLE ROW LEVEL SECURITY;

REVOKE ALL ON ALL SEQUENCES IN SCHEMA send_managed FROM send_managed_app;
REVOKE ALL ON send_managed.send_logs  FROM send_managed_app;
REVOKE ALL ON send_managed.job_events FROM send_managed_app;
REVOKE ALL ON send_managed.config     FROM send_managed_app;
REVOKE ALL ON send_managed.jobs       FROM send_managed_app;
REVOKE ALL ON send_managed.boxes      FROM send_managed_app;
REVOKE ALL ON send_managed.accounts   FROM send_managed_app;
REVOKE ALL ON send_managed.pools      FROM send_managed_app;

DELETE FROM send_managed.schema_migrations WHERE version = '0012';
