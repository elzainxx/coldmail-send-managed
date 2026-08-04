package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// accountScoreState reads the three columns a reclaim must NOT touch (the P6-10 carve-out surface).
func (e *dispatchEnv) accountScoreState(t *testing.T, acctID string) (score, cf int, lastFail *time.Time) {
	t.Helper()
	if err := e.admin.QueryRow(context.Background(),
		`SELECT account_status_score, consecutive_failures, last_failure_at FROM send_managed.accounts WHERE id=$1`, acctID).
		Scan(&score, &cf, &lastFail); err != nil {
		t.Fatalf("read account score state: %v", err)
	}
	return score, cf, lastFail
}

// TestCarveout_ReclaimDoesNotScore_WithReportedControl (THE HEADLINE, P6-10 + T7) — a Control-declared
// reclaim (dead worker / lease expiry) leaves the account's score, consecutive_failures, and
// last_failure_at BYTE-UNCHANGED and writes NO account_events. Its PAIRED POSITIVE CONTROL: the
// SAME error_class='timeout' delivered as a worker-REPORTED outcome on an identical fresh account DOES
// recompute the score (consecutive_failures +=1). Same class, different provenance (declared vs
// reported), deliberately different account effect — proving the carve-out is a real exemption, not
// a dead/broken scoring path.
func TestCarveout_ReclaimDoesNotScore_WithReportedControl(t *testing.T) {
	e := newDispatchEnv(t, "carveout")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	// --- The carve-out: a reclaimed job must not move its account's score. ---
	job := e.seedQueuedJob(t, "reclaim@example.com")
	e.dispatchToSending(t, job)
	var acctID string
	if err := e.admin.QueryRow(ctx, `SELECT assigned_account_id FROM send_managed.jobs WHERE id=$1`, job).Scan(&acctID); err != nil {
		t.Fatalf("read assigned account: %v", err)
	}
	score0, cf0, lastFail0 := e.accountScoreState(t, acctID)

	// Force a lease-expiry reclaim (Detector B) on the one in-flight job.
	e.expireLease(t, job)
	e.setWorkerHeartbeat(t, e.px.workerID, 5*time.Second) // worker alive → it's a pure per-job reclaim
	if _, n, err := e.d.ReclaimTick(ctx); err != nil || n != 1 {
		t.Fatalf("ReclaimTick reclaimed n=%d err=%v, want 1/nil", n, err)
	}
	// Job reclaimed (queued, timeout) but the ACCOUNT is byte-unchanged.
	if st, _, leaseNull, _, _ := e.jobState(t, job); st != "queued" || !leaseNull {
		t.Fatalf("reclaimed job state=%s leaseNull=%v, want queued/true", st, leaseNull)
	}
	score1, cf1, lastFail1 := e.accountScoreState(t, acctID)
	if score1 != score0 {
		t.Fatalf("RECLAIM CHANGED SCORE %d -> %d (P6-10 carve-out: a Control-declared reclaim must not score)", score0, score1)
	}
	if cf1 != cf0 {
		t.Fatalf("RECLAIM CHANGED consecutive_failures %d -> %d (must be untouched, P6-10)", cf0, cf1)
	}
	if (lastFail0 == nil) != (lastFail1 == nil) {
		t.Fatalf("RECLAIM CHANGED last_failure_at nullity (must be untouched, P6-10)")
	}
	var aev int
	if err := e.admin.QueryRow(ctx, `SELECT count(*) FROM send_managed.account_events WHERE account_id=$1`, acctID).Scan(&aev); err != nil {
		t.Fatalf("account_events: %v", err)
	}
	if aev != 0 {
		t.Fatalf("reclaim wrote %d account_events, want 0 (no account effect, P6-10)", aev)
	}

	// --- PAIRED POSITIVE CONTROL: the SAME class via a worker-REPORTED timeout DOES score. ---
	// A fresh account seeded identically (score 100, cf 0). Park the first account so routing lands
	// the control job on the fresh one. Drive a worker-reported timeout (error_reason=timeout) through
	// the real outcome handler — Phase 5 §3.3 recomputes on a reported outcome.
	ctlAcct := e.seedAccount(t, pool, "sec-ctl")
	ctlScore0, ctlCf0, _ := e.accountScoreState(t, ctlAcct)
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET quarantined=TRUE WHERE id=$1`, acctID); err != nil {
		t.Fatalf("park first account: %v", err)
	}
	e.px.freeAllWorkers()
	ctlJob := e.seedQueuedJob(t, "reported@example.com")
	e.dispatchToSending(t, ctlJob)
	// Sanity: the control job used the fresh account, not the parked one.
	var usedAcct string
	if err := e.admin.QueryRow(ctx, `SELECT assigned_account_id FROM send_managed.jobs WHERE id=$1`, ctlJob).Scan(&usedAcct); err != nil {
		t.Fatalf("read control account: %v", err)
	}
	if usedAcct != ctlAcct {
		t.Fatalf("control job used account %s, want the fresh control account %s", usedAcct, ctlAcct)
	}
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: ctlJob, Outcome: workerproto.OutcomeError,
		ErrorReason: workerproto.ReasonTimeout, SMTPResponse: ""})
	ctlScore1, ctlCf1, _ := e.accountScoreState(t, ctlAcct)
	if ctlScore1 >= ctlScore0 {
		t.Fatalf("POSITIVE CONTROL FAILED: a worker-REPORTED timeout did NOT lower the score (%d -> %d) — the carve-out test would be vacuous if scoring were broken", ctlScore0, ctlScore1)
	}
	if ctlCf1 != ctlCf0+1 {
		t.Fatalf("POSITIVE CONTROL FAILED: a worker-REPORTED timeout did NOT bump consecutive_failures (%d -> %d)", ctlCf0, ctlCf1)
	}
}

// TestCarveout_NoCascadeQuarantine (P6-10 no-cascade teeth, T7) — a dead worker holding MANY in-flight
// jobs spread across SEVERAL distinct accounts is reclaimed at once; EVERY one of those accounts
// stays un-quarantined and un-penalized. This is the teeth against a naive "penalize on reclaim"
// that would let one worker outage cascade-quarantine healthy capacity.
func TestCarveout_NoCascadeQuarantine(t *testing.T) {
	e := newDispatchEnv(t, "nocascade")
	ctx := context.Background()
	pool := e.seedPool(t)

	// Three distinct accounts, one job each, all in flight on the one worker.
	var accts, jobs []string
	for i := 0; i < 3; i++ {
		accts = append(accts, e.seedAccount(t, pool, "sec"))
	}
	for i := 0; i < 3; i++ {
		j := e.seedQueuedJob(t, "cascade@example.com")
		e.dispatchToSending(t, j)
		e.px.freeAllWorkers()
		jobs = append(jobs, j)
	}
	// Snapshot every account's score state before the reclaim.
	type snap struct{ score, cf int }
	before := map[string]snap{}
	for _, a := range accts {
		s, cf, _ := e.accountScoreState(t, a)
		before[a] = snap{s, cf}
	}

	// The worker dies; all three jobs reclaimed at once.
	e.setWorkerHeartbeat(t, e.px.workerID, 400*time.Second)
	if _, n, err := e.d.ReclaimTick(ctx); err != nil || n != 3 {
		t.Fatalf("ReclaimTick n=%d err=%v, want 3/nil", n, err)
	}

	// EVERY account: score/cf byte-unchanged, NOT quarantined, NO account_events.
	for _, a := range accts {
		s, cf, _ := e.accountScoreState(t, a)
		if s != before[a].score || cf != before[a].cf {
			t.Fatalf("account %s changed by reclaim: score %d->%d cf %d->%d (no-cascade violated)", a, before[a].score, s, before[a].cf, cf)
		}
		_, quar, _ := e.accountFlagsNow(t, a)
		if quar {
			t.Fatalf("account %s was quarantined by a worker-death reclaim (cascade-quarantine — P6-10 violated)", a)
		}
		var aev int
		if err := e.admin.QueryRow(ctx, `SELECT count(*) FROM send_managed.account_events WHERE account_id=$1`, a).Scan(&aev); err != nil {
			t.Fatalf("events: %v", err)
		}
		if aev != 0 {
			t.Fatalf("account %s got %d account_events from a reclaim, want 0", a, aev)
		}
	}
	_ = jobs
}

// TestConfigOrdering_RejectsInvertedPair (P6-11, CLAUDE.md Rule 1) — LoadConfig REJECTS a set where
// smtp_operation_timeout_sec >= job_max_processing_time_sec (never clamps), and LOADS a valid set.
// This is the load-time enforcement the reclaim carve-out's correctness depends on.
func TestConfigOrdering_RejectsInvertedPair(t *testing.T) {
	e := newDispatchEnv(t, "ordering")
	ctx := context.Background()

	// Valid pair (default 30 < 120): loads clean.
	if _, err := LoadConfig(ctx, e.pool); err != nil {
		t.Fatalf("valid config pair failed to load: %v", err)
	}

	// Invert: smtp_operation_timeout_sec = 120 == lease 120 → violation (strict <, equality rejected).
	e.setConfigInt(t, "smtp_operation_timeout_sec", 120)
	if _, err := LoadConfig(ctx, e.pool); err == nil {
		t.Fatal("LoadConfig accepted smtp_operation_timeout_sec == job_max_processing_time_sec — must reject (Rule 1)")
	} else if err.Error() != "config_ordering_violation: smtp_operation_timeout_sec (120) must be < job_max_processing_time_sec (120)" {
		t.Fatalf("unexpected error: %v", err)
	}

	// Strictly greater: also rejected.
	e.setConfigInt(t, "smtp_operation_timeout_sec", 200)
	if _, err := LoadConfig(ctx, e.pool); err == nil {
		t.Fatal("LoadConfig accepted smtp_operation_timeout_sec > lease — must reject")
	}

	// Restore a valid pair: loads again (proves it was the ordering, not a latent failure).
	e.setConfigInt(t, "smtp_operation_timeout_sec", 30)
	if _, err := LoadConfig(ctx, e.pool); err != nil {
		t.Fatalf("restored valid pair failed to load: %v", err)
	}
}
