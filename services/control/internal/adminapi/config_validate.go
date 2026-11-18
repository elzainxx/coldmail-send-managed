package adminapi

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"

	// Embed the IANA tz database so daily_reset_timezone validation (time.LoadLocation) works
	// regardless of the container's system tzdata. This is a data dependency, not a tuning
	// constant (config-not-code is about behavioral dials; the tz table is a validation source).
	_ "time/tzdata"
)

// validateConfigValue enforces value_type (§6.3) and per-key semantic rules (§6.4) BEFORE write.
// The DB stores opaque JSONB, so these checks are the only guard against a bad dial; each failure
// is a clean envelope (never a raw error). value is the UNWRAPPED scalar the client sent.
func validateConfigValue(key, valueType string, value json.RawMessage) *apiError {
	// --- value_type gate ---
	switch valueType {
	case "integer":
		if _, ok := asInt(value); !ok {
			return typeErr(key, issueMalformed, "config_value_not_integer")
		}
	case "string":
		var s string
		if json.Unmarshal(value, &s) != nil {
			return typeErr(key, issueMalformed, "config_value_not_string")
		}
	case "boolean":
		var b bool
		if json.Unmarshal(value, &b) != nil {
			return typeErr(key, issueMalformed, "config_value_not_boolean")
		}
	case "uuid":
		// string matching UUID, or null.
		if string(value) != "null" {
			var s string
			if json.Unmarshal(value, &s) != nil || !isUUID(s) {
				return typeErr(key, issueMalformed, "config_value_not_uuid")
			}
		}
	case "array<integer>":
		var raw []json.RawMessage
		if json.Unmarshal(value, &raw) != nil {
			return typeErr(key, issueMalformed, "config_value_not_int_array")
		}
		for _, elem := range raw {
			if _, ok := asInt(elem); !ok {
				return typeErr(key, issueMalformed, "config_value_not_int_array")
			}
		}
	case "number":
		// Phase 8 (variation_zero_width_rate / variation_user_agent_rate, amended P8-OD-1
		// RULED-WIDEN): a JSON number, integer or float. Phase 8 is the first phase to need a
		// non-integer numeric dial — semantic range checks run in the block below.
		//
		// REJECT a quoted numeric string (e.g. "0.5") here, not just a genuine JSON number: R3
		// review found that json.Number happily decodes FROM a JSON string token too, so without
		// this guard {"value":"0.5"} would pass this gate, get stored verbatim as {"v":"0.5"}, and
		// then hard-fail config.go's readFloatKey on every subsequent LoadConfig call (every
		// dispatch/reclaim/rescan/retention tick) — a write that looks successful (200) silently
		// wedges the Control, and dbhealth.go's generic-error path misclassifies that failure as a DB
		// outage. Reject at the type gate instead: the write-time value_type check exists precisely
		// to make this class of mismatch impossible.
		if rawJSONIsQuoted(value) {
			return typeErr(key, issueMalformed, "config_value_not_number")
		}
		var f json.Number
		dec := json.NewDecoder(bytes.NewReader(value))
		dec.UseNumber()
		if err := dec.Decode(&f); err != nil {
			return typeErr(key, issueMalformed, "config_value_not_number")
		}
		if _, err := f.Float64(); err != nil {
			return typeErr(key, issueMalformed, "config_value_not_number")
		}
	case "array<string>":
		// Phase 8 (variation_preheader_templates, §6.1): a JSON array of strings. Empty is valid
		// (the toggle degrades to "no candidates" worker-side, variation.js's pickSeeded) — only a
		// non-array or a non-string element is rejected.
		var arr []json.RawMessage
		if json.Unmarshal(value, &arr) != nil {
			return typeErr(key, issueMalformed, "config_value_not_string_array")
		}
		for _, elem := range arr {
			var s string
			if json.Unmarshal(elem, &s) != nil {
				return typeErr(key, issueMalformed, "config_value_not_string_array")
			}
		}
	case "json":
		// Phase 5 §7: a json-typed value must be a well-formed JSON object (the {"v": ...} envelope
		// is added by the store; here `value` is the UNWRAPPED inner value). Reject a non-object or
		// malformed JSON; per-key inner-shape checks run in the semantic block below.
		var obj map[string]json.RawMessage
		if json.Unmarshal(value, &obj) != nil {
			return typeErr(key, issueMalformed, "config_value_not_json_object")
		}
	default:
		// Unknown value_type in the seed contract — reject rather than write blindly.
		return typeErr(key, issueMalformed, "config_unknown_value_type:"+valueType)
	}

	// --- per-key semantic validation (§6.4) ---
	switch key {
	case "daily_reset_timezone":
		var tz string
		_ = json.Unmarshal(value, &tz)
		if _, err := time.LoadLocation(tz); err != nil {
			return typeErr(key, issueForbiddenValue, "config_invalid_iana_tz:"+tz)
		}
	case "daily_reset_hour":
		if n, ok := asInt(value); !ok || n < 0 || n > 23 {
			return typeErr(key, issueOutOfRange, "config_daily_reset_hour_out_of_range")
		}
	case "quarantine_threshold":
		if n, ok := asInt(value); !ok || n < 0 || n > 100 {
			return typeErr(key, issueOutOfRange, "config_quarantine_threshold_out_of_range")
		}
	case "daily_limit_default":
		// > 0 and <= the same hardcoded sanity ceiling as per-account daily_limit (§6.4 says the
		// default is bounded by "the ceiling used to sanity-bound per-account daily_limit in §3").
		// OD-24 option (c): a bound, not a config dial.
		if n, ok := asInt(value); !ok || n <= 0 || n > DailyLimitCeiling {
			return typeErr(key, issueOutOfRange, "config_daily_limit_default_out_of_range")
		}
	case "throttle_pause_seconds":
		// Phase 5 §7: a positive integer (the throttle-pause window / requeue delay in seconds).
		if n, ok := asInt(value); !ok || n <= 0 {
			return typeErr(key, issueOutOfRange, "config_throttle_pause_seconds_out_of_range")
		}
	case "job_max_failed_attempts":
		// send_managed_0028 (M2-G2, the attempt cap): 1-1000 failed SMTP attempts. 0 is REJECTED, not
		// "disabled": a cap of 0 would fail every job on its first failed attempt, and "never cap" is
		// spelled with a large number, not with a zero that means the opposite of what it says. The
		// Control's LoadConfig refuses a stored value below 1 as well (dispatch/config.go) — this is the
		// operator-feedback layer, so the mistake is reported in the response to the PUT.
		if n, ok := asInt(value); !ok || n < 1 || n > 1000 {
			return typeErr(key, issueOutOfRange, "config_job_max_failed_attempts_out_of_range")
		}
	case "job_failed_max_age_sec":
		// send_managed_0028 (M2-G2, the age cap): 60 s to 30 days. A floor of one minute keeps a typo
		// (a value in minutes or in hours typed as seconds) from failing every retrying job at once.
		if n, ok := asInt(value); !ok || n < 60 || n > 2592000 {
			return typeErr(key, issueOutOfRange, "config_job_failed_max_age_sec_out_of_range")
		}
	case "worker_reconnect_base_ms":
		// send_managed_0029 (Phase 3 close-out B1): the first wait of the worker's capped exponential reconnect
		// backoff. 10 ms to 60 s. The 10 ms floor is the MINIMUM LEGAL value (a harness runs 50 ms); below it a
		// fleet would redial in a tight loop. Rejected, never clamped.
		if n, ok := asInt(value); !ok || n < workerproto.ReconnectBaseMinMS || n > workerproto.ReconnectBaseMaxMS {
			return typeErr(key, issueOutOfRange, "config_worker_reconnect_base_ms_out_of_range")
		}
	case "worker_reconnect_cap_ms":
		// send_managed_0029: the largest un-jittered wait of the reconnect backoff. 10 ms to 10 minutes. There
		// is deliberately NO cross-key check against the base: a base above the cap simply makes every wait the
		// cap (the delay is min(cap, base*2^(n-1))), which is well defined, so the two keys can be changed in
		// either order without an ordering violation.
		if n, ok := asInt(value); !ok || n < workerproto.ReconnectCapMinMS || n > workerproto.ReconnectCapMaxMS {
			return typeErr(key, issueOutOfRange, "config_worker_reconnect_cap_ms_out_of_range")
		}
	case "worker_reconnect_jitter_pct":
		// send_managed_0029: the jitter added on top of each wait, as a percentage of it. 0 to 100. 0 is LEGAL
		// (no jitter) and is a real value that reaches the worker as 0.
		if n, ok := asInt(value); !ok || n < workerproto.ReconnectJitterMinPct || n > workerproto.ReconnectJitterMaxPct {
			return typeErr(key, issueOutOfRange, "config_worker_reconnect_jitter_pct_out_of_range")
		}
	case "status_score_weights":
		// Phase 5 §3.2 inner shape: all five keys present, weights/params numeric and non-negative,
		// fail_saturation and recency_horizon_hours strictly positive (they are divisors). A bad
		// shape is rejected at write; the module ALSO re-validates on read and falls back to the
		// seeded default (never crashes) — this write-time check is the first line of defense.
		if perr := validateScoreWeights(key, value); perr != nil {
			return perr
		}
	case "retry_backoff_schedule":
		// Phase 5 §6.4 inner shape: ladder_seconds a non-empty array of positive ints, max_seconds
		// a positive int. Same write-check + read-fallback posture as status_score_weights.
		if perr := validateBackoffSchedule(key, value); perr != nil {
			return perr
		}
	case "variation_comment_intensity":
		// Phase 8 §5 toggle 2 parameter: low/medium/high only (fixes the 5%/15%/30% rate). The worker
		// ALSO degrades an unrecognized value to "low" (variation.js's resolveVariation) — this
		// write-time check is the operator-feedback layer, same posture as the other enum-string
		// keys below, so a typo is rejected loudly rather than silently degrading.
		if perr := validateEnumString(key, value, "low", "medium", "high"); perr != nil {
			return perr
		}
	case "variation_header_style":
		// Phase 8 §5 toggle 9: none/gmail/outlook/apple/thunderbird/random.
		if perr := validateEnumString(key, value, "none", "gmail", "outlook", "apple", "thunderbird", "random"); perr != nil {
			return perr
		}
	case "variation_precedence":
		// Phase 8 §5 toggle 11: auto/marketing/personal.
		if perr := validateEnumString(key, value, "auto", "marketing", "personal"); perr != nil {
			return perr
		}
	case "variation_css_magnitude_px":
		// Amended P8-OD-1 RULED-WIDEN: max +/-px spread for toggle 3. Must be >= 1 (a magnitude of 0
		// would make the toggle a documented no-op while still reporting itself active — confusing,
		// not merely permissive).
		if n, ok := asInt(value); !ok || n < 1 {
			return typeErr(key, issueOutOfRange, "config_variation_css_magnitude_px_out_of_range")
		}
	case "variation_zero_width_rate", "variation_user_agent_rate":
		// Amended P8-OD-1 RULED-WIDEN: both are fractions — 0 (fully off) through 1 (fully on) are
		// both legitimate, inclusive.
		if perr := validateRateFraction(key, value); perr != nil {
			return perr
		}
	case "dispatch_mode":
		// Fix-plan item 12 (send_managed_0025): the graduated dispatch ladder. Same posture as
		// autoquarantine_mode directly below — the Control-side loader ALSO fails safe on an
		// unrecognized value (rung.go's resolveRung degrades to accept_and_defer), and this write-time
		// check is the operator-feedback layer so a typo is rejected loudly here rather than silently
		// landing somewhere the operator did not choose.
		//
		// That matters more for this key than for any other enum on this surface: the two dispositions
		// of a typo'd value differ in what STOPS. An operator who meant `serving` and typed `srving`
		// would, without this check, have their mail halted at accept_and_defer — the safe rung, but
		// emphatically not the one they asked for. Rejecting the write means they find out in the
		// response to their own PUT instead of in a support ticket.
		if perr := validateEnumString(key, value, "serving", "accept_and_defer", "refuse_new", "stopped"); perr != nil {
			return perr
		}
	case "autoquarantine_mode":
		// Phase 9.5 B2 §2.1: the three-way shadow ladder. The Control-side seam (account_effect.go)
		// ALSO degrades an unrecognized value to "shadow" defensively — this write-time check is the
		// operator-feedback layer, same posture as the other enum-string keys above, so a typo is
		// rejected loudly here rather than silently landing on the safe default.
		if perr := validateEnumString(key, value, "off", "shadow", "enforce"); perr != nil {
			return perr
		}
	case "autopause_mode", "retention_mode":
		// Fix-plan item 13 (send_managed_0026): the same three-way shadow ladder, generalized to the two
		// remaining autonomous mutators. Grouped with autoquarantine_mode's value set rather than given
		// their own cases because they ARE the same ladder — three modes, same names, same ordering,
		// same fail-safe target — and splitting them into per-key cases with identical bodies is how the
		// three drift apart one edit at a time.
		//
		// The Control side (dispatch/mutator_mode.go's normalizeMutatorMode) ALSO degrades an
		// unrecognized value to "shadow"; this write-time check is the operator-feedback layer, so a
		// typo is rejected in the response to the PUT rather than silently landing on the default.
		//
		// retention_mode is the one on this surface where a rejected typo matters most, and in the
		// opposite direction from dispatch_mode's: an operator who meant `enforce` and typed `enfroce`
		// would, without this check, get a Control that silently deletes NOTHING and a send_logs table
		// that grows until the disk fills. The failure is slow, quiet, and discovered by an alert on
		// something else entirely.
		if perr := validateEnumString(key, value, "off", "shadow", "enforce"); perr != nil {
			return perr
		}
	case "alert_webhook_url":
		// Phase 9.5 B4 §6: empty (disabled) or an http(s):// URL. Rejecting a garbage value here,
		// rather than at webhook-fire time, means a typo'd URL is caught by the write, not silently
		// swallowed as "every delivery attempt fails" with no operator-visible signal.
		if perr := validateWebhookURL(key, value); perr != nil {
			return perr
		}
	case "unit_price_credits":
		// Phase 9.5 S10 §6: 0-1000 credits/recipient (0 is a legitimate "free" tier, not an error).
		if n, ok := asInt(value); !ok || n < 0 || n > 1000 {
			return typeErr(key, issueOutOfRange, "config_unit_price_credits_out_of_range")
		}
	case "health_metrics_window_sec":
		// Phase 9.5 B3 §6: 60-86400 seconds (1 minute to 1 day).
		if n, ok := asInt(value); !ok || n < 60 || n > 86400 {
			return typeErr(key, issueOutOfRange, "config_health_metrics_window_sec_out_of_range")
		}
	case "brain_singleton_acquire_timeout_sec":
		// Phase 9.5 B5 §6: 0-600 seconds (0 = fail-fast, no wait).
		if n, ok := asInt(value); !ok || n < 0 || n > 600 {
			return typeErr(key, issueOutOfRange, "config_brain_singleton_acquire_timeout_sec_out_of_range")
		}
	case "variation_comment_intensity_rates":
		// Phase 9.5 S11 §6 inner shape: low/medium/high all present, each 0-100. Same write-check +
		// read-fallback posture as status_score_weights/retry_backoff_schedule.
		if perr := validateIntensityRates(key, value); perr != nil {
			return perr
		}
	case "variation_zero_width_rate_pct":
		// Phase 9.5 S11 §6: 0-100 (a documented duplicate of variation_zero_width_rate, P9.5-OD-3 —
		// validated the same regardless of consumption status).
		if n, ok := asInt(value); !ok || n < 0 || n > 100 {
			return typeErr(key, issueOutOfRange, "config_variation_zero_width_rate_pct_out_of_range")
		}
	case "variation_css_micro_params":
		// Phase 9.5 S11 §6 inner shape: delta_px 0-5, skip_below_px 0-20.
		if perr := validateCSSMicroParams(key, value); perr != nil {
			return perr
		}
	}
	return nil
}

