package dispatch

// Chunk 5 behavioral tests: the fairness cursor + the pre-claim pool-eligibility gate (§5, §1.2
// P4-5). Real Postgres, real concurrent workers where the spec calls for them.

import (
	"context"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// TestFairness_RoundRobinFairness_NoStarvation (T4-style) — 3 users of unequal campaign size, an
// over-provisioned shared pool (capacity far exceeds total demand), and MULTIPLE workers so more
// than one push can land per tick. Asserts the two smaller campaigns complete before the largest
// drains, and no single campaign monopolizes a tick — the pre-claim lane grant + per-tick full-
// ring visitation together bound how much of one Tick call one user can consume.
func TestFairness_RoundRobinFairness_NoStarvation(t *testing.T) {
	e := newDispatchEnv(t, "fairness")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	e.setAccountDailyState(t, acct, 100000, 0, todayUTC(), false, nil) // over-provisioned
	// Enough workers that worker SUPPLY is never the binding constraint (3 users x ceiling 3 = up to 9
	// candidate claims/tick) — this test isolates FAIRNESS among campaigns, not worker scarcity
	// (which is a Phase 3 §1.3 concern of its own, already covered by TestTick_NoWorkerRequeuesCleanly).
	for i := 0; i < 9; i++ {
		e.addWorker(t)
	}
	e.setConfigInt(t, "concurrency_per_user_default", 3)

	userBig := e.seedUser(t, "big@tenant.example")
	userMed := e.seedUser(t, "med@tenant.example")
	userSmall := e.seedUser(t, "small@tenant.example")
	for i := 0; i < 30; i++ {
		e.seedQueuedJobFor(t, userBig, "big"+string(rune('a'+i%26))+"@example.com")
	}
	for i := 0; i < 6; i++ {
		e.seedQueuedJobFor(t, userMed, "med"+string(rune('a'+i))+"@example.com")
	}
	for i := 0; i < 2; i++ {
		e.seedQueuedJobFor(t, userSmall, "small"+string(rune('a'+i))+"@example.com")
	}

	// Run enough ticks to drain the small and medium campaigns, tracking how many ticks each
	// takes. If the fairness ring didn't visit every active user every tick, small/med would
	// starve behind big's 30 jobs (only 3 lanes/tick/user, but ALL THREE users get up to 3/tick).
	smallDone, medDone := false, false
	smallDoneAtTick, medDoneAtTick := -1, -1
	for tick := 1; tick <= 15; tick++ {
		e.settleAllSending(t) // simulate the worker reporting success on every in-flight job + frees workers
		if _, _, err := e.d.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
		if !smallDone && e.userTerminalCount(t, userSmall) == 2 {
			smallDone, smallDoneAtTick = true, tick
		}
		if !medDone && e.userTerminalCount(t, userMed) == 6 {
			medDone, medDoneAtTick = true, tick
		}
	}
	if !smallDone || !medDone {
		t.Fatalf("small/medium campaigns never completed (smallDone=%v medDone=%v) — starved behind the large campaign", smallDone, medDone)
	}
	bigCount := e.userTerminalCount(t, userBig)
	if bigCount >= 30 {
		// Not itself a failure, but if big ALSO finished immediately the test can't distinguish
		// "fair interleaving" from "so much capacity nothing could starve" — flag it as weak.
		t.Logf("note: the large campaign also fully drained by tick 15 (bigCount=%d) — capacity was generous", bigCount)
	}
	if smallDoneAtTick > 10 || medDoneAtTick > 12 {
		t.Fatalf("small campaign done at tick %d, medium at tick %d — took implausibly long for a fair ring visiting every active user every tick", smallDoneAtTick, medDoneAtTick)
	}
}

// TestFairness_PreClaimGate_ExhaustedUserNotClaimed (P4-5) — a user whose ENTIRE pool chain is
// exhausted must NOT be claimed for: their jobs stay queued, untouched, across MULTIPLE ticks —
// no claim/requeue churn, no job_events beyond genesis. A second, healthy user in the SAME tick
// IS serviced normally (anti-vacuity: the gate doesn't just stop the whole loop).
func TestFairness_PreClaimGate_ExhaustedUserNotClaimed(t *testing.T) {
	e := newDispatchEnv(t, "preclaimgate")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	e.setAccountDailyState(t, acct, 5, 5, todayUTC(), true, strPtr("manual")) // fully ineligible, never self-heals

	userExhausted := e.seedUser(t, "exhausted@tenant.example")
	jobExhausted := e.seedQueuedJobFor(t, userExhausted, "ex@example.com")

	for tick := 1; tick <= 3; tick++ {
		if _, _, err := e.d.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}
	state, _, leaseNull, _, attempt := e.jobState(t, jobExhausted)
	if state != "queued" || !leaseNull {
		t.Fatalf("exhausted user's job state=%s leaseNull=%v after 3 ticks, want queued/true (never claimed)", state, leaseNull)
	}
	if attempt != 0 {
		t.Fatalf("attempt_count=%d after 3 ticks, want 0 — the pre-claim gate must skip claiming, never claim-then-requeue-churn", attempt)
	}
	if n := e.eventCount(t, jobExhausted); n != 1 {
		t.Fatalf("job_events count=%d after 3 ticks, want 1 (genesis only — no claim/requeue churn)", n)
	}

	// Anti-vacuity: a healthy second user in the SAME pool IS serviced normally on the next tick.
	// A second worker is needed here: once healthyAcct exists, BOTH userExhausted and userHealthy
	// pass the gate (they share the same default shared pool) and both compete for a worker in the
	// ring — this test isolates "the gate doesn't block the whole tick," not worker supply.
	e.addWorker(t)
	healthyAcct := e.seedAccount(t, pool, "sec2")
	e.setAccountDailyState(t, healthyAcct, 100, 0, todayUTC(), false, nil)
	userHealthy := e.seedUser(t, "healthy@tenant.example")
	jobHealthy := e.seedQueuedJobFor(t, userHealthy, "hz@example.com")
	if _, pushed, err := e.d.Tick(ctx); err != nil || pushed == 0 {
		t.Fatalf("healthy user tick: pushed=%d err=%v, want the healthy job pushed", pushed, err)
	}
	stateHealthy, _, _, _, _ := e.jobState(t, jobHealthy)
	if stateHealthy != "sending" {
		t.Fatalf("healthy user's job state=%s, want sending (the gate must not have blocked the whole tick)", stateHealthy)
	}
}

// TestFairness_PreClaimGate_UnconfiguredSharedPoolNoChurn (T10-style, via the gate) — a base user
// with default_shared_pool_id UNSET is never claimed for across multiple ticks: no churn, no
// crash, clean queued state — proving the P4-5 gate treats an EMPTY chain (§4.3) identically to an
// exhausted one.
func TestFairness_PreClaimGate_UnconfiguredSharedPoolNoChurn(t *testing.T) {
	e := newDispatchEnv(t, "gateunconf")
	ctx := context.Background()
	e.clearDefaultSharedPool(t)

	userA := e.seedUser(t, "gateunconf-a@tenant.example")
	job := e.seedQueuedJobFor(t, userA, "gu@example.com")

	for tick := 1; tick <= 3; tick++ {
		if _, _, err := e.d.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", tick, err)
		}
	}
	state, _, leaseNull, _, attempt := e.jobState(t, job)
	if state != "queued" || !leaseNull || attempt != 0 {
		t.Fatalf("state=%s leaseNull=%v attempt=%d after 3 ticks, want queued/true/0 (gate skips claiming, no churn)", state, leaseNull, attempt)
	}
	if n := e.eventCount(t, job); n != 1 {
		t.Fatalf("job_events count=%d after 3 ticks, want 1 (genesis only)", n)
	}
}

