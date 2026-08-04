package adminapi

import "testing"

// variation_config_test.go — Phase 8 CHUNK 5: proves the admin config surface (§6.3 value_type +
// §6.4 semantic validation) genuinely handles the seventeen Phase 8 keys (amended from fourteen,
// P8-OD-1 RULED-WIDEN) — a boolean, a string array, the three enum-string keys, and the three
// amended number/integer rate-magnitude parameters — through the REAL PUT /v1/admin/config/{key}
// endpoint, not just via a direct SQL write. This is the operator-facing half of Chunk 5;
// variation_test.go (internal/dispatch) proves the Control reads what lands here.

// TestVariationConfig_BooleanToggleRoundTrips — a plain boolean toggle key writes/reads cleanly,
// and a wrong-typed value is rejected with the same value_type gate every other boolean key uses.
func TestVariationConfig_BooleanToggleRoundTrips(t *testing.T) {
	e := newTestEnv(t, "varcfgbool")

	e.do(t, "PUT", "/v1/admin/config/variation_html_minify", map[string]any{"value": true}).requireStatus(t, 200)
	g := e.do(t, "GET", "/v1/admin/config/variation_html_minify", nil).requireStatus(t, 200)
	if v, _ := g.body["value"].(bool); v != true {
		t.Fatalf("variation_html_minify after PUT true = %v, want true", g.body["value"])
	}

	// wrong type => 400, value unchanged.
	e.do(t, "PUT", "/v1/admin/config/variation_html_minify", map[string]any{"value": "yes"}).requireStatus(t, 400)
	g2 := e.do(t, "GET", "/v1/admin/config/variation_html_minify", nil).requireStatus(t, 200)
	if v, _ := g2.body["value"].(bool); v != true {
		t.Fatalf("variation_html_minify after bad PUT = %v, want unchanged true", g2.body["value"])
	}
}

// TestVariationConfig_EnumStringKeysRejectInvalidValues — the three Phase 8 enum-string keys
// (variation_comment_intensity, variation_header_style, variation_precedence) accept only their
// documented values and reject a typo with issueForbiddenValue, not a silent write.
func TestVariationConfig_EnumStringKeysRejectInvalidValues(t *testing.T) {
	e := newTestEnv(t, "varcfgenum")

	cases := []struct {
		key   string
		valid string
		bad   string
	}{
		{"variation_comment_intensity", "high", "extreme"},
		{"variation_header_style", "outlook", "windows95"},
		{"variation_precedence", "marketing", "urgent"},
	}
	for _, c := range cases {
		ok := e.do(t, "PUT", "/v1/admin/config/"+c.key, map[string]any{"value": c.valid}).requireStatus(t, 200)
		_ = ok
		g := e.do(t, "GET", "/v1/admin/config/"+c.key, nil).requireStatus(t, 200)
		if g.str("value") != c.valid {
			t.Fatalf("%s after valid PUT = %q, want %q", c.key, g.str("value"), c.valid)
		}

		bad := e.do(t, "PUT", "/v1/admin/config/"+c.key, map[string]any{"value": c.bad}).requireStatus(t, 400)
		if bad.errorDetailIssue("value") != issueForbiddenValue {
			t.Fatalf("%s invalid enum: issue=%s, want forbidden_value", c.key, bad.errorDetailIssue("value"))
		}
		// stored value unchanged after the rejected write.
		g2 := e.do(t, "GET", "/v1/admin/config/"+c.key, nil).requireStatus(t, 200)
		if g2.str("value") != c.valid {
			t.Fatalf("%s after rejected PUT = %q, want unchanged %q", c.key, g2.str("value"), c.valid)
		}
	}
}