// validateIntensityRates checks the §6 inner object shape of variation_comment_intensity_rates:
// low/medium/high all present, each an integer percent 0-100.
func validateIntensityRates(key string, value json.RawMessage) *apiError {
	var r struct {
		Low    *json.Number `json:"low"`
		Medium *json.Number `json:"medium"`
		High   *json.Number `json:"high"`
	}
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	if err := dec.Decode(&r); err != nil {
		return typeErr(key, issueMalformed, "config_variation_comment_intensity_rates_malformed")
	}
	pct := func(n *json.Number) bool {
		if n == nil {
			return false
		}
		v, err := n.Int64()
		return err == nil && v >= 0 && v <= 100
	}
	if !pct(r.Low) || !pct(r.Medium) || !pct(r.High) {
		return typeErr(key, issueForbiddenValue, "config_variation_comment_intensity_rates_out_of_range")
	}
	return nil
}

// validateCSSMicroParams checks the §6 inner object shape of variation_css_micro_params:
// delta_px 0-5, skip_below_px 0-20.
func validateCSSMicroParams(key string, value json.RawMessage) *apiError {
	var p struct {
		DeltaPx     *json.Number `json:"delta_px"`
		SkipBelowPx *json.Number `json:"skip_below_px"`
	}
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	if err := dec.Decode(&p); err != nil {
		return typeErr(key, issueMalformed, "config_variation_css_micro_params_malformed")
	}
	inRange := func(n *json.Number, max int64) bool {
		if n == nil {
			return false
		}
		v, err := n.Int64()
		return err == nil && v >= 0 && v <= max
	}
	if !inRange(p.DeltaPx, 5) || !inRange(p.SkipBelowPx, 20) {
		return typeErr(key, issueForbiddenValue, "config_variation_css_micro_params_out_of_range")
	}
	return nil
}

