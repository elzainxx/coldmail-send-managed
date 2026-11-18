package adminapi

// worker_reconnect_config_test.go — the operator surface of the three worker reconnect dials (send_managed_0029,
// Phase 3 close-out B1): worker_reconnect_base_ms (default 1000, 10..60000), worker_reconnect_cap_ms (default 30000,
// 10..600000) and worker_reconnect_jitter_pct (default 20, 0..100).
//
// The READ half — that the worker consumes them, backs off by them and never gives up — is proven by the worker's
// own tests (services/worker/test/reconnect.test.mjs) and the live outage suite (tests/harness/phase13_reconnect.sh).
// This file proves what the operator touches: the keys exist with the documented production defaults and type, the
// MINIMUM LEGAL values (10 ms, 10 ms, 0 %) are accepted (the platform harness runs 50 ms / 200 ms), one step outside
// each range is refused with the field named and changes nothing (rejected, never clamped), a wrong JSON type is
// refused, and the base may exceed the cap (no cross-key ordering: the wait is min(cap, base*2^(n-1))).
//
// Real Postgres, real HTTP against the production Server; every boundary has its paired accept/refuse.

import "testing"

type reconnectDial struct {
	key          string
	def, lo, hi  int
	justOutside  []any
	wrongTypeBad []any
}

func reconnectDials() []reconnectDial {
	return []reconnectDial{
		{"worker_reconnect_base_ms", 1000, 10, 60000, []any{9, 0, -1, 60001}, []any{"1000", 2.5, true}},
		{"worker_reconnect_cap_ms", 30000, 10, 600000, []any{9, 0, -1, 600001}, []any{"30000", 2.5, true}},
		{"worker_reconnect_jitter_pct", 20, 0, 100, []any{-1, 101}, []any{"20", 2.5, true}},
	}
}

func TestWorkerReconnectConfig_SeededWithTheDocumentedProductionDefaults(t *testing.T) {
	e := newTestEnv(t, "wrcseed")
	for _, d := range reconnectDials() {
		r := e.do(t, "GET", "/v1/admin/config/"+d.key, nil).requireStatus(t, 200)
		if r.body["value"] != float64(d.def) || r.body["default_value"] != float64(d.def) {
			t.Fatalf("%s: value=%v default_value=%v, want both %d: %s", d.key, r.body["value"], r.body["default_value"], d.def, r.raw)
		}
		if r.body["value_type"] != "integer" {
			t.Fatalf("%s: value_type=%v, want integer", d.key, r.body["value_type"])
		}
		if desc, _ := r.body["description"].(string); len(desc) < 150 {
			t.Fatalf("%s: description is %d chars — an operator must be told what the dial does, its range and its minimum legal value", d.key, len(desc))
		}
	}
}

func TestWorkerReconnectConfig_RangeEnforcedAtBothEnds_RejectedNeverClamped(t *testing.T) {
	e := newTestEnv(t, "wrcrange")
	for _, d := range reconnectDials() {
		path := "/v1/admin/config/" + d.key
		// the boundaries (the minimum legal value included) are accepted and read back
		for _, ok := range []int{d.lo, d.def, d.hi} {
			e.do(t, "PUT", path, map[string]any{"value": ok}).requireStatus(t, 200)
			if got := e.do(t, "GET", path, nil).requireStatus(t, 200); got.body["value"] != float64(ok) {
				t.Fatalf("%s: after PUT %d the value reads %v", d.key, ok, got.body["value"])
			}
		}
		// pin a known value, then prove every refusal leaves it exactly as it was
		e.do(t, "PUT", path, map[string]any{"value": d.lo}).requireStatus(t, 200)
		for _, bad := range append(append([]any{}, d.justOutside...), d.wrongTypeBad...) {
			r := e.do(t, "PUT", path, map[string]any{"value": bad})
			if r.status != 400 || r.errorCode() != "validation_failed" {
				t.Fatalf("%s: PUT %v: status=%d code=%q, want 400 validation_failed: %s", d.key, bad, r.status, r.errorCode(), r.raw)
			}
			if r.errorDetailIssue("value") == "" {
				t.Fatalf("%s: PUT %v: the 400 names no issue on `value`: %s", d.key, bad, r.raw)
			}
		}
		for _, bad := range d.justOutside {
			if issue := e.do(t, "PUT", path, map[string]any{"value": bad}).errorDetailIssue("value"); issue != "out_of_range" {
				t.Fatalf("%s: PUT %v: issue=%q, want out_of_range (a number outside the range is not `malformed`)", d.key, bad, issue)
			}
		}
		if got := e.do(t, "GET", path, nil).requireStatus(t, 200); got.body["value"] != float64(d.lo) {
			t.Fatalf("%s: a refused write altered the stored value: %v (want %d: rejected, never clamped)", d.key, got.body["value"], d.lo)
		}
	}
}

func TestWorkerReconnectConfig_BaseMayExceedCap_NoCrossKeyOrdering(t *testing.T) {
	e := newTestEnv(t, "wrcorder")
	// base 60000 above cap 100: legal (every wait is then the cap), in either order of writing.
	e.do(t, "PUT", "/v1/admin/config/worker_reconnect_cap_ms", map[string]any{"value": 100}).requireStatus(t, 200)
	e.do(t, "PUT", "/v1/admin/config/worker_reconnect_base_ms", map[string]any{"value": 60000}).requireStatus(t, 200)
	e.do(t, "PUT", "/v1/admin/config/worker_reconnect_base_ms", map[string]any{"value": 10}).requireStatus(t, 200)
	e.do(t, "PUT", "/v1/admin/config/worker_reconnect_cap_ms", map[string]any{"value": 10}).requireStatus(t, 200)
}
