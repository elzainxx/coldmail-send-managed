-- send_managed_0021_operability_config.up.sql — Phase 9.5 (§6). The ONLY structural delta this phase
-- makes: eleven additive INSERTs into the EXISTING send_managed.config table (same pattern as
-- send_managed_0010/0017/0018/0019). NO table structure change; NO column, constraint, enum, or index is
-- added or altered — this migration is fully schema-neutral (§10: "No DDL. Config seeds only.").
-- value is JSONB-wrapped {"v": ...}; default_value is never mutated (CLAUDE.md Rule 1). All eleven
-- keys are appended to Docs/config-manifest.txt + Docs/config-reference.md in the SAME change
-- (config_drift gate: 42 -> 53).
--
-- Operability controls (B1-B5, §1-§5):
--   dispatch_enabled                     : B1 global panic switch (§1.1). true = normal dispatch;
--                                          false = the tick claims/routes/pushes NOTHING; queued
--                                          jobs stay queued, nothing is lost. Consumed by the Control
--                                          at the top of every dispatch tick and the reclaim loop.
--   autoquarantine_mode                  : B2 shadow ladder (§2.1). off|shadow|enforce, default
--                                          shadow (launch-safety default — recommended disposition
--                                          #1, §9). enforce = current Phase 5 behavior; shadow =
--                                          decide-but-log-only via existing account_events columns;
--                                          off = no decision, no row. Consumed by the Control at the
--                                          Phase 5 auto-quarantine decision seam (account_effect.go).
--   autopause_enabled                    : B2 kill-switch (§2.2). true = current Phase 5 auto-pause
--                                          behavior (throttle-driven); false = the module does not
--                                          auto-pause accounts (manual pause/unpause unaffected).
--   variation_enabled                    : B2 master kill-switch (§2.3). true = current Phase 8
--                                          behavior (individual toggles apply); false = bypass the
--                                          WHOLE variation layer regardless of individual toggles —
--                                          baseline Phase 3 assembly, byte-identical.
--   alert_webhook_url                    : B4 optional outbound webhook (§4.3). "" (default) = no
--                                          webhook; conditions remain visible via the health
--                                          endpoint only. Set = POST a two-vocabulary-clean payload
--                                          on each condition's rising edge (deduped until it clears).
--   health_metrics_window_sec            : B3 rolling window (§3.2) for rate/error/shadow counts in
--                                          GET /v1/admin/health. Consumed by the admin API.
--   brain_singleton_acquire_timeout_sec  : B5 bounded-wait-then-fail (§5.2) for the Control startup
--                                          singleton advisory lock. 0 = fail-fast, no wait.
--
-- Config-not-code completeness dials (S10, S11, §2.4 — defaults = today's hardcoded constants, so
-- behavior is UNCHANGED at default; each becomes retunable without a redeploy):
--   unit_price_credits                   : S10. Pricing was a documented constant (1 credit/
--                                          recipient); promoted to a seeded key. Consumed by the
--                                          Control's quote/describe handlers only — no worker, no send-
--                                          path effect.
--   variation_comment_intensity_rates    : S11. The % of closing tags for each
--                                          variation_comment_intensity bucket (low/medium/high),
--                                          previously hardcoded 5/15/30. Consumed worker-side via the
--                                          already-existing push_job.variation block (no new wire
--                                          field mechanism).
--   variation_zero_width_rate_pct        : S11. % of word boundaries eligible for zero-width
--                                          insertion, previously hardcoded ~5. Worker-side, same
--                                          delivery mechanism as above.
--   variation_css_micro_params           : S11. CSS micro-variation pixel delta + skip-below
--                                          threshold, previously hardcoded +/-1px / skip <3px.
--                                          Worker-side, same delivery mechanism as above.
--
-- Grants (ADR-0008 trap): all eleven rows land in send_managed.config, which send_managed_app already holds
-- SELECT on (send_managed_0010). No new table, no new grant; the schema-wide TestGrantsInvariant stays
-- green (a config INSERT changes no table-class privilege). Phase 9.5 adds NO schema object, so
-- there is no new grant to enumerate. No locked enum, column, or constraint is touched (§10).
INSERT INTO send_managed.config (key, value, default_value, description, value_type) VALUES
('dispatch_enabled', '{"v":true}', '{"v":true}',
 'Global dispatch panic switch. false = the tick claims/routes/pushes nothing; queued jobs stay queued, nothing lost. Reclaim/outcome/heartbeat/config-read all continue. Phase 9.5 B1 §1.',
 'boolean'),
('autoquarantine_mode', '{"v":"shadow"}', '{"v":"shadow"}',
 'Auto-quarantine shadow ladder: off|shadow|enforce. shadow (launch-safety default) decides but does not act, logging an account_events row; enforce is the Phase 5 behavior; off disables the decision entirely. Phase 9.5 B2 §2.1.',
 'string'),
('autopause_enabled', '{"v":true}', '{"v":true}',
 'Auto-pause (throttle) kill-switch. false = the module does not auto-pause accounts on a throttle outcome (manual pause/unpause unaffected). Phase 9.5 B2 §2.2.',
 'boolean'),
('variation_enabled', '{"v":true}', '{"v":true}',
 'Content-variation master kill-switch. false = bypass the entire Phase 8 variation layer regardless of individual toggles (baseline Phase 3 assembly, byte-identical). Phase 9.5 B2 §2.3.',
 'boolean'),
('alert_webhook_url', '{"v":""}', '{"v":""}',
 'Optional outbound alert webhook URL. Empty = disabled (conditions remain visible via GET /v1/admin/health only). Set = POST a two-vocabulary-clean JSON payload on each condition rising edge, deduped until it clears. Phase 9.5 B4 §4.3.',
 'string'),
('health_metrics_window_sec', '{"v":3600}', '{"v":3600}',
 'Rolling window (seconds) for the rate/error/shadow counts in GET /v1/admin/health. Phase 9.5 B3 §3.2.',
 'integer'),
('brain_singleton_acquire_timeout_sec', '{"v":60}', '{"v":60}',
 'Bounded wait (seconds) for the Control startup singleton advisory lock before exiting non-zero. 0 = fail-fast, no wait. Recommended >= shutdown_drain_timeout_sec (operational guidance, not a reject-enforced invariant). Phase 9.5 B5 §5.2.',
 'integer'),
('unit_price_credits', '{"v":1}', '{"v":1}',
 'Credits charged per recipient (quote/describe). Was a documented constant; default unchanged, now retunable without a redeploy. Phase 9.5 S10 §2.4.',
 'integer'),
('variation_comment_intensity_rates', '{"v":{"low":5,"medium":15,"high":30}}', '{"v":{"low":5,"medium":15,"high":30}}',
 'Percent of closing tags injected for each variation_comment_intensity bucket. Was hardcoded 5/15/30; default unchanged. Worker-consumed via push_job.variation. Phase 9.5 S11 §2.4.',
 'json'),
('variation_zero_width_rate_pct', '{"v":5}', '{"v":5}',
 'Percent of word boundaries eligible for zero-width insertion (variation_zero_width). Was hardcoded ~5; default unchanged. Worker-consumed via push_job.variation. Phase 9.5 S11 §2.4.',
 'integer'),
('variation_css_micro_params', '{"v":{"delta_px":1,"skip_below_px":3}}', '{"v":{"delta_px":1,"skip_below_px":3}}',
 'CSS micro-variation pixel delta and skip-below threshold (variation_css_micro). Was hardcoded +/-1px / skip <3px; default unchanged. Worker-consumed via push_job.variation. Phase 9.5 S11 §2.4.',
 'json');

INSERT INTO send_managed.schema_migrations (version) VALUES ('0021');
