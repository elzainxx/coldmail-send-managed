package adminapi

// attempt_cap_config_test.go — the write-time half of the two attempt-cap dials (send_managed_0028, M2-G2):
// job_max_failed_attempts (default 6, 1..1000) and job_failed_max_age_sec (default 86400, 60..2592000).
//
// The READ half — that the Control consumes them, and that the cap fires exactly at the dial — is proven in
// internal/dispatch/attempts_cap_test.go. This file proves the operator surface: the keys exist with the
// documented defaults and types, a value outside the documented range is refused with the field named
// (never silently stored — a stored 0 would fail every job on its first failed attempt), a value inside it
// is accepted and read back, and a wrong JSON type is refused.
//
// Real Postgres, real HTTP against the production Server; each boundary has its paired accept/refuse.

import "testing"

func TestAttemptCapConfig_SeededWithTheDocumentedDefaults(t *testing.T) {
	e := newTestEnv(t, "capcfgseed")
	for _, tc := range []struct {
		key  string
		want float64
	}{
		{"job_max_failed_attempts", 6},
		{"job_failed_max_age_sec", 86400},
	} {
		r := e.do(t, "GET", "/v1/admin/config/"+tc.key, nil).requireStatus(t, 200)
		if r.body["value"] != tc.want || r.body["default_value"] != tc.want {
			t.Fatalf("%s: value=%v default_value=%v, want both %v: %s", tc.key, r.body["value"], r.body["default_value"], tc.want, r.raw)
		}
		if r.body["value_type"] != "integer" {
			t.Fatalf("%s: value_type=%v, want integer: %s", tc.key, r.body["value_type"], r.raw)
		}
		if d, _ := r.body["description"].(string); len(d) < 100 {
			t.Fatalf("%s: description is %d chars — an operator reading this dial must be told what it counts and what it does NOT count: %s", tc.key, len(d), r.raw)
		}
	}
}

func TestAttemptCapConfig_JobMaxFailedAttemptsRangeEnforced(t *testing.T) {
	e := newTestEnv(t, "capcfgattempts")
	const path = "/v1/admin/config/job_max_failed_attempts"
	// 0 is refused, NOT read as "disabled": it would fail every job on its first failed attempt.
	for _, bad := range []any{0, -1, 1001, "6", 2.5, true} {
		r := e.do(t, "PUT", path, map[string]any{"value": bad})
		if r.status != 400 {
			t.Fatalf("PUT %v: status=%d, want 400 (out of range or wrong type): %s", bad, r.status, r.raw)
		}
		if r.errorDetailIssue("value") == "" {
			t.Fatalf("PUT %v: the 400 names no issue on `value`: %s", bad, r.raw)
		}
	}
	// The boundaries are accepted and read back.
	for _, ok := range []int{1, 6, 1000} {
		e.do(t, "PUT", path, map[string]any{"value": ok}).requireStatus(t, 200)
		if got := e.do(t, "GET", path, nil).requireStatus(t, 200); got.body["value"] != float64(ok) {
			t.Fatalf("after PUT %d the value reads %v", ok, got.body["value"])
		}
	}
	// A refused write changed nothing.
	if got := e.do(t, "GET", path, nil).requireStatus(t, 200); got.body["value"] != 1000.0 {
		t.Fatalf("a refused write altered the stored value: %v", got.body["value"])
	}
}

func TestAttemptCapConfig_JobFailedMaxAgeSecRangeEnforced(t *testing.T) {
	e := newTestEnv(t, "capcfgage")
	const path = "/v1/admin/config/job_failed_max_age_sec"
	for _, bad := range []any{0, 59, 2592001, -5, "86400", 3600.5} {
		r := e.do(t, "PUT", path, map[string]any{"value": bad})
		if r.status != 400 {
			t.Fatalf("PUT %v: status=%d, want 400: %s", bad, r.status, r.raw)
		}
	}
	for _, ok := range []int{60, 86400, 2592000} {
		e.do(t, "PUT", path, map[string]any{"value": ok}).requireStatus(t, 200)
		if got := e.do(t, "GET", path, nil).requireStatus(t, 200); got.body["value"] != float64(ok) {
			t.Fatalf("after PUT %d the value reads %v", ok, got.body["value"])
		}
	}
}
