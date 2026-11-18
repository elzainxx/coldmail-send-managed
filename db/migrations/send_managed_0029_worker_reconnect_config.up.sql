-- send_managed_0029_worker_reconnect_config.up.sql -- Phase 3 close-out B1: THE WORKER NEVER GIVES UP.
--
-- Until this migration the Node worker made about thirty one-second reconnect attempts to the Control and then
-- EXITED (1) for its supervisor to restart; an outage longer than that needed the worker restarted. The worker
-- now reconnects for ever (until its own shutdown) with a capped exponential backoff plus jitter
-- (services/worker/src/reconnect.js). The three numbers that shape that wait are config, not code (CLAUDE.md
-- Rule 1), so they are seeded here. The worker has NO database, so the Control reads them and DELIVERS them on
-- register_ack (workerlink/stream.go readReconnectDials, the same route box_outcome_buffer_ttl_sec takes); the
-- worker adopts them for every wait that follows. Its WORKER_RECONNECT_* environment variables, when set, win
-- over the delivered values (an operator / test-harness override), and a worker that has never registered uses
-- the same numbers as built-in defaults.
--
--   wait(n) = min(cap, base * 2^(n-1))  *  (1 + jitter * u)      u uniform in [0, 1)
--
-- so no wait ever exceeds cap * (1 + jitter), and the attempt counter resets to 1 after every success.
--
-- ADDITIVE and schema-neutral: three config rows, no table, column, enum or constraint. An older Control
-- ignores them; an older worker ignores the register_ack fields that carry them. Defaults are suited to
-- production (1 s, 30 s, 20 %). The MINIMUM LEGAL values (10 ms, 10 ms, 0 %) exist so a test harness can run the
-- worker at 50 ms / 200 ms and see more than thirty attempts in seconds; a PUT outside the legal range is
-- REJECTED (adminapi/config_validate.go), never clamped.

INSERT INTO send_managed.config (key, value, default_value, description, value_type) VALUES
('worker_reconnect_base_ms', '{"v":1000}', '{"v":1000}',
 'Worker reconnect backoff, first wait (Phase 3 close-out B1): milliseconds the worker waits after its FIRST failed attempt to reach or register with the Control before trying again; each further consecutive failure doubles the wait up to worker_reconnect_cap_ms. The worker never gives up reconnecting. Delivered to the worker on register_ack and adopted for every wait after that; a worker that has not yet registered, or that has WORKER_RECONNECT_BASE_MS set, uses its own value. Integer 10-60000 (minimum legal value 10); a value outside that range is rejected, not clamped. 1000 by default.', 'integer'),
('worker_reconnect_cap_ms', '{"v":30000}', '{"v":30000}',
 'Worker reconnect backoff, cap (Phase 3 close-out B1): the largest un-jittered wait in milliseconds between two reconnect attempts, however long the Control stays down; the doubling from worker_reconnect_base_ms stops here, so with jitter no wait ever exceeds cap x (1 + worker_reconnect_jitter_pct/100). It is also how long, at most, a recovered Control waits for a worker that was waiting out an outage. A base above the cap simply makes every wait the cap. Integer 10-600000 (minimum legal value 10); rejected, not clamped, outside that range. 30000 (30 s) by default.', 'integer'),
('worker_reconnect_jitter_pct', '{"v":20}', '{"v":20}',
 'Worker reconnect backoff, jitter (Phase 3 close-out B1): each wait is lengthened by a random 0 to this percentage of itself (wait x (1 + jitter x u), u uniform in [0,1)), so a fleet of workers that lost the same Control together does not redial on the same tick. 0 means no jitter and is a legal value that reaches the worker as 0. Integer 0-100; rejected, not clamped, outside that range. 20 by default.', 'integer');

INSERT INTO send_managed.schema_migrations (version) VALUES ('0029');
