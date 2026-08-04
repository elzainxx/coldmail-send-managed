package dispatch

import (
	"context"
	"encoding/json"
	"testing"
)

// variation_test.go — Phase 8 CHUNK 5: proves the Control reads the LIVE send_managed.config content-
// variation keys (send_managed_0020, §6.1) — not a hardcoded snapshot — and stamps the resolved values
// onto push_job.variation. Real Postgres, real Dispatcher, no mocks (CLAUDE.md Rule 3).
//
// This is the config-not-code closing proof for Phase 8: Chunk 2 shipped a wire-only literal
// snapshot (defaultVariationSnapshot, since no seeded row existed yet); this test asserts the
// REPLACEMENT (variationSnapshot(cfg), config.go's seventeen readers — amended from fourteen,
// P8-OD-1 RULED-WIDEN) genuinely reads what an operator writes, exactly the same freshness
// contract every other dispatch dial already has (TryDispatch calls LoadConfig fresh — §0 OD-14,
// "a config change takes effect on the very next attempt, no redeploy").

// TestVariation_DefaultConfigYieldsNeutralSnapshot — the §4 identity baseline at the LIVE-config
// layer (not just the wire-shape layer Chunk 2 already covers): with send_managed_0020's seeded
// defaults untouched, every field of the stamped push_job.variation must equal the Phase-3-neutral
// value the worker's own VARIATION_DEFAULTS expects.
func TestVariation_DefaultConfigYieldsNeutralSnapshot(t *testing.T) {
	e := newDispatchEnv(t, "variationdflt")
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	job := e.seedQueuedJob(t, "variation-default@example.com")
	e.dispatchToSending(t, job)

	pj, ok := e.px.lastPush()
	if !ok {
		t.Fatal("no push recorded")
	}
	v := pj.Variation
	if v == nil {
		t.Fatal("push_job.variation is nil — the Control must always stamp a snapshot (Chunk 2/5), even at all-default")
	}
	switch {
	case v.HTMLMinify, v.CommentInjection, v.CSSMicro, v.UniqueURLParams, v.ZeroWidth,
		v.EncodingRotation, v.MIMEBoundaryRotation, v.DynamicPreheader, v.UserAgentInjection, v.ThreadIndex:
		t.Fatalf("a boolean toggle is true at the seeded default: %+v", v)
	}
	if v.CommentIntensity != "low" {
		t.Errorf("comment_intensity = %q, want the seeded default %q", v.CommentIntensity, "low")
	}
	if v.HeaderStyle != "none" {
		t.Errorf("header_style = %q, want the seeded default %q (Phase 3 identity)", v.HeaderStyle, "none")
	}
	if v.Precedence != "auto" {
		t.Errorf("precedence = %q, want the seeded default %q (Phase 3 identity)", v.Precedence, "auto")
	}
	if len(v.PreheaderTemplates) != 7 {
		t.Errorf("preheader_templates has %d entries, want the seeded 7 token-free defaults", len(v.PreheaderTemplates))
	}
	// The three amended P8-OD-1 rate/magnitude parameters at their seeded defaults.
	if v.CSSMagnitudePx != 1 {
		t.Errorf("css_magnitude_px = %d, want the seeded default 1", v.CSSMagnitudePx)
	}
	if v.ZeroWidthRate != 0.05 {
		t.Errorf("zero_width_rate = %v, want the seeded default 0.05", v.ZeroWidthRate)
	}
	if v.UserAgentRate != 0.3 {
		t.Errorf("user_agent_rate = %v, want the seeded default 0.3", v.UserAgentRate)
	}
}

