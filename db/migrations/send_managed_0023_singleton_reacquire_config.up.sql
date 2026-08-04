-- send_managed_0023_singleton_reacquire_config.up.sql — G11-OD-1 / ADR-0011 remediation (option B, ruled
-- by the user 2026-07-29). ONE additive INSERT into the EXISTING send_managed.config table (same pattern
-- as send_managed_0010/0017/0018/0019/0021). NO table structure change; NO column, constraint, enum, or
-- index is added or altered — this migration is schema-neutral, config seed only.
-- value is JSONB-wrapped {"v": ...}; default_value is never mutated (CLAUDE.md Rule 1). The key is
-- appended to Docs/config-manifest.txt + Docs/config-reference.md in the SAME change
-- (config_drift gate: 53 -> 54).
--
-- WHY A NEW KEY. ADR-0011 option B keeps a Control that has LOST the singleton advisory lock alive but
-- DISPATCHING NOTHING, with the keeper re-attempting acquisition on a bounded retry. That retry
-- cadence is a tunable, so Rule 1 makes it a seeded row rather than a Go constant: too fast and a
-- long DB outage hammers a dead server with try-lock attempts, too slow and a Control that could
-- safely resume sits idle for longer than it needs to. It is deliberately SEPARATE from
-- brain_singleton_acquire_timeout_sec, which bounds the STARTUP wait before exiting non-zero — a
-- different event (startup CONTENTION vs mid-life LOSS) with a different correct answer: startup
-- gives up and exits, mid-life never gives up because there is nothing to exit into that a
-- supervisor restart would not already do better.
--
-- DEFAULT 5 seconds, matching the keeper's existing 5s liveness-verify ticker: the keeper already
-- wakes on that cadence, so a re-acquire attempt costs one extra trivial query on a tick it was
-- taking anyway. A single pg_try_advisory_lock is non-blocking and cheap; the cost of the retry is
-- bounded by this interval, not by any lock wait.

INSERT INTO send_managed.config (key, value, default_value, description, value_type) VALUES
('brain_singleton_reacquire_interval_sec', '{"v":5}', '{"v":5}',
 'How often (seconds) a Control that LOST the singleton advisory lock mid-life re-attempts pg_try_advisory_lock. While the lock is not held the dispatch/reclaim/retention loops are PAUSED (no claim, no push, no reclaim, no sweep) and readyz reports not_ready; the outcome path stays live so a worker can still report an in-flight send. 0 = never re-acquire (the Control stays paused until restarted). Distinct from brain_singleton_acquire_timeout_sec, which bounds the STARTUP wait. G11-OD-1 / ADR-0011 option B.',
 'integer');

INSERT INTO send_managed.schema_migrations (version) VALUES ('0023');