// TestVariationConfig_PreheaderTemplatesArrayRoundTrips — the array<string> value_type (only
// Phase 8 key of this shape) writes/reads a real string array, rejects a non-string element, and
// accepts an empty array (the toggle degrades to "no candidates" worker-side, not an error here).
func TestVariationConfig_PreheaderTemplatesArrayRoundTrips(t *testing.T) {
	e := newTestEnv(t, "varcfgarr")

	e.do(t, "PUT", "/v1/admin/config/variation_preheader_templates",
		map[string]any{"value": []string{"Custom one", "Custom two"}}).requireStatus(t, 200)
	g := e.do(t, "GET", "/v1/admin/config/variation_preheader_templates", nil).requireStatus(t, 200)
	arr, ok := g.body["value"].([]any)
	if !ok || len(arr) != 2 || arr[0] != "Custom one" || arr[1] != "Custom two" {
		t.Fatalf("variation_preheader_templates after PUT = %v, want [Custom one, Custom two]", g.body["value"])
	}

	// a non-string element is rejected.
	bad := e.do(t, "PUT", "/v1/admin/config/variation_preheader_templates",
		map[string]any{"value": []any{"ok", 42}}).requireStatus(t, 400)
	if bad.errorDetailIssue("value") != issueMalformed {
		t.Fatalf("non-string element: issue=%s, want malformed", bad.errorDetailIssue("value"))
	}

	// an empty array is a VALID value, not an error.
	e.do(t, "PUT", "/v1/admin/config/variation_preheader_templates", map[string]any{"value": []string{}}).requireStatus(t, 200)
	g2 := e.do(t, "GET", "/v1/admin/config/variation_preheader_templates", nil).requireStatus(t, 200)
	arr2, _ := g2.body["value"].([]any)
	if len(arr2) != 0 {
		t.Fatalf("variation_preheader_templates after empty-array PUT = %v, want []", g2.body["value"])
	}
}

// TestVariationConfig_RateParametersRoundTripAndBounds — the three amended P8-OD-1 keys
// (variation_css_magnitude_px: integer >= 1; variation_zero_width_rate / variation_user_agent_rate:
// number in [0, 1]) accept valid values, reject out-of-range values with issueOutOfRange, and
// reject a wrong-typed value with issueMalformed — through the real endpoint.
func TestVariationConfig_RateParametersRoundTripAndBounds(t *testing.T) {
	e := newTestEnv(t, "varcfgrate")

	// variation_css_magnitude_px: integer, >= 1.
	e.do(t, "PUT", "/v1/admin/config/variation_css_magnitude_px", map[string]any{"value": 5}).requireStatus(t, 200)
	g := e.do(t, "GET", "/v1/admin/config/variation_css_magnitude_px", nil).requireStatus(t, 200)
	if v, _ := g.body["value"].(float64); v != 5 {
		t.Fatalf("variation_css_magnitude_px after PUT = %v, want 5", g.body["value"])
	}
	bad := e.do(t, "PUT", "/v1/admin/config/variation_css_magnitude_px", map[string]any{"value": 0}).requireStatus(t, 400)
	if bad.errorDetailIssue("value") != issueOutOfRange {
		t.Fatalf("variation_css_magnitude_px=0: issue=%s, want out_of_range", bad.errorDetailIssue("value"))
	}
	badType := e.do(t, "PUT", "/v1/admin/config/variation_css_magnitude_px", map[string]any{"value": "big"}).requireStatus(t, 400)
	if badType.errorDetailIssue("value") != issueMalformed {
		t.Fatalf("variation_css_magnitude_px wrong type: issue=%s, want malformed", badType.errorDetailIssue("value"))
	}

	// variation_zero_width_rate / variation_user_agent_rate: number in [0, 1], boundaries included.
	for _, key := range []string{"variation_zero_width_rate", "variation_user_agent_rate"} {
		e.do(t, "PUT", "/v1/admin/config/"+key, map[string]any{"value": 0.42}).requireStatus(t, 200)
		gr := e.do(t, "GET", "/v1/admin/config/"+key, nil).requireStatus(t, 200)
		if v, _ := gr.body["value"].(float64); v != 0.42 {
			t.Fatalf("%s after PUT = %v, want 0.42", key, gr.body["value"])
		}
		// boundaries 0 and 1 are both legitimate.
		e.do(t, "PUT", "/v1/admin/config/"+key, map[string]any{"value": 0}).requireStatus(t, 200)
		e.do(t, "PUT", "/v1/admin/config/"+key, map[string]any{"value": 1}).requireStatus(t, 200)
		// out of range on both sides.
		overBad := e.do(t, "PUT", "/v1/admin/config/"+key, map[string]any{"value": 1.5}).requireStatus(t, 400)
		if overBad.errorDetailIssue("value") != issueOutOfRange {
			t.Fatalf("%s=1.5: issue=%s, want out_of_range", key, overBad.errorDetailIssue("value"))
		}
		underBad := e.do(t, "PUT", "/v1/admin/config/"+key, map[string]any{"value": -0.1}).requireStatus(t, 400)
		if underBad.errorDetailIssue("value") != issueOutOfRange {
			t.Fatalf("%s=-0.1: issue=%s, want out_of_range", key, underBad.errorDetailIssue("value"))
		}
		// value left at the last successful write (1) after the rejected writes.
		gFinal := e.do(t, "GET", "/v1/admin/config/"+key, nil).requireStatus(t, 200)
		if v, _ := gFinal.body["value"].(float64); v != 1 {
			t.Fatalf("%s after rejected writes = %v, want unchanged 1", key, gFinal.body["value"])
		}
	}
}

