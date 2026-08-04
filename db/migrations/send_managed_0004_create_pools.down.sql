-- send_managed_0004_create_pools.down.sql — drop send_managed.pools (dev/CI only; forward-only in prod).
-- CASCADE drops dependent rows (e.g. send_managed.accounts.pool_id FK) — destructive, dev-safe only.
DROP TABLE IF EXISTS send_managed.pools CASCADE;
DELETE FROM send_managed.schema_migrations WHERE version = '0004';