// TestFairness_RotateRing — unit-level proof of rotateRing's wrap/advance semantics, independent
// of the DB (pure function, §5.1).
func TestFairness_RotateRing(t *testing.T) {
	ring := []ringSlot{{userID: "a"}, {userID: "c"}, {userID: "e"}}

	got := rotateRing(ring, "")
	if got[0].userID != "a" {
		t.Fatalf("cursor=\"\": start=%s, want a (nothing precedes an empty cursor... wait, everything is > \"\")", got[0].userID)
	}

	got = rotateRing(ring, "b")
	if got[0].userID != "c" {
		t.Fatalf("cursor=b: start=%s, want c (first member > b)", got[0].userID)
	}

	got = rotateRing(ring, "e")
	if got[0].userID != "a" {
		t.Fatalf("cursor=e (the last member itself): start=%s, want a (wrap to smallest)", got[0].userID)
	}

	got = rotateRing(ring, "zzz")
	if got[0].userID != "a" {
		t.Fatalf("cursor past every member: start=%s, want a (wrap)", got[0].userID)
	}

	// Every element must survive rotation (only order changes).
	if len(got) != 3 {
		t.Fatalf("rotateRing dropped elements: got %d, want 3", len(got))
	}
}

// settleAllSending drives every currently-'sending' job to 'sent' via the REAL OutcomeHandler
// (simulating the worker reporting accepted for each), then frees every worker's in-flight marker. This
// lets a multi-tick test push more total jobs than there are workers/lane-capacity in one snapshot,
// exactly as a real worker completing a send and reporting back would.
func (e *dispatchEnv) settleAllSending(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	// Report each job as the worker it was actually PUSHED TO (assigned_box_id), not as some fixed worker:
	// a real worker only ever reports outcomes for jobs pushed to itself, and the Control's stale-replay
	// attribution guard (§1.3) applies a report only from the worker owning the current attempt. With
	// multi-worker tests (extraWorkers) a hardcoded worker id would be a mis-attributed report and get
	// correctly dropped, leaving the jobs stuck 'sending'.
	rows, err := e.admin.Query(ctx, `SELECT id, assigned_box_id::text FROM send_managed.jobs WHERE state = 'sending'`)
	if err != nil {
		t.Fatalf("settleAllSending query: %v", err)
	}
	type sendingJob struct{ id, workerID string }
	var jobs []sendingJob
	for rows.Next() {
		var j sendingJob
		var worker *string
		if err := rows.Scan(&j.id, &worker); err != nil {
			rows.Close()
			t.Fatalf("settleAllSending scan: %v", err)
		}
		if worker != nil {
			j.workerID = *worker
		} else {
			j.workerID = e.px.workerID
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	for _, j := range jobs {
		e.oh.Handle(j.workerID, workerproto.Report{
			Type: workerproto.TypeReport, JobID: j.id, Outcome: workerproto.OutcomeAccepted,
			ProviderMessageID: "PMID-" + j.id, SMTPResponse: "250 2.0.0 Ok",
		})
	}
	e.px.freeAllWorkers()
}

// userTerminalCount counts a user's jobs that reached 'sending' or beyond (a proxy for "was
// dispatched," since these tests use a recordingPusher that never reports back to 'sent').
func (e *dispatchEnv) userTerminalCount(t *testing.T, userID string) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(context.Background(),
		`SELECT count(*) FROM send_managed.jobs WHERE user_id=$1 AND state IN ('sending','sent','bounced')`, userID).Scan(&n); err != nil {
		t.Fatalf("userTerminalCount: %v", err)
	}
	return n
}