// TestVariation_LiveConfigChangeReachesNextPushJob — the config-not-code proof itself: flip a
// boolean toggle + a string toggle via a direct send_managed.config write (the same write shape the
// admin API's PUT /v1/admin/config/{key} produces), then dispatch a SECOND job with NO Control
// restart. Its push_job.variation MUST reflect the new values — proving the read path is live
// config, not a cached/hardcoded snapshot.
func TestVariation_LiveConfigChangeReachesNextPushJob(t *testing.T) {
	e := newDispatchEnv(t, "variationlive")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	// First job, still at the seeded defaults — the baseline this test's change must differ from.
	jobBefore := e.seedQueuedJob(t, "before@example.com")
	e.dispatchToSending(t, jobBefore)
	pjBefore, _ := e.px.lastPush()
	if pjBefore.Variation == nil || pjBefore.Variation.HTMLMinify {
		t.Fatalf("precondition: html_minify already true before the config change — the test would be vacuous")
	}

	// Live operator write: flip variation_html_minify on and variation_header_style to "gmail".
	e.setConfigBool(t, "variation_html_minify", true)
	e.setConfigStr(t, "variation_header_style", "gmail")

	e.px.freeAllWorkers()
	jobAfter := e.seedQueuedJob(t, "after@example.com")
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("dispatch after config change: %v", err)
	}
	if st, _, _, _, _ := e.jobState(t, jobAfter); st != "sending" {
		t.Fatalf("precondition: state=%s, want sending", st)
	}
	pjAfter, ok := e.px.lastPush()
	if !ok || pjAfter.JobID != jobAfter {
		t.Fatalf("push after config change did not target the expected job (got %+v)", pjAfter)
	}
	if pjAfter.Variation == nil {
		t.Fatal("push_job.variation is nil after the config change")
	}
	if !pjAfter.Variation.HTMLMinify {
		t.Error("html_minify was set to true in send_managed.config but the pushed snapshot still says false — the Control is not reading live config")
	}
	if pjAfter.Variation.HeaderStyle != "gmail" {
		t.Errorf("header_style = %q after the config change, want %q", pjAfter.Variation.HeaderStyle, "gmail")
	}
	// The FIRST job's already-pushed snapshot must be untouched by the later config write — proving
	// this is a per-dispatch-fresh stamp (§2.1), not a shared mutable struct both pushes alias.
	if pjBefore.Variation.HTMLMinify {
		t.Error("the EARLIER push's recorded variation snapshot mutated after a later config change — snapshots must be independent per push")
	}
}

// TestVariation_RateParametersReachPushJob — the three amended P8-OD-1 parameters
// (variation_css_magnitude_px, variation_zero_width_rate, variation_user_agent_rate) are read live
// and reach the wire, closing the same config-not-code loop TestVariation_LiveConfigChangeReachesNextPushJob
// proves for the boolean/string keys — for the numeric ones specifically, since they are Phase 8's
// first non-integer/non-boolean dials and go through a different reader (readFloatKey).
func TestVariation_RateParametersReachPushJob(t *testing.T) {
	e := newDispatchEnv(t, "variationrates")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	e.setConfigInt(t, "variation_css_magnitude_px", 7)
	e.setConfigFloat(t, "variation_zero_width_rate", 0.42)
	e.setConfigFloat(t, "variation_user_agent_rate", 0.99)

	job := e.seedQueuedJob(t, "rates@example.com")
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if st, _, _, _, _ := e.jobState(t, job); st != "sending" {
		t.Fatalf("precondition: state=%s, want sending", st)
	}
	pj, ok := e.px.lastPush()
	if !ok || pj.JobID != job {
		t.Fatalf("push did not target the expected job (got %+v)", pj)
	}
	if pj.Variation.CSSMagnitudePx != 7 {
		t.Errorf("css_magnitude_px = %d, want 7", pj.Variation.CSSMagnitudePx)
	}
	if pj.Variation.ZeroWidthRate != 0.42 {
		t.Errorf("zero_width_rate = %v, want 0.42", pj.Variation.ZeroWidthRate)
	}
	if pj.Variation.UserAgentRate != 0.99 {
		t.Errorf("user_agent_rate = %v, want 0.99", pj.Variation.UserAgentRate)
	}
}

// TestVariation_S11RatesReachPushJob — Phase 9.5 S11 (§2.4): the two genuinely-new sub-values
// (variation_comment_intensity_rates' per-bucket table, variation_css_micro_params.skip_below_px)
// are read live and stamped onto push_job.variation. The other two sub-values in the S11 keys
// (variation_zero_width_rate_pct, variation_css_micro_params.delta_px) are documented duplicates
// of Phase 8's already-wired variation_zero_width_rate/variation_css_magnitude_px (P9.5-OD-3,
// RULED) and are deliberately NOT re-tested here — TestVariation_RateParametersReachPushJob above
// already covers those two dials.
func TestVariation_S11RatesReachPushJob(t *testing.T) {
	e := newDispatchEnv(t, "variations11")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	if _, err := e.admin.Exec(ctx,
		`UPDATE send_managed.config SET value = jsonb_build_object('v', jsonb_build_object('low',10,'medium',20,'high',40)) WHERE key='variation_comment_intensity_rates'`); err != nil {
		t.Fatalf("set variation_comment_intensity_rates: %v", err)
	}
	if _, err := e.admin.Exec(ctx,
		`UPDATE send_managed.config SET value = jsonb_build_object('v', jsonb_build_object('delta_px',1,'skip_below_px',9)) WHERE key='variation_css_micro_params'`); err != nil {
		t.Fatalf("set variation_css_micro_params: %v", err)
	}

	job := e.seedQueuedJob(t, "s11rates@example.com")
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if st, _, _, _, _ := e.jobState(t, job); st != "sending" {
		t.Fatalf("precondition: state=%s, want sending", st)
	}
	pj, ok := e.px.lastPush()
	if !ok || pj.JobID != job {
		t.Fatalf("push did not target the expected job (got %+v)", pj)
	}
	if pj.Variation.CommentIntensityRates == nil {
		t.Fatal("comment_intensity_rates is nil on the wire — S11 must always stamp the table")
	}
	if got := pj.Variation.CommentIntensityRates; got.Low != 10 || got.Medium != 20 || got.High != 40 {
		t.Errorf("comment_intensity_rates = %+v, want {10 20 40}", got)
	}
	if pj.Variation.CSSSkipBelowPx != 9 {
		t.Errorf("css_skip_below_px = %d, want 9", pj.Variation.CSSSkipBelowPx)
	}
}

