package adminapi

// health_errors_window_test.go — GET /v1/admin/health's errors_last_window counts ONE failure ONCE.
//
// WHY THIS EXISTS. Found walking Stage 9 of Docs/coldmail-send-managed-local-test-guide.md against the live
// docker stack. queryErrorsInWindow was `COUNT(*) FROM send_managed.job_events WHERE error_class IS NOT
// NULL`, and send_managed_0008's triggers copied the job row's STICKY last_error_class onto EVERY event
// (fixed in the trigger by send_managed_0022). So a single requeue produced four tagged rows — the requeue
// itself plus the queued->assigned, assigned->sending, and sending->SENT that followed — and the
// endpoint reported errors_last_window {"unknown": 4} for ONE failure.
//
// That is an operator-facing harm, not a cosmetic one: Docs/operator-runbook.md's "Sends are failing"
// triage reads this number FIRST to size an incident, and Docs/monitoring.md graphs it. A 4x
// over-count mis-sizes a real incident and manufactures phantom ones.
//
// job_events is append-only by design (send_managed_0008's reject-update trigger + the REVOKE), so the
// already-written bad rows were deliberately NOT rewritten to launder history. The reader is
// therefore ALSO filtered on the transition shape, which is what this test pins: it seeds rows that
// look exactly like the pre-send_managed_0022 smear and proves the endpoint no longer counts them.
//
// ANTI-VACUITY (Rule 10). Paired controls in ONE assertion set, over the SAME window:
//   - NEGATIVE: an error_class stamped on a non-error transition (INCLUDING sending->sent) is NOT
//     counted. Reverting the reader's filter fails this.
//   - POSITIVE: genuine error transitions ARE counted, once each, under their own class. A reader
//     that just returned an empty map — or filtered on the class instead of the shape — fails this.
// A regression cannot satisfy both. The negative rows are seeded with a DIFFERENT class from the
// positive ones, so a count that silently included them would be visibly wrong per-class, not merely
// wrong in total.

import (
	"testing"
)

// seedTaggedEvent inserts ONE job_events row directly, with a chosen transition and error_class.
//
// Direct INSERT is deliberate here and is not a Rule-3 violation: the point is to reproduce rows that
// a PRE-send_managed_0022 trigger would have written, which the current trigger correctly refuses to write.
// Driving the live trigger could not produce them, so there would be nothing to test — the reader's
// filter exists precisely because such rows are already durably in real databases. The rows are real
// rows in a real table, written through the real constraints (job_events_from_state_check still
// applies), and what is under test is the READER. The trigger-side behavior is covered separately by
// dispatch's TestJobEvents_ErrorContextOnlyOnErrorTransitions, which uses no direct inserts at all.
func (e *testEnv) seedTaggedEvent(t *testing.T, jobID, fromState, toState, class string) {
	t.Helper()
	var from any
	if fromState != "" {
		from = fromState
	}
	if _, err := e.admin.Exec(bgCtx(),
		`INSERT INTO send_managed.job_events
		   (job_id, user_id, from_state, to_state, actor, error_class, error_message)
		 VALUES ($1, $2, $3::send_managed.job_state, $4::send_managed.job_state, 'dispatcher',
		         $5::send_managed.error_class, 'seeded for errors_last_window test')`,
		jobID, sysUserHealth, from, toState, class); err != nil {
		t.Fatalf("seed tagged event (%s->%s, %s): %v", fromState, toState, class, err)
	}
}

