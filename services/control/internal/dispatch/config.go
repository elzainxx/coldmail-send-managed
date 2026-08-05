// Package dispatch is the Control's Phase-3 minimal send path: the trivial claim + single-account
// selection (§5.1), the credential handoff (decrypt in memory, push to the worker), and the outcome
// handler (idempotency check, persist provider_message_id, requeue on failure, write send_logs).
// It is deliberately NOT Phase 4: no polling loop, no routing strategy, no pool fallback, no
// round-robin, no concurrency lanes. Dispatch is event-kicked (one attempt per run) — the
// simplest mechanism that turns a queued job into one push_job (§5.1).
//
// This package is the ONLY holder of the credential.Decryptor besides main; it is never injected
// into internal/adminapi, so no operator route can reach plaintext (OD-7, structural).
package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/coldmail-send-managed/control/internal/configordering"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config holds the seeded send_managed.config values the dispatcher CONSUMES (config-not-code, §0).
// Phase 3 consumed the first two; Phase 4 additionally consumes the tick interval, the per-user
// concurrency ceiling, the daily-reset timezone, and the default shared pool id — ALL ALREADY
// seeded in Phase 1's send_managed_0010 (Phase 4 adds NO new key, §0). Every value is read at runtime
// from send_managed.config, never hardcoded.
type Config struct {
	JobMaxProcessingTimeSec   int     // job_max_processing_time_sec — the lease duration (§1.1/§5.1)
	DebugLogRetentionDays     int     // debug_log_retention_days — send_logs.ttl_expires_at (§7)
	DispatchTickIntervalMS    int     // dispatch_tick_interval_ms — the polling loop period (§1.2, Phase 4)
	ConcurrencyPerUserDefault int     // concurrency_per_user_default — the per-user lane ceiling (§6, Phase 4)
	DailyResetTimezone        string  // daily_reset_timezone — the lazy daily-reset boundary tz (§2.1, Phase 4)
	DailyResetHour            int     // daily_reset_hour — hour-of-day (0-23) the daily boundary falls at within DailyResetTimezone (§11, Phase 1; wired in the Config Consumption Sweep closeout — was seeded/validated since send_managed_0010 but never read)
	DefaultSharedPoolID       *string // default_shared_pool_id — the V1 base-routing shared target (§4.2/§4.3, Phase 4); seeded NULL, nil until an operator sets it

	// Phase 6 pacing dials (both ALREADY seeded in Phase 1's send_managed_0010; Phase 6 consumes them
	// unchanged — no re-seed, additive-only contract §5). They bound the per-lane random inter-send
	// delay drawn per send (pacing.go, §1.1).
	SendDelayMinSec int // send_delay_min_sec — lower bound of the per-lane random inter-send delay (§1.1)
	SendDelayMaxSec int // send_delay_max_sec — upper bound of the per-lane random inter-send delay (§1.1)

	// Phase 6 reclaim/heartbeat dials. reclaim_tick_interval_ms is new (send_managed_0018, P6-7); the
	// heartbeat interval/threshold are already seeded (Phase 1 send_managed_0010) and consumed unchanged
	// as the dead-worker silence window (§2.2). job_max_processing_time_sec (above) is the per-job
	// lease / Detector-B backstop deadline (§2.3).
	ReclaimTickIntervalMS    int // reclaim_tick_interval_ms — reclaim loop poll interval (§2.1)
	HeartbeatIntervalSec     int // heartbeat_interval_sec — expected heartbeat cadence (§2.2)
	HeartbeatMissedThreshold int // heartbeat_missed_threshold — missed beats before dead; window = interval×threshold (§2.2)
	SMTPOperationTimeoutSec  int // smtp_operation_timeout_sec — worker per-operation SMTP timeout (§0/§2, P6-11); MUST be < JobMaxProcessingTimeSec (enforced at load)

	// Phase 7 runtime-hardening dials (all seeded send_managed_0019; config-not-code, §5). The Control
	// consumes the rescan window, the retention sweep's cadence/batch size and the rollup
	// granularity here. box_outcome_buffer_ttl_sec is CONSUMED BY THE WORKER (delivered on register_ack,
	// §1.3) but is READ here too, because the P7-11 ordering invariant is validated at config-LOAD
	// against StartupReconnectWindowSec — the Control must see both keys together to reject an unsafe
	// pair. shutdown_drain_timeout_sec bounds the graceful-shutdown drain (§4.1).
	StartupReconnectWindowSec int    // startup_reconnect_window_sec — rescan reconnect-reconciliation window (§1.2)
	WorkerOutcomeBufferTTLSec int    // box_outcome_buffer_ttl_sec — bound on the worker's outcome buffer (§1.3, P7-4); INVARIANT: >= StartupReconnectWindowSec
	ShutdownDrainTimeoutSec   int    // shutdown_drain_timeout_sec — max SIGTERM drain wait before leave-in-place (§4.1, P7-5)
	RetentionSweepIntervalMS  int    // retention_sweep_interval_ms — the retention housekeeping loop's tick (§3.1)
	RetentionDeleteBatchSize  int    // retention_delete_batch_size — rows per send_logs DELETE batch (§3.1, owner of Phase 1 #29)
	RollupGranularity         string // rollup_granularity — bucket granularity for the derive-on-demand summaries (§3.2)

	// Phase 5 policy dials (all seeded send_managed_0017 / send_managed_0010; config-not-code, §7).
	QuarantineThreshold  int             // quarantine_threshold — score below which auto-quarantine fires (§3.4/§4.2b)
	ThrottlePauseSeconds int             // throttle_pause_seconds — throttle-pause window + throttle-class requeue delay (§4.3/§6.4)
	Backoff              BackoffSchedule // retry_backoff_schedule — the transient/timeout/unknown requeue ladder (§6.4)
	ScoreWeights         ScoreWeights    // status_score_weights — the account status-score formula (§3.2)

	// Phase 8 content-variation dials (all seventeen seeded send_managed_0020, appended to Chunk 1's
	// version file — Docs/history/open-decisions.md P8-OD-1; amended 2026-07-26 from fourteen to
	// seventeen, RULED-WIDEN). GLOBAL scope (§3): one operator-owned set resolved once per
	// LoadConfig and stamped verbatim onto every push_job.variation (variation.go). The Control
	// performs NO decision logic on these beyond reading + stamping; the worker's
	// resolveVariation/STAGES own all toggle semantics (§1.2).
	VariationHTMLMinify           bool     // variation_html_minify — toggle 1
	VariationCommentInjection     bool     // variation_comment_injection — toggle 2
	VariationCommentIntensity     string   // variation_comment_intensity — param of toggle 2 (low/medium/high)
	VariationCSSMicro             bool     // variation_css_micro — toggle 3
	VariationCSSMagnitudePx       int      // variation_css_magnitude_px — param of toggle 3 (max +/-px spread; amended, P8-OD-1)
	VariationUniqueURLParams      bool     // variation_unique_url_params — toggle 4 (NOT tracking, P8-10)
	VariationZeroWidth            bool     // variation_zero_width — toggle 5, DEFAULT OFF
	VariationZeroWidthRate        float64  // variation_zero_width_rate — param of toggle 5 (fraction 0-1; amended, P8-OD-1)
	VariationEncodingRotation     bool     // variation_encoding_rotation — toggle 6
	VariationMIMEBoundaryRotation bool     // variation_mime_boundary_rotation — toggle 7
	VariationDynamicPreheader     bool     // variation_dynamic_preheader — toggle 8
	VariationPreheaderTemplates   []string // variation_preheader_templates — param of toggle 8 (P8-7)
	VariationHeaderStyle          string   // variation_header_style — toggle 9
	VariationUserAgentInjection   bool     // variation_user_agent_injection — toggle 10, decoupled (P8-6)
	VariationUserAgentRate        float64  // variation_user_agent_rate — param of toggle 10 (fraction 0-1; amended, P8-OD-1)
	VariationPrecedence           string   // variation_precedence — toggle 11
	VariationThreadIndex          bool     // variation_thread_index — toggle 12, inert (P8-8)

	// Phase 9.5 operability & control-plane safety dials (all eleven seeded send_managed_0021;
	// config-not-code, §6). B1-B5 controls + the S10/S11 completeness dials.
	DispatchEnabled                   bool           // dispatch_enabled — B1 global panic switch (§1.1); a severity FLOOR of accept_and_defer, see rung.go
	DispatchMode                      string         // dispatch_mode — the graduated rung ladder, send_managed_0025 (fix-plan item 12); read through EffectiveRung(), never directly by a gate
	AutoquarantineMode                string         // autoquarantine_mode — B2 shadow ladder off|shadow|enforce (§2.1)
	AutopauseEnabled                  bool           // autopause_enabled — B2 kill-switch (§2.2); a CEILING on AutopauseMode, see mutator_mode.go
	AutopauseMode                     string         // autopause_mode — shadow ladder off|shadow|enforce, send_managed_0026 (item 13); read through EffectiveAutopauseMode(), never directly
	RetentionMode                     string         // retention_mode — shadow ladder off|shadow|enforce, send_managed_0026 (item 13); read through EffectiveRetentionMode(), never directly
	VariationEnabled                  bool           // variation_enabled — B2 master kill-switch (§2.3)
	AlertWebhookURL                   string         // alert_webhook_url — B4 optional outbound webhook (§4.3); "" = disabled
	HealthMetricsWindowSec            int            // health_metrics_window_sec — B3 rolling window (§3.2)
	ControlSingletonAcquireTimeoutSec int            // brain_singleton_acquire_timeout_sec — B5 bounded wait (§5.2)
	UnitPriceCredits                  int            // unit_price_credits — S10 (§2.4)
	VariationCommentIntensityRates    IntensityRates // variation_comment_intensity_rates — S11 (§2.4)
	VariationZeroWidthRatePct         int            // variation_zero_width_rate_pct — S11 (§2.4)
	VariationCSSMicroParams           CSSMicroParams // variation_css_micro_params — S11 (§2.4)
}

