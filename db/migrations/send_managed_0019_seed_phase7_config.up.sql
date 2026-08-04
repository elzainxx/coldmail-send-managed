-- send_managed_0019_seed_phase7_config.up.sql — Phase 7 (§5.1). The ONLY structural delta Phase 7 makes.
-- Six additive INSERTs into the EXISTING send_managed.config table (same pattern as send_managed_0010/0017/0018).
-- No table structure changes; NO column, constraint, enum, or index is added or altered — Phase 7 is
-- fully schema-neutral (P7-1: per-day summaries derive on demand from the permanent job_events, so
-- NO send_managed.send_log_summaries table / send_managed_0020 is created). value is JSONB-wrapped {"v": ...};
-- default_value is never mutated (CLAUDE.md Rule 1). All six keys are appended to
-- Docs/config-manifest.txt + Docs/config-reference.md in the SAME change (config_drift gate: 19 -> 25).
--
--   startup_reconnect_window_sec : the startup-rescan reconnect-reconciliation window (§1.2). On boot
--                                  the rescan EXTENDS orphaned in-flight leases by this many seconds
--                                  (event-free — no state change ⇒ no job_events row ⇒ no account FK
--                                  lock ⇒ cannot deadlock, the Phase 6 lesson) and refreshes worker
--                                  heartbeats, so reconnecting workers can replay buffered outcomes
--                                  before the continuous reclaim loop reclaims. Consumed by the Control.
--                                  ORDERING INVARIANT (P7-11, §5.3): box_outcome_buffer_ttl_sec MUST be
--                                  >= this, else a late-but-in-window reconnect finds its buffered
--                                  outcome expired -> lease reclaim -> re-send -> silent duplicate.
--   shutdown_drain_timeout_sec   : max graceful-shutdown drain wait for in-flight sends to report
--                                  before leaving them for the next boot's rescan (§4). Keep BELOW the
--                                  deploy SIGTERM->SIGKILL grace. Consumed by the Control.
--   retention_sweep_interval_ms  : the housekeeping-loop tick for the send_logs retention sweep (§3.1).
--                                  _ms to match dispatch_tick_interval_ms / reclaim_tick_interval_ms.
--                                  Consumed by the Control.
--   retention_delete_batch_size  : rows per send_logs retention DELETE batch (§3.1) — bounds lock/
--                                  vacuum pressure. OWNER of Phase 1 #29 (deferred to Phase 7).
--   rollup_granularity           : bucket granularity for on-demand debug-log summaries (§3.2).
--                                  Introduced because Phase 1 has NO metrics_aggregation_period key
--                                  (flagged, not silently reused — P7-9). string value.
--   box_outcome_buffer_ttl_sec   : bound on the WORKER's outcome buffer (P7-4, §1.3): retain/replay
--                                  unacked outcomes up to this age, then drop and fall back to reclaim.
--                                  Consumed by the worker (coordinated Phase 3 note). ORDERING INVARIANT
--                                  (P7-11): must be >= startup_reconnect_window_sec (default 900 >= 120,
--                                  ~2x margin recommended for reconnect-backoff).
--
-- Grants (ADR-0008 trap): all six rows land in send_managed.config, which send_managed_app already holds SELECT
-- on (send_managed_0010). No new table, no new grant; the schema-wide TestGrantsInvariant stays green (a
-- config INSERT changes no table-class privilege). Phase 7 adds NO schema object, so there is no new
-- grant to enumerate.
INSERT INTO send_managed.config (key, value, default_value, description, value_type) VALUES
('startup_reconnect_window_sec', '{"v":120}', '{"v":120}',
 'Startup rescan reconnect-reconciliation window: seconds the rescan extends orphaned in-flight leases / holds workers before the continuous reclaim loop reclaims. Phase 7 §1. INVARIANT (P7-11): box_outcome_buffer_ttl_sec >= this.',
 'integer'),
('shutdown_drain_timeout_sec', '{"v":30}', '{"v":30}',
 'Max graceful-shutdown drain wait for in-flight sends to report before leaving them for the next boot rescan. Keep below deploy SIGTERM->SIGKILL grace. Phase 7 §4.',
 'integer'),
('retention_sweep_interval_ms', '{"v":3600000}', '{"v":3600000}',
 'Housekeeping-loop tick for the send_logs retention sweep. Phase 7 §3.',
 'integer'),
('retention_delete_batch_size', '{"v":1000}', '{"v":1000}',
 'Rows per send_logs retention DELETE batch (bounds lock/vacuum pressure). Owner of Phase 1 #29. Phase 7 §3.',
 'integer'),
('rollup_granularity', '{"v":"day"}', '{"v":"day"}',
 'Bucket granularity for debug-log summaries. Introduced because Phase 1 has no metrics_aggregation_period key. Phase 7 §3.',
 'string'),
('box_outcome_buffer_ttl_sec', '{"v":900}', '{"v":900}',
 'Bound on the worker outcome-buffer: retain/replay unacked outcomes up to this age, then drop and fall back to reclaim. Consumed by the worker (coordinated Phase 3). INVARIANT (P7-11): >= startup_reconnect_window_sec (with margin). Phase 7 §1.3.',
 'integer');

INSERT INTO send_managed.schema_migrations (version) VALUES ('0019');
