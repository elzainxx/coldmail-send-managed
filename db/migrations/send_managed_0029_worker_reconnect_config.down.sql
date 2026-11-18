-- send_managed_0029_worker_reconnect_config.down.sql -- reverse send_managed_0029 (dev/CI only; prod is
-- forward-only): remove EXACTLY the three seeded reconnect dial rows, and nothing else.
--
-- The Control reads these rows on register_ack and treats an absent or unreadable one as "do not deliver
-- it" (the worker then keeps its environment override or built-in default), so, unlike the keys LoadConfig
-- requires, running this down without reverting the source leaves a Control that still starts and a worker
-- that still reconnects (with its built-in 1 s / 30 s / 20 %).

DELETE FROM send_managed.config WHERE key IN ('worker_reconnect_base_ms', 'worker_reconnect_cap_ms', 'worker_reconnect_jitter_pct');

DELETE FROM send_managed.schema_migrations WHERE version = '0029';