// IntensityRates is the §2.4 S11 inner shape of variation_comment_intensity_rates: percent (0-100)
// of closing tags injected per variation_comment_intensity bucket.
type IntensityRates struct {
	Low    int `json:"low"`
	Medium int `json:"medium"`
	High   int `json:"high"`
}

// intensityRatesDefault mirrors the send_managed_0021 seed — the §7-style reject-and-fall-back value for
// a malformed stored json (never a hardcoded shortcut on the healthy path).
var intensityRatesDefault = IntensityRates{Low: 5, Medium: 15, High: 30}

// CSSMicroParams is the §2.4 S11 inner shape of variation_css_micro_params.
type CSSMicroParams struct {
	DeltaPx     int `json:"delta_px"`
	SkipBelowPx int `json:"skip_below_px"`
}

// cssMicroParamsDefault mirrors the send_managed_0021 seed.
var cssMicroParamsDefault = CSSMicroParams{DeltaPx: 1, SkipBelowPx: 3}

// BackoffSchedule is the §6.4 exponential ladder for transient/timeout/unknown requeues. The
// saturating attempt_count indexes the ladder; past the last rung the delay saturates at
// max_seconds (§6.4). Read from the retry_backoff_schedule json config key.
type BackoffSchedule struct {
	LadderSeconds []int `json:"ladder_seconds"`
	MaxSeconds    int   `json:"max_seconds"`
}

