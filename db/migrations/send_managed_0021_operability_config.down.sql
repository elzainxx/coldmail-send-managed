-- send_managed_0021_operability_config.down.sql — reverse of the Phase 9.5 config seed. Deletes exactly
-- the eleven keys send_managed_0021 inserted and un-records the version. Additive-only + reversible (DoD).
DELETE FROM send_managed.config WHERE key IN
 ('dispatch_enabled','autoquarantine_mode','autopause_enabled','variation_enabled',
  'alert_webhook_url','health_metrics_window_sec','brain_singleton_acquire_timeout_sec',
  'unit_price_credits','variation_comment_intensity_rates','variation_zero_width_rate_pct',
  'variation_css_micro_params');
DELETE FROM send_managed.schema_migrations WHERE version = '0021';