// TestHealth_ErrorsLastWindowCountsOnlyErrorTransitions — the two controls above, read through the
// real production handler over real HTTP.
func TestHealth_ErrorsLastWindowCountsOnlyErrorTransitions(t *testing.T) {
	e := newTestEnv(t, "healtherrwin")

	// A real job to hang the events off (job_events.job_id is a FK). Seeding it 'queued' means its
	// own genesis event carries no error context, so it cannot perturb either count.
	job := e.seedHealthJob(t, "queued")

	// --- NEGATIVE rows: the pre-send_managed_0022 smear, class 'unknown'. ---
	// Exactly the three spurious shapes from the live trace. sending->sent is the important one: a
	// SUCCESS that claimed to have failed.
	e.seedTaggedEvent(t, job, "queued", "assigned", "unknown")
	e.seedTaggedEvent(t, job, "assigned", "sending", "unknown")
	e.seedTaggedEvent(t, job, "sending", "sent", "unknown")

	// --- POSITIVE rows: genuine error transitions, deliberately DIFFERENT classes. ---
	// One of each error shape the state machine has: a requeue (non-genesis -> queued), a terminal
	// failure, and a terminal bounce. Distinct classes mean a leaked negative row would show up as a
	// wrong 'unknown' key rather than inflating a class that is supposed to be non-zero anyway.
	e.seedTaggedEvent(t, job, "sending", "queued", "transport_4xx")
	e.seedTaggedEvent(t, job, "sending", "failed", "timeout")
	e.seedTaggedEvent(t, job, "sending", "bounced", "bounce_permanent")

	// Widen the window so every seeded row is inside it — otherwise a zero result would be
	// indistinguishable from a correct filter, and the positive control would be vacuous.
	e.setHealthConfigInt(t, "health_metrics_window_sec", 3600)

	resp := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	errsAny, ok := resp.body["errors_last_window"]
	if !ok {
		t.Fatalf("health response has no errors_last_window field; body=%s", resp.raw)
	}
	errs, ok := errsAny.(map[string]any)
	if !ok {
		t.Fatalf("errors_last_window is %T, want an object; body=%s", errsAny, resp.raw)
	}

	// The response shape contract (§3.2 / Docs/api-contract.md): all six classes always present, 0
	// included, so a 0 is genuine data rather than a missing key. Assert it here too — the filter
	// change must not perturb the shape.
	for _, class := range []string{"bounce_permanent", "transport_4xx", "auth_failure", "throttle", "timeout", "unknown"} {
		if _, present := errs[class]; !present {
			t.Fatalf("errors_last_window is missing the %q key — all six classes are always present (0 included) so operators can tell 'zero' from 'no data'; body=%s", class, resp.raw)
		}
	}

	// --- NEGATIVE CONTROL: the three smeared rows are NOT counted. ---
	if got := intField(t, errs, "unknown"); got != 0 {
		t.Fatalf("errors_last_window.unknown = %d, want 0. Three rows carrying error_class='unknown' were seeded on NON-error transitions (queued->assigned, assigned->sending, and sending->SENT) — exactly what send_managed_0008's ungated trigger wrote before send_managed_0022. Counting them is the live 4x incident-sizing inflation: the runbook's 'Sends are failing' triage reads this number first, and one of these rows is a SUCCESSFUL send. queryErrorsInWindow must filter on the TRANSITION SHAPE, not merely on error_class IS NOT NULL; body=%s", got, resp.raw)
	}

	// --- POSITIVE CONTROL: each genuine error transition counted exactly once. ---
	// This is what stops "return nothing" or "filter on the class" from passing: under-reporting
	// errors_last_window hides real incidents, which is strictly worse than over-reporting them.
	for class, want := range map[string]int{
		"transport_4xx":    1, // sending -> queued  (a requeue)
		"timeout":          1, // sending -> failed  (terminal)
		"bounce_permanent": 1, // sending -> bounced (terminal)
	} {
		if got := intField(t, errs, class); got != want {
			t.Fatalf("errors_last_window.%s = %d, want %d — this is a GENUINE error transition and must still be counted. send_managed_0022 + the reader filter NARROW which rows count; they must never stop a real failure from being reported, or an operator sees a quiet dashboard during a live incident; body=%s", class, got, want, resp.raw)
		}
	}

	// Classes nothing was seeded for stay at 0 — proves the filter is not, say, counting every row
	// under every key.
	for _, class := range []string{"auth_failure", "throttle"} {
		if got := intField(t, errs, class); got != 0 {
			t.Fatalf("errors_last_window.%s = %d, want 0 — no %s row was seeded; body=%s", class, got, class, resp.raw)
		}
	}
}