// validateWebhookURL enforces §6's allowed range for alert_webhook_url: empty (the disabled
// default) or a string beginning with http:// or https://.
func validateWebhookURL(key string, value json.RawMessage) *apiError {
	var s string
	if json.Unmarshal(value, &s) != nil {
		return typeErr(key, issueMalformed, "config_value_not_string")
	}
	if s == "" {
		return nil
	}
	if !strings.HasPrefix(s, "http://") && !strings.HasPrefix(s, "https://") {
		return typeErr(key, issueForbiddenValue, "config_alert_webhook_url_invalid")
	}
	return nil
}

// validateRateFraction rejects a number outside [0, 1] — the shared bound for Phase 8's two
// amended rate parameters (P8-OD-1 RULED-WIDEN). The "number" value_type case already rejects a
// quoted numeric string before this runs, but this function is also reachable on its own, so it
// re-guards defensively rather than relying solely on call-site ordering.
func validateRateFraction(key string, value json.RawMessage) *apiError {
	if rawJSONIsQuoted(value) {
		return typeErr(key, issueMalformed, "config_value_not_number")
	}
	var f json.Number
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	if err := dec.Decode(&f); err != nil {
		return typeErr(key, issueMalformed, "config_value_not_number")
	}
	v, err := f.Float64()
	if err != nil || v < 0 || v > 1 {
		return typeErr(key, issueOutOfRange, "config_"+key+"_out_of_range")
	}
	return nil
}

