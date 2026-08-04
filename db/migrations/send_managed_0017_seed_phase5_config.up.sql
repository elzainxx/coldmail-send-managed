-- send_managed_0017_seed_phase5_config.up.sql — Phase 5 (§7, §9, P5-7): three new seeded config keys.
-- Additive INSERTs into the EXISTING send_managed.config table (same pattern as send_managed_0010). No table
-- structure changes. value is JSONB-wrapped {"v": ...}; default_value is never mutated (Rule 1).
-- These three keys are appended to Docs/config-manifest.txt + Docs/config-reference.md in the SAME
-- change (config_drift gate). quarantine_threshold (30) and retry_count_4xx (3) already exist from
-- send_managed_0010 — Phase 5 seeds NO duplicate.
INSERT INTO send_managed.config (key, value, default_value, description, value_type) VALUES
('status_score_weights',
   '{"v":{"w_fail":60,"w_prox":15,"w_recency":25,"fail_saturation":5,"recency_horizon_hours":72}}',
   '{"v":{"w_fail":60,"w_prox":15,"w_recency":25,"fail_saturation":5,"recency_horizon_hours":72}}',
   'Weights and shape parameters for the account status score (Phase 5 §3.2): w_fail/w_prox/w_recency penalty weights, fail_saturation (failures for full failure penalty), recency_horizon_hours.', 'json'),
('retry_backoff_schedule',
   '{"v":{"ladder_seconds":[30,120,600],"max_seconds":600}}',
   '{"v":{"ladder_seconds":[30,120,600],"max_seconds":600}}',
   'Exponential backoff ladder (seconds) for transient/timeout/unknown requeues, capped at max_seconds (Phase 5 §6.4).', 'json'),
('throttle_pause_seconds',
   '{"v":900}', '{"v":900}',
   'Duration (seconds) of a throttle pause before lazy auto-clear, and the throttle-class requeue delay (Phase 5 §4.3/§6.4).', 'integer');

INSERT INTO send_managed.schema_migrations (version) VALUES ('0017');