// backoffScheduleDefault mirrors the send_managed_0017 seed. Used ONLY as the reject-and-fall-back
// value when the stored json is malformed (§7: "never crash") — never as a config-not-code
// shortcut; the healthy path always reads the seeded row.
var backoffScheduleDefault = BackoffSchedule{LadderSeconds: []int{30, 120, 600}, MaxSeconds: 600}

// ScoreWeights is the §3.2 status-score formula's weights + shape parameters, read from the
// status_score_weights json config key.
type ScoreWeights struct {
	WFail               float64 `json:"w_fail"`
	WProx               float64 `json:"w_prox"`
	WRecency            float64 `json:"w_recency"`
	FailSaturation      float64 `json:"fail_saturation"`
	RecencyHorizonHours float64 `json:"recency_horizon_hours"`
}

// scoreWeightsDefault mirrors the send_managed_0017 seed — the §7 reject-and-fall-back value for a
// malformed stored json (never a hardcoded shortcut).
var scoreWeightsDefault = ScoreWeights{WFail: 60, WProx: 15, WRecency: 25, FailSaturation: 5, RecencyHorizonHours: 72}

// backoffForAttempt returns the requeue delay (seconds) for a given saturating attempt_count,
// indexing the ladder and capping at MaxSeconds (§6.4). attempt_count is 1-based after the first
// failure; index 0 of the ladder is the first backoff. A ladder rung is itself capped at
// MaxSeconds so a mis-seeded ladder can never exceed the documented cap.
func (b BackoffSchedule) backoffForAttempt(attemptCount int) int {
	ladder := b.LadderSeconds
	if len(ladder) == 0 {
		ladder = backoffScheduleDefault.LadderSeconds
	}
	idx := attemptCount - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(ladder) {
		idx = len(ladder) - 1
	}
	d := ladder[idx]
	max := b.MaxSeconds
	if max <= 0 {
		max = backoffScheduleDefault.MaxSeconds
	}
	if d > max {
		d = max
	}
	return d
}