// TestVariation_CSSSkipBelowPxZeroSurvivesJSONRoundTrip — the boundary value skip_below_px=0 (a
// legitimate operator choice per §6's documented 0-20 range: "vary every declaration, no matter
// how small") must survive all the way to the ACTUAL JSON BYTES a worker would receive over the wire
// — not just the in-memory Go struct the recordingPusher test double captures before marshaling
// (TestVariation_S11RatesReachPushJob above never serializes anything, so it could never have
// caught the omitempty regression R3's review gate found: CSSSkipBelowPx's json tag silently
// discarding an operator-set 0). This test closes that gap with a real json.Marshal/Unmarshal.
func TestVariation_CSSSkipBelowPxZeroSurvivesJSONRoundTrip(t *testing.T) {
	e := newDispatchEnv(t, "variations11zero")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	if _, err := e.admin.Exec(ctx,
		`UPDATE send_managed.config SET value = jsonb_build_object('v', jsonb_build_object('delta_px',1,'skip_below_px',0)) WHERE key='variation_css_micro_params'`); err != nil {
		t.Fatalf("set variation_css_micro_params: %v", err)
	}

	job := e.seedQueuedJob(t, "s11zero@example.com")
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	pj, ok := e.px.lastPush()
	if !ok || pj.JobID != job {
		t.Fatalf("push did not target the expected job (got %+v)", pj)
	}
	if pj.Variation.CSSSkipBelowPx != 0 {
		t.Fatalf("in-memory css_skip_below_px = %d, want 0 (precondition)", pj.Variation.CSSSkipBelowPx)
	}

	// The actual regression check: marshal the pushed Variation to JSON (what would really cross
	// the wire to a worker) and confirm css_skip_below_px is PRESENT and reads back as 0 — not absent.
	raw, err := json.Marshal(pj.Variation)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(raw, &asMap); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}
	val, present := asMap["css_skip_below_px"]
	if !present {
		t.Fatalf("css_skip_below_px is ABSENT from the wire JSON at value 0 — an omitempty tag is silently discarding a legitimate operator-set 0: %s", raw)
	}
	if f, ok := val.(float64); !ok || f != 0 {
		t.Fatalf("css_skip_below_px = %v on the wire, want the literal 0: %s", val, raw)
	}
}

// TestVariation_PreheaderTemplatesOverrideReachesPushJob — the array<string> toggle-parameter
// reads correctly too (not just booleans/strings), and an operator-supplied override replaces the
// seeded default list rather than merging with it.
func TestVariation_PreheaderTemplatesOverrideReachesPushJob(t *testing.T) {
	e := newDispatchEnv(t, "variationpreheader")
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	if _, err := e.admin.Exec(context.Background(),
		`UPDATE send_managed.config SET value = jsonb_build_object('v', $1::jsonb), updated_at = NOW() WHERE key = 'variation_preheader_templates'`,
		`["Custom filler one", "Custom filler two"]`); err != nil {
		t.Fatalf("set preheader templates: %v", err)
	}

	job := e.seedQueuedJob(t, "preheader-override@example.com")
	e.dispatchToSending(t, job)
	pj, ok := e.px.lastPush()
	if !ok {
		t.Fatal("no push recorded")
	}
	if got := pj.Variation.PreheaderTemplates; len(got) != 2 || got[0] != "Custom filler one" || got[1] != "Custom filler two" {
		t.Fatalf("preheader_templates = %v, want the operator override (2 custom entries)", got)
	}
}
