-- send_managed_0001_create_schema.down.sql — reverse of the feature-chain start.
-- This migration only CONFIRMED the bootstrap's schema (it created nothing new), so its DOWN
-- must NOT drop the schema, roles, or tracking table — those are owned by the Phase 0
-- bootstrap (send_managed_0000) and are torn down only by send_managed_0000's own down. Reversing 0001
-- means only un-recording version 0001; the schema remains for the bootstrap to own.
DELETE FROM send_managed.schema_migrations WHERE version = '0001';
