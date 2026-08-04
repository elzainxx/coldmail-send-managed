package workerproto

// variation_wire_test.go — proves the JSON wire encoding of Variation's zero-value-sensitive
// numeric fields (CSSMagnitudePx, ZeroWidthRate, UserAgentRate, CSSSkipBelowPx — every P8-OD-1/
// P9.5-OD-3 amended parameter) genuinely survives a real json.Marshal/Unmarshal round trip at
// their zero value, not just "the Go struct field equals 0 in memory" (which every field would
// satisfy trivially and would NOT catch an incorrect `omitempty` tag — that bug only manifests at
// the actual JSON encoding boundary, R3 review gate finding).

import (
	"encoding/json"
	"testing"
)

// TestVariation_ZeroValueNumericFieldsSurviveJSONRoundTrip — CSSMagnitudePx=0/ZeroWidthRate=0/
// UserAgentRate=0/CSSSkipBelowPx=0 must all be present (not omitted) on the wire and must decode
// back to 0, not to any other value. This is the exact JSON-boundary check that would have caught
// the CSSSkipBelowPx `omitempty` regression (a field is distinguishable from "absent" only when
// the encoder actually emits it).
func TestVariation_ZeroValueNumericFieldsSurviveJSONRoundTrip(t *testing.T) {
	v := Variation{
		CSSMagnitudePx: 0,
		ZeroWidthRate:  0,
		UserAgentRate:  0,
		CSSSkipBelowPx: 0,
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal to map: %v", err)
	}

	for _, field := range []string{"css_magnitude_px", "zero_width_rate", "user_agent_rate", "css_skip_below_px"} {
		val, present := m[field]
		if !present {
			t.Errorf("field %q is ABSENT from the JSON wire encoding at its zero value — an omitempty tag is silently discarding a legitimate 0 (the exact P8-OD-1/P9.5-OD-3 bug class): %s", field, raw)
			continue
		}
		if string(val) != "0" {
			t.Errorf("field %q = %s, want the literal 0", field, val)
		}
	}

	// Round-trip: decoding the marshaled bytes back into a Variation must reproduce the zero values
	// exactly (not silently collapse to some other default via the resolver — that's the worker's job,
	// but the wire struct itself must be faithful).
	var v2 Variation
	if err := json.Unmarshal(raw, &v2); err != nil {
		t.Fatalf("unmarshal to Variation: %v", err)
	}
	if v2.CSSMagnitudePx != 0 || v2.ZeroWidthRate != 0 || v2.UserAgentRate != 0 || v2.CSSSkipBelowPx != 0 {
		t.Errorf("round-tripped Variation = %+v, want every amended numeric field still 0", v2)
	}
}