// LoadConfig reads the dispatcher-consumed config keys from send_managed.config. Values are JSONB-wrapped
// {"v": ...}; this unwraps the scalar. A missing key is an error (the migration seeds them), not
// a silent default — a hardcoded fallback would violate config-not-code. The dispatcher RE-READS
// config each tick (Phase 2 OD-14, spec §0), so a change takes effect without a redeploy.
func LoadConfig(ctx context.Context, db *pgxpool.Pool) (Config, error) {
	var c Config
	var err error
	if c.JobMaxProcessingTimeSec, err = readIntKey(ctx, db, "job_max_processing_time_sec"); err != nil {
		return c, err
	}
	if c.DebugLogRetentionDays, err = readIntKey(ctx, db, "debug_log_retention_days"); err != nil {
		return c, err
	}
	if c.DispatchTickIntervalMS, err = readIntKey(ctx, db, "dispatch_tick_interval_ms"); err != nil {
		return c, err
	}
	if c.ConcurrencyPerUserDefault, err = readIntKey(ctx, db, "concurrency_per_user_default"); err != nil {
		return c, err
	}
	if c.SendDelayMinSec, err = readIntKey(ctx, db, "send_delay_min_sec"); err != nil {
		return c, err
	}
	if c.SendDelayMaxSec, err = readIntKey(ctx, db, "send_delay_max_sec"); err != nil {
		return c, err
	}
	if c.ReclaimTickIntervalMS, err = readIntKey(ctx, db, "reclaim_tick_interval_ms"); err != nil {
		return c, err
	}
	if c.HeartbeatIntervalSec, err = readIntKey(ctx, db, "heartbeat_interval_sec"); err != nil {
		return c, err
	}
	if c.HeartbeatMissedThreshold, err = readIntKey(ctx, db, "heartbeat_missed_threshold"); err != nil {
		return c, err
	}
	if c.SMTPOperationTimeoutSec, err = readIntKey(ctx, db, "smtp_operation_timeout_sec"); err != nil {
		return c, err
	}
	if c.StartupReconnectWindowSec, err = readIntKey(ctx, db, "startup_reconnect_window_sec"); err != nil {
		return c, err
	}
	if c.WorkerOutcomeBufferTTLSec, err = readIntKey(ctx, db, "box_outcome_buffer_ttl_sec"); err != nil {
		return c, err
	}
	if c.ShutdownDrainTimeoutSec, err = readIntKey(ctx, db, "shutdown_drain_timeout_sec"); err != nil {
		return c, err
	}
	if c.RetentionSweepIntervalMS, err = readIntKey(ctx, db, "retention_sweep_interval_ms"); err != nil {
		return c, err
	}
	if c.RetentionDeleteBatchSize, err = readIntKey(ctx, db, "retention_delete_batch_size"); err != nil {
		return c, err
	}
	if c.RollupGranularity, err = readStringKey(ctx, db, "rollup_granularity"); err != nil {
		return c, err
	}
	if c.DailyResetTimezone, err = readStringKey(ctx, db, "daily_reset_timezone"); err != nil {
		return c, err
	}
	if c.DailyResetHour, err = readIntKey(ctx, db, "daily_reset_hour"); err != nil {
		return c, err
	}
	if c.DefaultSharedPoolID, err = readNullableStringKey(ctx, db, "default_shared_pool_id"); err != nil {
		return c, err
	}
	if c.QuarantineThreshold, err = readIntKey(ctx, db, "quarantine_threshold"); err != nil {
		return c, err
	}
	if c.ThrottlePauseSeconds, err = readIntKey(ctx, db, "throttle_pause_seconds"); err != nil {
		return c, err
	}
	// The two json keys: read the seeded row, but on a MALFORMED value fall back to the documented
	// default with a logged warning rather than crash the dispatcher (§7). A missing key is still a
	// hard error (the migration seeds it); only a corrupt/edited value triggers the fallback.
	c.Backoff = readBackoffSchedule(ctx, db)
	c.ScoreWeights = readScoreWeights(ctx, db)

	// Phase 8's fourteen content-variation keys (send_managed_0020, Chunk 5). Missing keys are a hard
	// error like every other dial above — the migration seeds them, so an absent row means a
	// pre-Chunk-5 schema, not a legitimate operator override.
	if c.VariationHTMLMinify, err = readBoolKey(ctx, db, "variation_html_minify"); err != nil {
		return c, err
	}
	if c.VariationCommentInjection, err = readBoolKey(ctx, db, "variation_comment_injection"); err != nil {
		return c, err
	}
	if c.VariationCommentIntensity, err = readStringKey(ctx, db, "variation_comment_intensity"); err != nil {
		return c, err
	}
	if c.VariationCSSMicro, err = readBoolKey(ctx, db, "variation_css_micro"); err != nil {
		return c, err
	}
	if c.VariationCSSMagnitudePx, err = readIntKey(ctx, db, "variation_css_magnitude_px"); err != nil {
		return c, err
	}
	if c.VariationUniqueURLParams, err = readBoolKey(ctx, db, "variation_unique_url_params"); err != nil {
		return c, err
	}
	if c.VariationZeroWidth, err = readBoolKey(ctx, db, "variation_zero_width"); err != nil {
		return c, err
	}
	if c.VariationZeroWidthRate, err = readFloatKey(ctx, db, "variation_zero_width_rate"); err != nil {
		return c, err
	}
	if c.VariationEncodingRotation, err = readBoolKey(ctx, db, "variation_encoding_rotation"); err != nil {
		return c, err
	}
	if c.VariationMIMEBoundaryRotation, err = readBoolKey(ctx, db, "variation_mime_boundary_rotation"); err != nil {
		return c, err
	}
	if c.VariationDynamicPreheader, err = readBoolKey(ctx, db, "variation_dynamic_preheader"); err != nil {
		return c, err
	}
	if c.VariationPreheaderTemplates, err = readStringArrayKey(ctx, db, "variation_preheader_templates"); err != nil {
		return c, err
	}
	if c.VariationHeaderStyle, err = readStringKey(ctx, db, "variation_header_style"); err != nil {
		return c, err
	}
	if c.VariationUserAgentInjection, err = readBoolKey(ctx, db, "variation_user_agent_injection"); err != nil {
		return c, err
	}
	if c.VariationUserAgentRate, err = readFloatKey(ctx, db, "variation_user_agent_rate"); err != nil {
		return c, err
	}
	if c.VariationPrecedence, err = readStringKey(ctx, db, "variation_precedence"); err != nil {
		return c, err
	}
	if c.VariationThreadIndex, err = readBoolKey(ctx, db, "variation_thread_index"); err != nil {
		return c, err
	}

	// Phase 9.5's eleven operability + completeness keys (send_managed_0021). Missing keys are a hard
	// error like every other dial above — the migration seeds them, so an absent row means a
	// pre-send_managed_0021 schema, not a legitimate operator override.
	if c.DispatchEnabled, err = readBoolKey(ctx, db, "dispatch_enabled"); err != nil {
		return c, err
	}
	// dispatch_mode (send_managed_0025, fix-plan item 12). A HARD error when the row is absent, like
	// every other dial: the migration seeds it, so a missing row means a pre-0025 schema. It is NOT
	// defaulted to serving here — a Control that boots and silently sends mail because it could not
	// read the kill-switch is the failure this ladder exists to remove. An unrecognized VALUE is a
	// different case and does fail safe, in resolveRung (rung.go), because at that point the row was
	// read successfully and the safe answer is knowable.
	if c.DispatchMode, err = readStringKey(ctx, db, "dispatch_mode"); err != nil {
		return c, err
	}
	if c.AutoquarantineMode, err = readStringKey(ctx, db, "autoquarantine_mode"); err != nil {
		return c, err
	}
	if c.AutopauseEnabled, err = readBoolKey(ctx, db, "autopause_enabled"); err != nil {
		return c, err
	}
	// send_managed_0026 (fix-plan item 13). Both are read as hard requirements, exactly like
	// dispatch_mode and autoquarantine_mode: a missing row is a boot failure, not a silent default. The
	// fail-safe lives in EffectiveAutopauseMode/EffectiveRetentionMode (mutator_mode.go), which handle
	// an unrecognized VALUE — a different question from an absent ROW, and the reason a graceful
	// default here would be wrong (a Control that boots and deletes rows an operator believed were
	// merely being counted is worse than one that refuses to boot).
	if c.AutopauseMode, err = readStringKey(ctx, db, "autopause_mode"); err != nil {
		return c, err
	}
	if c.RetentionMode, err = readStringKey(ctx, db, "retention_mode"); err != nil {
		return c, err
	}
	if c.VariationEnabled, err = readBoolKey(ctx, db, "variation_enabled"); err != nil {
		return c, err
	}
	if c.AlertWebhookURL, err = readStringKey(ctx, db, "alert_webhook_url"); err != nil {
		return c, err
	}
	if c.HealthMetricsWindowSec, err = readIntKey(ctx, db, "health_metrics_window_sec"); err != nil {
		return c, err
	}
	if c.ControlSingletonAcquireTimeoutSec, err = readIntKey(ctx, db, "brain_singleton_acquire_timeout_sec"); err != nil {
		return c, err
	}
	if c.UnitPriceCredits, err = readIntKey(ctx, db, "unit_price_credits"); err != nil {
		return c, err
	}
	c.VariationCommentIntensityRates = readIntensityRates(ctx, db)
	if c.VariationZeroWidthRatePct, err = readIntKey(ctx, db, "variation_zero_width_rate_pct"); err != nil {
		return c, err
	}
	c.VariationCSSMicroParams = readCSSMicroParams(ctx, db)

	// Config-ordering INVARIANT (CLAUDE.md Rule 1, Phase 6 P6-11): the worker's per-operation SMTP
	// timeout MUST be strictly shorter than the job lease, so a genuinely account-caused hang
	// surfaces as a worker-REPORTED timeout (scored → quarantined, Phase 5) rather than an unscored
	// Control lease reclaim that would thrash forever (§0/§2.4). A set that violates it is REJECTED at
	// LOAD with the external code config_ordering_violation — never clamped or auto-corrected (Rule 1:
	// "Do not clamp or auto-correct — reject"). This is the load-time enforcement point; the admin
	// write path deliberately does NOT couple these two independently-tuned dials at write time
	// (Phase 6 §5.1), so the reject lives here where the Control reads the whole set together. See
	// P6-OD-1 (Docs/history/open-decisions.md) for the reconciliation of Rule 1's reject vs §5.1's warn.
	if err := validateConfigOrdering(c); err != nil {
		return Config{}, err
	}
	return c, nil
}

