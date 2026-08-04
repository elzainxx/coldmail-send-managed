-- send_managed_0000_bootstrap.down.sql — reverse of the pre-feature bootstrap (dev/CI only; forward-only in
-- prod). Drops the schema (CASCADE removes the tracking table and anything created later),
-- revokes platform-table grants, and drops the roles if unused. DELETE of the version row is
-- moot once the schema is dropped, but we keep the convention explicit for symmetry.

-- Revoke platform-table grants (roles may persist if they own other objects).
REVOKE SELECT ON public.users     FROM send_managed_app;
REVOKE SELECT ON public.job_items FROM send_managed_app;

-- Remove default privileges we set.
ALTER DEFAULT PRIVILEGES FOR ROLE send_managed_owner IN SCHEMA send_managed
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM send_managed_app;
ALTER DEFAULT PRIVILEGES FOR ROLE send_managed_owner IN SCHEMA send_managed
    REVOKE USAGE, SELECT ON SEQUENCES FROM send_managed_app;

-- Bookkeeping delete (explicit, though the CASCADE below also removes the table).
DELETE FROM send_managed.schema_migrations WHERE version = '0000';

-- Drop the schema and everything in it.
DROP SCHEMA IF EXISTS send_managed CASCADE;

-- Drop the cluster-global roles BEST-EFFORT. Roles are shared across databases; if a role
-- still owns objects in another database (e.g. a template/test DB, or the platform DB),
-- DROP ROLE raises dependent_objects_still_exist (2BP01). That is legitimate — the role is
-- still in use elsewhere — so we catch it and retain the role rather than abort the whole
-- down. (Down is dev/CI-only anyway; prod is forward-only.)
DO $$
BEGIN
  BEGIN
    DROP ROLE IF EXISTS send_managed_app;
  EXCEPTION WHEN dependent_objects_still_exist THEN
    RAISE NOTICE 'send_managed_app retained: still owns objects in another database';
  END;
  BEGIN
    DROP ROLE IF EXISTS send_managed_owner;
  EXCEPTION WHEN dependent_objects_still_exist THEN
    RAISE NOTICE 'send_managed_owner retained: still owns objects in another database';
  END;
END $$;