// rawJSONIsQuoted reports whether raw's first non-whitespace byte is `"` — i.e. the JSON token is
// a string, not a number. encoding/json's json.Number decodes successfully from EITHER a bare
// numeric token (123, 0.5) OR a quoted numeric string ("123", "0.5"), so a numeric value_type gate
// that only checks "did json.Number decode" is not actually a type gate — it silently accepts the
// wrong JSON type. Guard against that explicitly wherever a numeric value_type is enforced (R3
// review finding).
func rawJSONIsQuoted(raw json.RawMessage) bool {
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '"'
}

// validateEnumString rejects a string value that is not one of `allowed` — the write-time operator
// feedback layer for Phase 8's three enum-string toggle keys. The worker-side resolver degrades an
// unrecognized value safely regardless (variation.js), so this check exists to catch a typo loudly
// rather than let it silently do nothing.
func validateEnumString(key string, value json.RawMessage, allowed ...string) *apiError {
	var s string
	if json.Unmarshal(value, &s) != nil {
		return typeErr(key, issueMalformed, "config_value_not_string")
	}
	for _, a := range allowed {
		if s == a {
			return nil
		}
	}
	return typeErr(key, issueForbiddenValue, "config_"+key+"_invalid_enum_value")
}

// validateScoreWeights checks the §3.2 inner object shape of status_score_weights.
func validateScoreWeights(key string, value json.RawMessage) *apiError {
	var w struct {
		WFail        *json.Number `json:"w_fail"`
		WProx        *json.Number `json:"w_prox"`
		WRecency     *json.Number `json:"w_recency"`
		FailSat      *json.Number `json:"fail_saturation"`
		RecencyHours *json.Number `json:"recency_horizon_hours"`
	}
	if json.Unmarshal(value, &w) != nil {
		return typeErr(key, issueMalformed, "config_status_score_weights_malformed")
	}
	nonNeg := func(n *json.Number) bool {
		if n == nil {
			return false
		}
		f, err := n.Float64()
		return err == nil && f >= 0
	}
	pos := func(n *json.Number) bool {
		if n == nil {
			return false
		}
		f, err := n.Float64()
		return err == nil && f > 0
	}
	if !nonNeg(w.WFail) || !nonNeg(w.WProx) || !nonNeg(w.WRecency) {
		return typeErr(key, issueForbiddenValue, "config_status_score_weights_bad_weight")
	}
	if !pos(w.FailSat) || !pos(w.RecencyHours) {
		return typeErr(key, issueForbiddenValue, "config_status_score_weights_bad_divisor")
	}
	return nil
}