// ErrConfigOrderingViolation is the load-time rejection of a config set that violates a documented
// config-ordering invariant (CLAUDE.md Rule 1). Its Error() string is the external code
// config_ordering_violation — the two-vocabulary safe code; the specific pair that violated is
// appended for internal logs only and never crosses a client boundary (LoadConfig callers log it,
// they do not serialize it to a caller).
type configOrderingError struct{ detail string }

func (e *configOrderingError) Error() string { return "config_ordering_violation: " + e.detail }

func (e *configOrderingError) isConfigFault() {}

// configFault is the MARKER every "the DB answered fine, but the config it returned is unusable"
// error implements. IsOutage (dbhealth.go) tests for THIS interface rather than for each concrete
// type, so the classification cannot be silently forgotten by a future error class: a new config
// fault that does not implement the marker will not compile against configFaultError, and one that
// does is exempt from outage classification automatically.
//
// WHY this is an interface and not a second errors.As on a concrete type (found 2026-07-28 walking
// the local test guide, Stage 6): IsOutage originally exempted *configOrderingError ALONE. A
// wrong-TYPE value (`{"v":"thirty"}` for an integer key, reachable by a direct SQL write or a bad
// migration — exactly the path config.go's validateConfigOrdering comment calls out) was a bare
// fmt.Errorf, so it fell to IsOutage's default `return true` and was reported as
// `db outage DETECTED: ... the DB is unreachable` against a perfectly healthy DB — then cleared by
// the next tick's SELECT 1, i.e. the precise 500ms DETECTED/RESOLVED flap that
// TestConfigOrdering_IsNotADBOutage exists to prevent, arriving through a sibling error class the
// test did not cover. Observed: 80 outage lines in a few seconds, burying the one line naming the
// bad key. The halt itself was correct (nothing was claimed, no restart, self-healed in 1s on
// repair) — the DIAGNOSIS handed to the operator was wrong, which is a Rule-5 honesty defect: it
// sends whoever is holding the pager to look at the database instead of at the config row.
type configFault interface {
	error
	isConfigFault()
}

