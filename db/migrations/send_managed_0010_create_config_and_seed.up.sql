-- send_managed_0010_create_config_and_seed.up.sql — Phase 1: send_managed.config + the 14 foundation keys.
-- Config-not-code (CLAUDE.md Rule 1): every tunable is a seeded row with a documented
-- default_value + value_type. This is `send_managed_0010` exactly as Phase 9 §4.1 locks ("Phase 1 —
-- send_managed_0010 (14 keys, the foundation)"). value is JSONB-wrapped {"v": ...}. default_value is
-- never mutated. Owned by send_managed_owner. These 14 keys are appended to Docs/config-manifest.txt
-- and Docs/config-reference.md in the same change (config_drift gate).

CREATE TABLE send_managed.config (
    -- Identity
    key TEXT PRIMARY KEY,

    -- Value and default
    value JSONB NOT NULL,
    default_value JSONB NOT NULL,

    -- Documentation
    description TEXT NOT NULL,
    value_type TEXT NOT NULL,

    -- Metadata
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by UUID
);

ALTER TABLE send_managed.config OWNER TO send_managed_owner;

INSERT INTO send_managed.config (key, value, default_value, description, value_type) VALUES
('daily_limit_default', '{"v": 500}', '{"v": 500}', 'Default max sends per account per day', 'integer'),
('daily_reset_timezone', '{"v": "UTC"}', '{"v": "UTC"}', 'Timezone for daily reset schedule (IANA tz name)', 'string'),
('daily_reset_hour', '{"v": 0}', '{"v": 0}', 'Hour of day (0-23) when daily counters reset', 'integer'),
('quarantine_threshold', '{"v": 30}', '{"v": 30}', 'account_status_score below which account is auto-quarantined', 'integer'),
('retry_count_4xx', '{"v": 3}', '{"v": 3}', 'Number of retries for 4xx transport errors before requeue', 'integer'),
('dispatch_tick_interval_ms', '{"v": 1000}', '{"v": 1000}', 'Milliseconds between dispatcher poll cycles', 'integer'),
('send_delay_min_sec', '{"v": 3}', '{"v": 3}', 'Minimum random delay between sends (seconds)', 'integer'),
('send_delay_max_sec', '{"v": 7}', '{"v": 7}', 'Maximum random delay between sends (seconds)', 'integer'),
('concurrency_per_user_default', '{"v": 3}', '{"v": 3}', 'Default number of parallel lanes per user', 'integer'),
('heartbeat_interval_sec', '{"v": 120}', '{"v": 120}', 'Expected interval between worker heartbeats (seconds)', 'integer'),
('heartbeat_missed_threshold', '{"v": 3}', '{"v": 3}', 'Number of missed heartbeats before worker considered stale', 'integer'),
('job_max_processing_time_sec', '{"v": 120}', '{"v": 120}', 'Per-job max processing time (lease duration)', 'integer'),
('debug_log_retention_days', '{"v": 7}', '{"v": 7}', 'Days to retain full-content debug logs before rollup', 'integer'),
('default_shared_pool_id', '{"v": null}', '{"v": null}', 'Platform-wide default shared pool ID for fallback (NULL until pool created)', 'uuid');

INSERT INTO send_managed.schema_migrations (version) VALUES ('0010');
