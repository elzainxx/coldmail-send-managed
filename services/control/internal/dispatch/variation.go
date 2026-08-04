package dispatch

import "github.com/coldmail-send-managed/control/internal/workerproto"

// variation.go — Phase 8 (P8-2/P8-3/P8-4): the Control's half of the content-variation wire
// contract. The Control reads the GLOBAL send_managed.config toggle set (Config, populated by LoadConfig
// from the seventeen send_managed_0020 keys — amended from fourteen post-Chunk-5, P8-OD-1 RULED-WIDEN)
// and stamps a RESOLVED snapshot onto every push_job (§2.2); the worker never reads config
// (services/worker/src/variation.js's resolveVariation/STAGES is the worker-side identity-function
// guarantee that makes an absent/all-default snapshot safe either way).
//
// variationSnapshot performs NO decision logic beyond copying cfg's already-loaded values into the
// wire shape — every toggle's semantics live worker-side. GLOBAL scope (§3): the same snapshot is
// stamped on every push_job in a given LoadConfig lifetime; there is no per-pool/per-campaign
// resolution (flagged future refinement, §9 P8-3 — not built).
//
// Phase 9.5 B2 §2.3: variation_enabled is the master kill-switch. false bypasses the ENTIRE layer
// regardless of individual toggles by returning nil here — the same "absent variation block" shape
// the worker's own resolveVariation/STAGES already treat as the Phase-3 baseline identity (§4.2), so
// this is not a NEW code path worker-side, just never sending the block at all.
func variationSnapshot(cfg Config) *workerproto.Variation {
	if !cfg.VariationEnabled {
		return nil
	}
	return &workerproto.Variation{
		HTMLMinify:       cfg.VariationHTMLMinify,
		CommentInjection: cfg.VariationCommentInjection,
		CommentIntensity: cfg.VariationCommentIntensity,
		// Phase 9.5 S11 (§2.4): variation_comment_intensity_rates and the CSS skip-below-px
		// threshold, stamped onto the already-existing wire block (no new mechanism). The
		// zero-width RATE and the CSS magnitude stay driven by Phase 8's own already-wired
		// variation_zero_width_rate/variation_css_magnitude_px (P8-OD-1) — send_managed_0021's
		// variation_zero_width_rate_pct and variation_css_micro_params.delta_px are documented
		// duplicates of those same two parameters under a different unit/shape (P9.5-OD-3, RULED);
		// only the genuinely new sub-values (the intensity rate table, the skip-below threshold)
		// are wired here.
		CommentIntensityRates: &workerproto.CommentIntensityRates{
			Low:    cfg.VariationCommentIntensityRates.Low,
			Medium: cfg.VariationCommentIntensityRates.Medium,
			High:   cfg.VariationCommentIntensityRates.High,
		},
		CSSMicro:             cfg.VariationCSSMicro,
		CSSMagnitudePx:       cfg.VariationCSSMagnitudePx,
		CSSSkipBelowPx:       cfg.VariationCSSMicroParams.SkipBelowPx,
		UniqueURLParams:      cfg.VariationUniqueURLParams,
		ZeroWidth:            cfg.VariationZeroWidth,
		ZeroWidthRate:        cfg.VariationZeroWidthRate,
		EncodingRotation:     cfg.VariationEncodingRotation,
		MIMEBoundaryRotation: cfg.VariationMIMEBoundaryRotation,
		DynamicPreheader:     cfg.VariationDynamicPreheader,
		PreheaderTemplates:   cfg.VariationPreheaderTemplates,
		HeaderStyle:          cfg.VariationHeaderStyle,
		UserAgentInjection:   cfg.VariationUserAgentInjection,
		UserAgentRate:        cfg.VariationUserAgentRate,
		Precedence:           cfg.VariationPrecedence,
		ThreadIndex:          cfg.VariationThreadIndex,
	}
}