// configValueError is a config value the DB returned intact but that does not parse as the type the
// key is declared to hold (wrong JSON type, missing "v", corrupt row). want carries the SAME
// human-readable shape the readers have always logged (e.g. `an integer {"v":N}`) so log greps and
// operator muscle memory are unchanged — this refactor changed the error's TYPE, not its text.
type configValueError struct {
	key  string
	want string
}

func (e *configValueError) Error() string {
	return fmt.Sprintf("config %q is not %s", e.key, e.want)
}

func (e *configValueError) isConfigFault() {}

// badConfigValue builds the config-fault error the typed readers return on a parse failure. A
// function (not an inline &configValueError{}) so every reader below is provably using the marker
// type: there is exactly one construction site to audit.
func badConfigValue(key, want string) error { return &configValueError{key: key, want: want} }

// validateConfigOrdering enforces BOTH documented config-ordering invariants at load (CLAUDE.md
// Rule 1 lists them as (a) and (b); Phase 7 §5.3 is the authoritative table), delegating to
// internal/configordering — the SAME pure functions the admin PUT boundary calls
// (internal/adminapi/config_validate.go), so load-time and write-time can never define the
// invariant two different ways (P9-OD-2, RULED-AND-CLOSED).
//
//	(a) P6-11: smtp_operation_timeout_sec  <  job_max_processing_time_sec  — STRICT. Equality is also
//	    a violation (a hang that ties the lease could be reclaimed before the worker reports, turning a
//	    scored worker-reported timeout into an unscored Control reclaim that thrashes forever).
//	(b) P7-11: box_outcome_buffer_ttl_sec  >= startup_reconnect_window_sec — NON-strict (§5.3 states
//	    `≥`; bare equality is the theoretical floor and the spec recommends ~2× margin as an operator
//	    GUIDELINE, deliberately not a second computed key — the worker's reconnect backoff has no numeric
//	    bound defined in Phase 3, so a margin cannot be computed, only recommended). If the buffer TTL
//	    were shorter than the rescan window, a worker reconnecting late-but-in-window would have ALREADY
//	    DROPPED its outcome → the job is lease-reclaimed → re-sent → SILENT DUPLICATE, the exact
//	    failure P7-2/P7-4 exist to prevent.
//
// Both are REJECTED at load, never clamped or auto-corrected (Rule 1: "Do not clamp or auto-correct
// — reject"), with the external code config_ordering_violation. A refused boot is loud and instantly
// fixable; the duplicate the unsafe pair would cause is silent and unrecoverable after the fact.
// This is now the BACKSTOP, not the only line of defense — the admin PUT boundary rejects the same
// invariant before a bad value ever lands in send_managed.config (defense in depth: a value that somehow
// reaches the DB by another path, e.g. a direct SQL write, is still caught here).
func validateConfigOrdering(c Config) error {
	if v := configordering.CheckSMTPTimeoutVsLease(c.SMTPOperationTimeoutSec, c.JobMaxProcessingTimeSec); v != nil {
		return &configOrderingError{detail: v.Detail()}
	}
	if v := configordering.CheckBufferTTLVsReconnectWindow(c.WorkerOutcomeBufferTTLSec, c.StartupReconnectWindowSec); v != nil {
		return &configOrderingError{detail: v.Detail()}
	}
	return nil
}

