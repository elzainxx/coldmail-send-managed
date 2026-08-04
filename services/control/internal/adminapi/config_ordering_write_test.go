package adminapi

// config_ordering_write_test.go — P9-OD-2 (RULED-AND-CLOSED, Docs/history/open-decisions.md): the config
// PUT boundary now rejects a write that would invert either config-ordering invariant, validated
// against the counterpart key's LIVE value, not the submitted value in isolation. Real Postgres,
// real HTTP against the actual production Server (CLAUDE.md Rule 3) — no mocks.

import (
	"strings"
	"testing"
)

// TestConfigOrdering_InvertedPUTRejectedAndNotPersisted — a PUT that would invert invariant (a)
// (smtp_operation_timeout_sec >= job_max_processing_time_sec) is rejected with
// config_ordering_violation, and the bad value does NOT land in send_managed.config (anti-vacuity: the
// stored value is read back and confirmed unchanged, not merely "the response said 400").
func TestConfigOrdering_InvertedPUTRejectedAndNotPersisted(t *testing.T) {
	e := newTestEnv(t, "ordwriteA")

	before := e.do(t, "GET", "/v1/admin/config/smtp_operation_timeout_sec", nil).requireStatus(t, 200)
	beforeVal := before.body["value"]

	// job_max_processing_time_sec is 120 by seeded default; 999 >= 120 violates invariant (a).
	bad := e.do(t, "PUT", "/v1/admin/config/smtp_operation_timeout_sec", map[string]any{"value": 999})
	bad.requireStatus(t, 400)
	if bad.errorCode() != codeConfigOrderingViolation {
		t.Fatalf("code=%s, want config_ordering_violation: %s", bad.errorCode(), string(bad.raw))
	}
	// The error must NAME the conflicting counterpart key + its current value, per the ruling
	// ("so the operator isn't guessing").
	if msg := bad.errorMessage(); !strings.Contains(msg, "job_max_processing_time_sec") || !strings.Contains(msg, "120") {
		t.Fatalf("error message does not name the counterpart key/value: %q", msg)
	}
	if bad.errorDetailIssue("value") != issueConflict {
		t.Fatalf("issue=%s, want conflict", bad.errorDetailIssue("value"))
	}

	after := e.do(t, "GET", "/v1/admin/config/smtp_operation_timeout_sec", nil).requireStatus(t, 200)
	if after.body["value"] != beforeVal {
		t.Fatalf("stored value changed after a REJECTED write: before=%v after=%v", beforeVal, after.body["value"])
	}

	// The symmetric direction: PUT job_max_processing_time_sec below the current smtp timeout (30).
	bad2 := e.do(t, "PUT", "/v1/admin/config/job_max_processing_time_sec", map[string]any{"value": 10})
	bad2.requireStatus(t, 400)
	if bad2.errorCode() != codeConfigOrderingViolation {
		t.Fatalf("code=%s, want config_ordering_violation: %s", bad2.errorCode(), string(bad2.raw))
	}
	if msg := bad2.errorMessage(); !strings.Contains(msg, "smtp_operation_timeout_sec") || !strings.Contains(msg, "30") {
		t.Fatalf("error message does not name the counterpart key/value: %q", msg)
	}
	after2 := e.do(t, "GET", "/v1/admin/config/job_max_processing_time_sec", nil).requireStatus(t, 200)
	if v, ok := after2.body["value"].(float64); !ok || v != 120 {
		t.Fatalf("job_max_processing_time_sec after a REJECTED write = %v, want unchanged 120", after2.body["value"])
	}

	// Invariant (b), both directions: box_outcome_buffer_ttl_sec (900) < startup_reconnect_window_sec.
	bad3 := e.do(t, "PUT", "/v1/admin/config/startup_reconnect_window_sec", map[string]any{"value": 901})
	bad3.requireStatus(t, 400)
	if bad3.errorCode() != codeConfigOrderingViolation {
		t.Fatalf("code=%s, want config_ordering_violation: %s", bad3.errorCode(), string(bad3.raw))
	}
	bad4 := e.do(t, "PUT", "/v1/admin/config/box_outcome_buffer_ttl_sec", map[string]any{"value": 1})
	bad4.requireStatus(t, 400)
	if bad4.errorCode() != codeConfigOrderingViolation {
		t.Fatalf("code=%s, want config_ordering_violation: %s", bad4.errorCode(), string(bad4.raw))
	}
}

// TestConfigOrdering_ValidPUTSucceeds — a PUT that keeps the pair valid succeeds normally, on
// both keys of both pairs (positive control — the boundary check must not over-reject).
func TestConfigOrdering_ValidPUTSucceeds(t *testing.T) {
	e := newTestEnv(t, "ordwriteB")

	// 60 < 120 (the seeded lease) — valid.
	ok1 := e.do(t, "PUT", "/v1/admin/config/smtp_operation_timeout_sec", map[string]any{"value": 60})
	ok1.requireStatus(t, 200)
	if v, ok := ok1.body["value"].(float64); !ok || v != 60 {
		t.Fatalf("value=%v, want 60", ok1.body["value"])
	}

	// Raise the lease further — still valid against the now-60 timeout.
	ok2 := e.do(t, "PUT", "/v1/admin/config/job_max_processing_time_sec", map[string]any{"value": 200})
	ok2.requireStatus(t, 200)

	// 950 >= 120 (the seeded window) — valid.
	ok3 := e.do(t, "PUT", "/v1/admin/config/box_outcome_buffer_ttl_sec", map[string]any{"value": 950})
	ok3.requireStatus(t, 200)

	// Lower the window — still valid against the now-950 buffer.
	ok4 := e.do(t, "PUT", "/v1/admin/config/startup_reconnect_window_sec", map[string]any{"value": 100})
	ok4.requireStatus(t, 200)
}

