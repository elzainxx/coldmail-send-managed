-- send_managed_0002_create_role.up.sql — Phase 1: confirm the app role + default privileges.
-- The `send_managed_app` LOGIN role and the `ALTER DEFAULT PRIVILEGES FOR ROLE send_managed_owner ...
-- GRANT ... TO send_managed_app` were established by the Phase 0 bootstrap (send_managed_0000). This
-- migration idempotently re-asserts them so the feature chain is self-contained, WITHOUT
-- recreating the role or changing ownership. Role password is NEVER set in migration SQL
-- (out-of-band via ALTER ROLE / the runner's set-app-password step, OD-1).

-- Confirm the login role exists (bootstrap created it; guard makes this safe to re-run).
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'send_managed_app') THEN
        CREATE ROLE send_managed_app LOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB;
    END IF;
END $$;

-- USAGE on the schema + default privileges on FUTURE tables/sequences that send_managed_owner
-- creates. Re-asserting is idempotent (GRANT is additive; ALTER DEFAULT PRIVILEGES is
-- last-writer). This keeps every feature table (0004+) usable by send_managed_app without a
-- per-table grant, matching the bootstrap posture. The append-only REVOKEs that narrow
-- job_events/send_logs land in their own creating migrations (0008/0009), per ADR-0002.
GRANT USAGE ON SCHEMA send_managed TO send_managed_app;
ALTER DEFAULT PRIVILEGES FOR ROLE send_managed_owner IN SCHEMA send_managed
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO send_managed_app;
ALTER DEFAULT PRIVILEGES FOR ROLE send_managed_owner IN SCHEMA send_managed
    GRANT USAGE, SELECT ON SEQUENCES TO send_managed_app;

-- Self-record.
INSERT INTO send_managed.schema_migrations (version) VALUES ('0002');