// readBackoffSchedule reads retry_backoff_schedule ({"v": {...}}), unwrapping and validating the
// inner object; on any malformation it logs and returns backoffScheduleDefault (§7 never-crash).
func readBackoffSchedule(ctx context.Context, db *pgxpool.Pool) BackoffSchedule {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key='retry_backoff_schedule'`).Scan(&raw); err != nil {
		log.Printf("dispatch: retry_backoff_schedule unreadable (%v) — using documented default", err)
		return backoffScheduleDefault
	}
	var wrap struct {
		V *BackoffSchedule `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil ||
		len(wrap.V.LadderSeconds) == 0 || wrap.V.MaxSeconds <= 0 {
		log.Printf("dispatch: retry_backoff_schedule malformed — using documented default (§7)")
		return backoffScheduleDefault
	}
	for _, s := range wrap.V.LadderSeconds {
		if s <= 0 {
			log.Printf("dispatch: retry_backoff_schedule has a non-positive rung — using documented default (§7)")
			return backoffScheduleDefault
		}
	}
	return *wrap.V
}

// readScoreWeights reads status_score_weights ({"v": {...}}), unwrapping and validating; on any
// malformation it logs and returns scoreWeightsDefault (§7 never-crash). The divisors
// fail_saturation and recency_horizon_hours must be strictly positive.
func readScoreWeights(ctx context.Context, db *pgxpool.Pool) ScoreWeights {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key='status_score_weights'`).Scan(&raw); err != nil {
		log.Printf("dispatch: status_score_weights unreadable (%v) — using documented default", err)
		return scoreWeightsDefault
	}
	var wrap struct {
		V *ScoreWeights `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil ||
		wrap.V.WFail < 0 || wrap.V.WProx < 0 || wrap.V.WRecency < 0 ||
		wrap.V.FailSaturation <= 0 || wrap.V.RecencyHorizonHours <= 0 {
		log.Printf("dispatch: status_score_weights malformed — using documented default (§7)")
		return scoreWeightsDefault
	}
	return *wrap.V
}

// readIntensityRates reads variation_comment_intensity_rates ({"v": {...}}), unwrapping and
// validating; on any malformation it logs and returns intensityRatesDefault (§7 never-crash
// posture, same as readScoreWeights/readBackoffSchedule). Each bucket must be within [0,100].
func readIntensityRates(ctx context.Context, db *pgxpool.Pool) IntensityRates {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key='variation_comment_intensity_rates'`).Scan(&raw); err != nil {
		log.Printf("dispatch: variation_comment_intensity_rates unreadable (%v) — using documented default", err)
		return intensityRatesDefault
	}
	var wrap struct {
		V *IntensityRates `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil ||
		wrap.V.Low < 0 || wrap.V.Low > 100 || wrap.V.Medium < 0 || wrap.V.Medium > 100 ||
		wrap.V.High < 0 || wrap.V.High > 100 {
		log.Printf("dispatch: variation_comment_intensity_rates malformed — using documented default (§7)")
		return intensityRatesDefault
	}
	return *wrap.V
}