// TestVariationConfig_QuotedNumericStringRejected — R3 review finding: encoding/json's json.Number
// decodes successfully from EITHER a bare numeric token (0.5) OR a quoted numeric string ("0.5"),
// so a "did json.Number decode" check alone is not a real type gate. Before the fix, a PUT with a
// JSON-STRING value like {"value":"0.5"} passed this endpoint's value_type check, got stored
// verbatim as {"v":"0.5"}, and then hard-failed dispatch's readFloatKey/readIntKey on every
// subsequent LoadConfig call — a write that looked like a clean 200 silently wedged the Control.
// Covers all three affected value_types this bug could reach: "integer" (variation_css_
// magnitude_px), "number" (the two rate keys), and "array<integer>" (any pre-existing array<integer>
// key would have the identical per-element gap).
func TestVariationConfig_QuotedNumericStringRejected(t *testing.T) {
	e := newTestEnv(t, "varcfgquoted")

	// "integer" value_type: a quoted numeric string must be rejected, not silently accepted.
	bad := e.do(t, "PUT", "/v1/admin/config/variation_css_magnitude_px", map[string]any{"value": "5"}).requireStatus(t, 400)
	if bad.errorDetailIssue("value") != issueMalformed {
		t.Fatalf(`variation_css_magnitude_px value:"5" (quoted): issue=%s, want malformed`, bad.errorDetailIssue("value"))
	}

	// "number" value_type: same for both amended rate keys.
	for _, key := range []string{"variation_zero_width_rate", "variation_user_agent_rate"} {
		got := e.do(t, "PUT", "/v1/admin/config/"+key, map[string]any{"value": "0.5"}).requireStatus(t, 400)
		if got.errorDetailIssue("value") != issueMalformed {
			t.Fatalf(`%s value:"0.5" (quoted): issue=%s, want malformed`, key, got.errorDetailIssue("value"))
		}
	}

	// Anti-vacuity: the exact same numeric value UNQUOTED must still be accepted (proves the
	// rejection above is about the JSON type, not the number itself).
	e.do(t, "PUT", "/v1/admin/config/variation_css_magnitude_px", map[string]any{"value": 5}).requireStatus(t, 200)
	e.do(t, "PUT", "/v1/admin/config/variation_zero_width_rate", map[string]any{"value": 0.5}).requireStatus(t, 200)
}
