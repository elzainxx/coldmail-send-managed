-- send_managed_0018_seed_phase6_config.up.sql — Phase 6 (§5.1, P6-7 + P6-11).
-- Two additive INSERTs into the EXISTING send_managed.config table (same pattern as send_managed_0010/0017).
-- No table structure changes; NO column, constraint, enum, or index is added or altered — the ONLY
-- structural delta Phase 6 makes. value is JSONB-wrapped {"v": ...}; default_value is never mutated
-- (CLAUDE.md Rule 1). Both keys are appended to Docs/config-manifest.txt + Docs/config-reference.md
-- in the SAME change (config_drift gate: 17 -> 19 keys).
--
--   reclaim_tick_interval_ms  : the reclaim loop's poll interval (§2.1, P6-7). 15000 (15s) is much
--                               smaller than both reclaim windows (lease 120s, dead-worker 360s) so a
--                               condition is detected within ~one tick, yet each tick is only two
--                               indexed queries — cheap. Named _ms to match dispatch_tick_interval_ms.
--   smtp_operation_timeout_sec: the WORKER's per-operation SMTP timeout (connect/greeting/socket-read
--                               on MAIL/RCPT/DATA), consumed by the worker (Phase 3 SMTP session). It
--                               is introduced HERE because Phase 6 is where the ordering invariant
--                               against the job lease becomes load-bearing (P6-10/P6-11): it MUST
--                               stay < job_max_processing_time_sec (defaults 30 < 120) so an account
--                               hang surfaces as a worker-REPORTED timeout (scored/quarantined) rather
--                               than an unscored Control lease reclaim. A coordinated one-line Phase 3
--                               closeout-delta note points here (same pattern as P5-13's Phase 4 edit).
--
-- Grants (ADR-0008 trap): both rows land in send_managed.config, which send_managed_app already holds
-- SELECT on (send_managed_0010). No new table, no new grant; the schema-wide TestGrantsInvariant stays
-- green (a config INSERT changes no table-class privilege).
INSERT INTO send_managed.config (key, value, default_value, description, value_type) VALUES
('reclaim_tick_interval_ms', '{"v":15000}', '{"v":15000}',
 'Milliseconds between reclaim-loop poll cycles (dead-worker heartbeat scan + per-job lease-expiry backstop). Phase 6 §2.',
 'integer'),
('smtp_operation_timeout_sec', '{"v":30}', '{"v":30}',
 'Worker per-operation SMTP timeout (connect/greeting/socket-read on MAIL/RCPT/DATA). MUST stay < job_max_processing_time_sec so an account hang surfaces as a worker-reported timeout (scored/quarantined) rather than an unscored Control lease reclaim. Consumed by the worker. Phase 6 §0/§2 (P6-11), coordinated with Phase 3.',
 'integer');

INSERT INTO send_managed.schema_migrations (version) VALUES ('0018');
