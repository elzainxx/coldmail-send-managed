-- send_managed_0025_dispatch_mode_ladder.down.sql — remove the dispatch_mode row.
--
-- This down is the clean direction: the key is NEW here, so removing the row restores the exact
-- pre-0025 state of send_managed.config. Nothing else was added, altered or granted, so there is
-- nothing else to reverse.
--
-- IT REMOVES EXACTLY ONE ROW, AND `dispatch_enabled` IS NOT IT. The boolean is RETAINED by this item
-- (see the up file: module 3's ADR-0016 two-condition drop test fails condition (1) for it), and it is
-- exactly what an over-broad down would sweep up — "remove the dispatch switches this migration
-- introduced" is a natural-sounding instruction that would leave a reverted system with NO dispatch
-- kill-switch at all. The migration lifecycle test asserts both halves at the Down 0025 window:
-- dispatch_mode gone, dispatch_enabled still present.
--
-- WHAT THIS DOWN CANNOT DO, and why that direction is the DANGEROUS one here. The Go side is a READ:
-- dispatch/config.go loads dispatch_mode with readStringKey, which errors when the row is absent, and
-- LoadConfig is called at Control boot, on the config-reload path, and by the admin surface. So
-- running this down WITHOUT reverting the Go source leaves a Control that does not start.
--
-- That coupling is deliberate and this down belongs to a source revert, not to a standalone
-- `psql -f`. Whoever runs it as part of reverting the item-12 commit gets the reader removed by git in
-- the same step. Whoever runs it standalone gets a hard, immediate, loud boot failure — which is the
-- correct failure for "the code requires a dial the database does not have," and is emphatically
-- better than the alternative a graceful default would produce: a Control that boots and silently
-- ignores the rung an operator pulled. A read that fails loudly is not a defect to be smoothed over
-- here.

DELETE FROM send_managed.config WHERE key = 'dispatch_mode';

DELETE FROM send_managed.schema_migrations WHERE version = '0025';
