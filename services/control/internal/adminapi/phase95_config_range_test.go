package adminapi

// phase95_config_range_test.go — review gate R3 finding (Phase 9.5 closeout): unit_price_credits,
// health_metrics_window_sec, brain_singleton_acquire_timeout_sec, variation_comment_intensity_rates,
// variation_zero_width_rate_pct, and variation_css_micro_params had no write-time semantic range
// validation despite the spec's own §6 table documenting explicit allowed ranges for each — a
// negative value or a malformed inner-shape object was silently accepted. Real Postgres, real HTTP
// against the actual production Server (httptest) — no mocks (CLAUDE.md Rule 3).

import "testing"

func TestPhase95_UnitPriceCreditsRangeEnforced(t *testing.T) {
	e := newTestEnv(t, "p95rangeunit")
	e.do(t, "PUT", "/v1/admin/config/unit_price_credits", map[string]any{"value": -1}).requireStatus(t, 400)
	e.do(t, "PUT", "/v1/admin/config/unit_price_credits", map[string]any{"value": 1001}).requireStatus(t, 400)
	e.do(t, "PUT", "/v1/admin/config/unit_price_credits", map[string]any{"value": 0}).requireStatus(t, 200) // boundary: free tier is legitimate
	e.do(t, "PUT", "/v1/admin/config/unit_price_credits", map[string]any{"value": 1000}).requireStatus(t, 200)
}

func TestPhase95_HealthMetricsWindowSecRangeEnforced(t *testing.T) {
	e := newTestEnv(t, "p95rangewindow")
	e.do(t, "PUT", "/v1/admin/config/health_metrics_window_sec", map[string]any{"value": 59}).requireStatus(t, 400)
	e.do(t, "PUT", "/v1/admin/config/health_metrics_window_sec", map[string]any{"value": 86401}).requireStatus(t, 400)
	e.do(t, "PUT", "/v1/admin/config/health_metrics_window_sec", map[string]any{"value": 60}).requireStatus(t, 200)
	e.do(t, "PUT", "/v1/admin/config/health_metrics_window_sec", map[string]any{"value": 86400}).requireStatus(t, 200)
}

func TestPhase95_ControlSingletonAcquireTimeoutSecRangeEnforced(t *testing.T) {
	e := newTestEnv(t, "p95rangesingleton")
	e.do(t, "PUT", "/v1/admin/config/brain_singleton_acquire_timeout_sec", map[string]any{"value": -1}).requireStatus(t, 400)
	e.do(t, "PUT", "/v1/admin/config/brain_singleton_acquire_timeout_sec", map[string]any{"value": 601}).requireStatus(t, 400)
	e.do(t, "PUT", "/v1/admin/config/brain_singleton_acquire_timeout_sec", map[string]any{"value": 0}).requireStatus(t, 200) // boundary: fail-fast is legitimate
	e.do(t, "PUT", "/v1/admin/config/brain_singleton_acquire_timeout_sec", map[string]any{"value": 600}).requireStatus(t, 200)
}

func TestPhase95_VariationZeroWidthRatePctRangeEnforced(t *testing.T) {
	e := newTestEnv(t, "p95rangezwrpct")
	e.do(t, "PUT", "/v1/admin/config/variation_zero_width_rate_pct", map[string]any{"value": -1}).requireStatus(t, 400)
	e.do(t, "PUT", "/v1/admin/config/variation_zero_width_rate_pct", map[string]any{"value": 101}).requireStatus(t, 400)
	e.do(t, "PUT", "/v1/admin/config/variation_zero_width_rate_pct", map[string]any{"value": 0}).requireStatus(t, 200)
	e.do(t, "PUT", "/v1/admin/config/variation_zero_width_rate_pct", map[string]any{"value": 100}).requireStatus(t, 200)
}

func TestPhase95_VariationCommentIntensityRatesShapeEnforced(t *testing.T) {
	e := newTestEnv(t, "p95rangeintensity")
	// Missing bucket.
	e.do(t, "PUT", "/v1/admin/config/variation_comment_intensity_rates",
		map[string]any{"value": map[string]any{"low": 5, "medium": 15}}).requireStatus(t, 400)
	// Out-of-range bucket.
	e.do(t, "PUT", "/v1/admin/config/variation_comment_intensity_rates",
		map[string]any{"value": map[string]any{"low": 5, "medium": 15, "high": 200}}).requireStatus(t, 400)
	// Valid.
	e.do(t, "PUT", "/v1/admin/config/variation_comment_intensity_rates",
		map[string]any{"value": map[string]any{"low": 10, "medium": 20, "high": 40}}).requireStatus(t, 200)
}

func TestPhase95_VariationCSSMicroParamsShapeEnforced(t *testing.T) {
	e := newTestEnv(t, "p95rangecssmicro")
	// delta_px out of range (max 5).
	e.do(t, "PUT", "/v1/admin/config/variation_css_micro_params",
		map[string]any{"value": map[string]any{"delta_px": 6, "skip_below_px": 3}}).requireStatus(t, 400)
	// skip_below_px out of range (max 20).
	e.do(t, "PUT", "/v1/admin/config/variation_css_micro_params",
		map[string]any{"value": map[string]any{"delta_px": 1, "skip_below_px": 21}}).requireStatus(t, 400)
	// Valid, including the boundary skip_below_px=0 (a legitimate "vary everything" choice).
	e.do(t, "PUT", "/v1/admin/config/variation_css_micro_params",
		map[string]any{"value": map[string]any{"delta_px": 5, "skip_below_px": 0}}).requireStatus(t, 200)
}
