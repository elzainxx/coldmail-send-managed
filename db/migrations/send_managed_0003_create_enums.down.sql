-- send_managed_0003_create_enums.down.sql — drop all Phase 1 enum types (dev/CI only; forward-only
-- in prod). CASCADE removes any columns that reference these enums (destructive — safe only on
-- a fresh/dev schema). Dropped in reverse dependency order for clarity.
DROP TYPE IF EXISTS send_managed.pause_reason    CASCADE;
DROP TYPE IF EXISTS send_managed.event_actor     CASCADE;
DROP TYPE IF EXISTS send_managed.box_status      CASCADE;
DROP TYPE IF EXISTS send_managed.routing_strategy CASCADE;
DROP TYPE IF EXISTS send_managed.pool_kind       CASCADE;
DROP TYPE IF EXISTS send_managed.error_class     CASCADE;
DROP TYPE IF EXISTS send_managed.job_state       CASCADE;

DELETE FROM send_managed.schema_migrations WHERE version = '0003';
