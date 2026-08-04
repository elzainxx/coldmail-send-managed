-- send_managed_0000_bootstrap.up.sql — Phase 0 pre-feature bootstrap (ADR-0002).
-- Numbered 0000 (pre-feature groundwork) so the LOCKED feature chain stays exactly
-- send_managed_0001..send_managed_0020 as every Phase 1–9 spec and the Phase 9 §4 config reference assume.
-- Creates the EMPTY send_managed schema owned by a non-login/non-superuser owner (OD-13/ADR-0004),
-- the login app role, the migration tracking table, and the enumerated grants. It creates
-- NO feature tables, enums, or config — those are Phase 1+ (the feature chain begins at
-- send_managed_0001_create_schema). Runs as an admin/owner-capable role (OD-1). Role passwords are
-- NEVER in migration SQL.

-- 1) Roles. Idempotent guards so this is safe under managed/out-of-band role provisioning.
DO $$
BEGIN
    -- send_managed_owner: OWNS the schema so FORCE ROW LEVEL SECURITY actually enforces against
    -- the app role. NOLOGIN, NOSUPERUSER, NOCREATEROLE, NOCREATEDB.
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'send_managed_owner') THEN
        CREATE ROLE send_managed_owner NOLOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB;
    END IF;
    -- send_managed_app: the LOGIN app role the Control connects as at runtime. Password is set
    -- OUT-OF-BAND (ALTER ROLE ... PASSWORD) — never here.
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'send_managed_app') THEN
        CREATE ROLE send_managed_app LOGIN NOSUPERUSER NOCREATEROLE NOCREATEDB;
    END IF;
END $$;

-- 2) Schema, owned by send_managed_owner; deny PUBLIC (defense in depth).
CREATE SCHEMA IF NOT EXISTS send_managed AUTHORIZATION send_managed_owner;
REVOKE ALL ON SCHEMA send_managed FROM PUBLIC;
GRANT USAGE ON SCHEMA send_managed TO send_managed_app;

-- 3) Migration tracking table — owned by send_managed_owner, one row per migration (self-tracked).
CREATE TABLE IF NOT EXISTS send_managed.schema_migrations (
    version    TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE send_managed.schema_migrations OWNER TO send_managed_owner;
-- The runner and migrations (admin role) read/write this; send_managed_app may read it.
GRANT SELECT ON send_managed.schema_migrations TO send_managed_app;

-- 4) Default privileges: objects FUTURE migrations create AS send_managed_owner are granted to
--    send_managed_app automatically. Append-only REVOKEs + platform-table SELECTs are issued
--    by their own Phase-1 migrations (e.g. send_managed_0012 grants/RLS), not here.
ALTER DEFAULT PRIVILEGES FOR ROLE send_managed_owner IN SCHEMA send_managed
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO send_managed_app;
ALTER DEFAULT PRIVILEGES FOR ROLE send_managed_owner IN SCHEMA send_managed
    GRANT USAGE, SELECT ON SEQUENCES TO send_managed_app;

-- 5) The ONLY two operations permitted on platform-owned tables: read-only SELECT.
--    Preflight (run before this) guarantees the tables exist.
GRANT SELECT ON public.users     TO send_managed_app;
GRANT SELECT ON public.job_items TO send_managed_app;

-- 6) Self-record (locked convention — the runner does NOT write this row; the file does).
INSERT INTO send_managed.schema_migrations (version) VALUES ('0000');
