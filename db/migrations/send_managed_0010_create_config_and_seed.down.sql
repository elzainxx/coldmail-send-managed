-- send_managed_0010_create_config_and_seed.down.sql — drop send_managed.config (dev/CI only; forward-only
-- in prod). Dropping the table removes the 14 seeded rows with it.
DROP TABLE IF EXISTS send_managed.config CASCADE;
DELETE FROM send_managed.schema_migrations WHERE version = '0010';
