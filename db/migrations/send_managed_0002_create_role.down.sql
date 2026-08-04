-- send_managed_0002_create_role.down.sql — reverse of the role-confirmation migration.
-- 0002 only re-asserted the bootstrap's role + default privileges; it created nothing new.
-- The role send_managed_app and the base default-privilege grants are owned by the Phase 0
-- bootstrap (send_managed_0000) and are reversed only by its down. So this DOWN un-records 0002
-- only — it must NOT drop the role (dropping it would break the still-applied bootstrap and
-- fail anyway if the role owns/holds objects).
DELETE FROM send_managed.schema_migrations WHERE version = '0002';