// readCSSMicroParams reads variation_css_micro_params ({"v": {...}}), unwrapping and validating;
// on any malformation it logs and returns cssMicroParamsDefault (§7 never-crash posture).
func readCSSMicroParams(ctx context.Context, db *pgxpool.Pool) CSSMicroParams {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key='variation_css_micro_params'`).Scan(&raw); err != nil {
		log.Printf("dispatch: variation_css_micro_params unreadable (%v) — using documented default", err)
		return cssMicroParamsDefault
	}
	var wrap struct {
		V *CSSMicroParams `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil ||
		wrap.V.DeltaPx < 0 || wrap.V.DeltaPx > 5 || wrap.V.SkipBelowPx < 0 || wrap.V.SkipBelowPx > 20 {
		log.Printf("dispatch: variation_css_micro_params malformed — using documented default (§7)")
		return cssMicroParamsDefault
	}
	return *wrap.V
}

// readIntKey reads one integer config key ({"v": <int>}).
func readIntKey(ctx context.Context, db *pgxpool.Pool, key string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key = $1`, key).Scan(&raw); err != nil {
		return 0, fmt.Errorf("read config %q: %w", key, err)
	}
	var wrap struct {
		V *int `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil {
		return 0, badConfigValue(key, `an integer {"v":N}`)
	}
	return *wrap.V, nil
}

// readBoolKey reads one boolean config key ({"v": true|false}) — Phase 8's toggle keys (§6.1).
func readBoolKey(ctx context.Context, db *pgxpool.Pool, key string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key = $1`, key).Scan(&raw); err != nil {
		return false, fmt.Errorf("read config %q: %w", key, err)
	}
	var wrap struct {
		V *bool `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil {
		return false, badConfigValue(key, `a boolean {"v":true|false}`)
	}
	return *wrap.V, nil
}

// readFloatKey reads one number config key ({"v": <float>}) — Phase 8's rate/fraction keys
// (variation_zero_width_rate, variation_user_agent_rate), amended in post-Chunk-5 (P8-OD-1,
// RULED-WIDEN). Phase 8 is the first phase to need a non-integer numeric dial.
func readFloatKey(ctx context.Context, db *pgxpool.Pool, key string) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key = $1`, key).Scan(&raw); err != nil {
		return 0, fmt.Errorf("read config %q: %w", key, err)
	}
	var wrap struct {
		V *float64 `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil {
		return 0, badConfigValue(key, `a number {"v":N}`)
	}
	return *wrap.V, nil
}

// readStringArrayKey reads one array<string> config key ({"v": ["...", ...]}) — Phase 8's
// variation_preheader_templates (§6.1). An empty array is a valid value (the toggle degrades to
// "no candidates, inject nothing" — variation.js's pickSeeded already handles that), so only a
// malformed/missing "v" is an error, not a zero-length list.
func readStringArrayKey(ctx context.Context, db *pgxpool.Pool, key string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key = $1`, key).Scan(&raw); err != nil {
		return nil, fmt.Errorf("read config %q: %w", key, err)
	}
	var wrap struct {
		V *[]string `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil {
		return nil, badConfigValue(key, `a string array {"v":["..."]}`)
	}
	return *wrap.V, nil
}

// readStringKey reads one string config key ({"v": "..."}).
func readStringKey(ctx context.Context, db *pgxpool.Pool, key string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key = $1`, key).Scan(&raw); err != nil {
		return "", fmt.Errorf("read config %q: %w", key, err)
	}
	var wrap struct {
		V *string `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil {
		return "", badConfigValue(key, `a string {"v":"..."}`)
	}
	return *wrap.V, nil
}

// readNullableStringKey reads a string config key that is LEGITIMATELY seeded NULL
// (default_shared_pool_id, §4.3 — an operator must set it before base sending works). Unlike
// readStringKey, a {"v": null} value is not an error here: it returns (nil, nil).
func readNullableStringKey(ctx context.Context, db *pgxpool.Pool, key string) (*string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var raw []byte
	if err := db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key = $1`, key).Scan(&raw); err != nil {
		return nil, fmt.Errorf("read config %q: %w", key, err)
	}
	var wrap struct {
		V *string `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil, badConfigValue(key, `a nullable string {"v": "..."|null}`)
	}
	return wrap.V, nil
}