// validateBackoffSchedule checks the §6.4 inner object shape of retry_backoff_schedule.
func validateBackoffSchedule(key string, value json.RawMessage) *apiError {
	var s struct {
		Ladder []json.Number `json:"ladder_seconds"`
		Max    *json.Number  `json:"max_seconds"`
	}
	if json.Unmarshal(value, &s) != nil {
		return typeErr(key, issueMalformed, "config_retry_backoff_schedule_malformed")
	}
	if len(s.Ladder) == 0 {
		return typeErr(key, issueForbiddenValue, "config_retry_backoff_schedule_empty_ladder")
	}
	for _, n := range s.Ladder {
		v, err := n.Int64()
		if err != nil || v <= 0 {
			return typeErr(key, issueForbiddenValue, "config_retry_backoff_schedule_bad_ladder")
		}
	}
	if s.Max == nil {
		return typeErr(key, issueForbiddenValue, "config_retry_backoff_schedule_missing_max")
	}
	if v, err := s.Max.Int64(); err != nil || v <= 0 {
		return typeErr(key, issueForbiddenValue, "config_retry_backoff_schedule_bad_max")
	}
	return nil
}

// asInt reports whether raw is a JSON integer (no fractional part) and returns it. Rejects a
// quoted numeric string (e.g. "5") up front — json.Number decodes from either a bare or a quoted
// numeric token, so without this check a string would silently pass as an integer (R3 review
// finding, shared with the "number" value_type gate above).
func asInt(raw json.RawMessage) (int64, bool) {
	if rawJSONIsQuoted(raw) {
		return 0, false
	}
	var num json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&num); err != nil {
		return 0, false
	}
	n, err := num.Int64()
	if err != nil {
		return 0, false
	}
	return n, true
}

// typeErr builds a config validation_failed envelope with a field detail keyed on "value".
func typeErr(key, issue, internalReason string) *apiError {
	return newErr(codeValidationFailed, internalReason).
		withDetails(fieldDetail{Field: "value", Issue: issue})
}
