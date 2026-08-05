-- send_managed_0026_shadow_mode_for_every_autonomous_mutator.down.sql — remove the two mode rows.
--
-- Both keys are NEW in 0026, so deleting the two rows restores the exact pre-0026 state of
-- send_managed.config. Nothing else was added, altered or granted.
--
-- IT REMOVES EXACTLY TWO ROWS, AND `autopause_enabled` IS NOT ONE OF THEM. The boolean is RETAINED by
-- item 13 (it has live consumers), and it is precisely what an over-broad down would sweep up:
-- "remove the auto-pause switches this migration introduced" is a natural-sounding instruction that
-- would leave a reverted system with no auto-pause kill-switch at all. `retention_sweep_interval_ms`
-- and `retention_delete_batch_size` are likewise untouched — they predate this migration and are rate
-- controls, not enables. The migration lifecycle test asserts all of it at the Down 0026 window:
-- both new keys gone, autopause_enabled and both retention rate keys still present.
--
-- BEHAVIOUR AFTER A REVERT, which is the part worth reading. Reverting this migration RESTORES
-- ENFORCING BEHAVIOUR, in both writers:
--   * a throttle outcome pauses the account again, governed only by autopause_enabled
--   * the retention sweep DELETEs again, governed only by its batch size
-- That is the correct direction — the pre-0026 code had no dial to consult, so removing the dials
-- returns the system to what it did before, not to a halted state. But note that it is the LESS SAFE
-- direction, so a revert is not a no-op operationally even though it is a clean one schema-wise.
--
-- WHAT THIS DOWN CANNOT DO. The Go side READS both keys (dispatch/config.go, via the same
-- readStringKey path autoquarantine_mode and dispatch_mode use), and that read ERRORS when the row is
-- absent — at Control boot, on the config-reload path, and from the admin surface. So running this
-- down WITHOUT reverting the Go source leaves a Control that does not start.
--
-- That coupling is deliberate, and identical to send_managed_0025's: this down belongs to a source
-- revert, not to a standalone `psql -f`. Whoever runs it as part of reverting the item-13 commit gets
-- the readers removed by git in the same step. Whoever runs it standalone gets a hard, immediate, loud
-- boot failure — the correct failure for "the code requires a dial the database does not have", and
-- far better than what a graceful default would produce: a Control that boots and silently deletes
-- rows an operator believed were being merely counted.

DELETE FROM send_managed.config WHERE key IN ('autopause_mode', 'retention_mode');

DELETE FROM send_managed.schema_migrations WHERE version = '0026';