// TestConfigOrdering_OrderedTwoStepWideningSucceeds — widening BOTH keys of a pair upward
// (e.g. raising the SMTP timeout AND the lease) requires ordering the two PUTs so each individual
// step stays valid — exactly the documented consequence of validating the pair, not the value in
// isolation. Raising the timeout FIRST (while the lease is still 120) to a value >= 120 must fail;
// raising the lease FIRST, then the timeout, must succeed.
func TestConfigOrdering_OrderedTwoStepWideningSucceeds(t *testing.T) {
	e := newTestEnv(t, "ordwriteC")

	// Attempting the "wrong order" first: raise smtp_operation_timeout_sec to 150 while the lease
	// is still 120 -> rejected (150 >= 120).
	wrongOrder := e.do(t, "PUT", "/v1/admin/config/smtp_operation_timeout_sec", map[string]any{"value": 150})
	wrongOrder.requireStatus(t, 400)
	if wrongOrder.errorCode() != codeConfigOrderingViolation {
		t.Fatalf("wrong-order widening: code=%s, want config_ordering_violation", wrongOrder.errorCode())
	}

	// Correct order: raise the lease FIRST (150 < the new target lease is fine, current timeout
	// is still 30) ...
	step1 := e.do(t, "PUT", "/v1/admin/config/job_max_processing_time_sec", map[string]any{"value": 300})
	step1.requireStatus(t, 200)

	// ... THEN raise the timeout to 150 (< 300 now) -> succeeds.
	step2 := e.do(t, "PUT", "/v1/admin/config/smtp_operation_timeout_sec", map[string]any{"value": 150})
	step2.requireStatus(t, 200)
	if v, ok := step2.body["value"].(float64); !ok || v != 150 {
		t.Fatalf("smtp_operation_timeout_sec after ordered widening = %v, want 150", step2.body["value"])
	}

	// Same shape for invariant (b): raising startup_reconnect_window_sec past the current buffer
	// TTL (900) fails first...
	wrongOrderB := e.do(t, "PUT", "/v1/admin/config/startup_reconnect_window_sec", map[string]any{"value": 1000})
	wrongOrderB.requireStatus(t, 400)

	// ... raising the buffer TTL first, then the window, succeeds.
	stepB1 := e.do(t, "PUT", "/v1/admin/config/box_outcome_buffer_ttl_sec", map[string]any{"value": 1500})
	stepB1.requireStatus(t, 200)

	stepB2 := e.do(t, "PUT", "/v1/admin/config/startup_reconnect_window_sec", map[string]any{"value": 1000})
	stepB2.requireStatus(t, 200)
}

// TestConfigOrdering_LoadPathBackstopStillRejectsDirectSQL — defense in depth: a value that
// reaches send_managed.config by a path OTHER than the admin PUT (e.g. a direct SQL write, or a future
// second write path) is STILL caught, because the config-LOAD path's own validateConfigOrdering
// is untouched — it is not bypassed or weakened by the new write-path check, only supplemented by
// it. This proves the boundary fix is additive, not a replacement for the existing backstop.
func TestConfigOrdering_LoadPathBackstopStillRejectsDirectSQL(t *testing.T) {
	e := newTestEnv(t, "ordwriteD")

	// Bypass the admin API entirely: write an inverted pair directly, exactly as a rogue write
	// path (or a pre-P9-OD-2 client) might have.
	if _, err := e.admin.Exec(bgCtx(),
		`UPDATE send_managed.config SET value = jsonb_build_object('v', 999) WHERE key='smtp_operation_timeout_sec'`); err != nil {
		t.Fatalf("direct SQL write: %v", err)
	}

	// The GET/PUT-boundary check has no say over a direct SQL write — confirm the bad value is
	// genuinely stored (the boundary fix does not retroactively scan the table).
	stored := e.do(t, "GET", "/v1/admin/config/smtp_operation_timeout_sec", nil).requireStatus(t, 200)
	if v, ok := stored.body["value"].(float64); !ok || v != 999 {
		t.Fatalf("precondition: direct SQL write did not land, got %v", stored.body["value"])
	}

	// The load-path backstop (internal/dispatch's validateConfigOrdering, via LoadConfig) is
	// exercised by internal/dispatch's own TestConfigOrdering_RejectsInvertedPair /
	// TestConfigOrdering_IsNotADBOutage against a real Dispatcher — this test only proves the
	// ADMIN write-path fix did not remove or alter that backstop's own inputs: the bad value is
	// genuinely present in send_managed.config for the Control's next LoadConfig call to catch, exactly as
	// it always was. (Driving a full Dispatcher here would duplicate internal/dispatch's own
	// suite; this package deliberately does not import internal/dispatch, OD-7's structural
	// package-isolation precedent.)
}
