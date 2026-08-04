package dispatch

// panicswitch_test.go — Phase 9.5 Chunk 1 (B1, §1). Real Postgres, real Tick() calls — no mocks
// (CLAUDE.md Rule 3). Proves the O1 behavioral contract: dispatch_enabled=false halts ALL new
// claims/pushes with zero loss (queued jobs stay queued, never failed/errored), while the reclaim
// loop keeps running (P9.5-OD-1, RULED) and resuming (true) picks up exactly where the queue stood.

import (
	"context"
	"testing"
)

// TestPanicSwitch_HaltsClaimAndPush_ZeroLoss (O1, core) — with dispatch_enabled=false, a Tick over
// a real queued job claims and pushes NOTHING: the job stays queued with no lease, no event beyond
// genesis, and the pusher never sees a push. Positive control: the SAME job, on a later tick with
// dispatch_enabled=true, dispatches normally — proving the halt was the switch, not a broken env.
func TestPanicSwitch_HaltsClaimAndPush_ZeroLoss(t *testing.T) {
	e := newDispatchEnv(t, "panicswitch")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	userA := e.seedUser(t, "panicswitch-a@tenant.example")
	job := e.seedQueuedJobFor(t, userA, "ps@example.com")

	e.setConfigBool(t, "dispatch_enabled", false)
	cfg, pushed, err := e.d.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick while paused: %v", err)
	}
	if cfg.DispatchEnabled {
		t.Fatalf("Tick's own loaded cfg.DispatchEnabled=true, want false — the flip did not take effect")
	}
	if pushed != 0 {
		t.Fatalf("pushed=%d while dispatch_enabled=false, want 0", pushed)
	}
	state, hasPMID, leaseNull, completed, attempt := e.jobState(t, job)
	if state != "queued" {
		t.Fatalf("job state=%s while paused, want queued (never failed/errored, zero loss)", state)
	}
	if !leaseNull || hasPMID || completed {
		t.Fatalf("job leaseNull=%v hasPMID=%v completed=%v while paused, want true/false/false (untouched)", leaseNull, hasPMID, completed)
	}
	if attempt != 0 {
		t.Fatalf("attempt_count=%d while paused, want 0 (no claim/requeue churn — nothing was ever claimed)", attempt)
	}
	if n := e.eventCount(t, job); n != 1 {
		t.Fatalf("job_events count=%d while paused, want 1 (genesis only — no claim, no churn)", n)
	}
	if e.px.pushCount() != 0 {
		t.Fatalf("pusher recorded %d push(es) while paused, want 0", e.px.pushCount())
	}

	// Positive control: resume. The SAME job (never re-enqueued, never manually requeued) dispatches
	// normally on the very next attempt — resume is a no-op recovery (§1.4), nothing to reconcile.
	e.setConfigBool(t, "dispatch_enabled", true)
	_, pushed2, err := e.d.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick after resume: %v", err)
	}
	if pushed2 != 1 {
		t.Fatalf("pushed=%d after resume, want 1 (the same job dispatches)", pushed2)
	}
	stateAfter, _, leaseNullAfter, _, _ := e.jobState(t, job)
	if stateAfter != "sending" || leaseNullAfter {
		t.Fatalf("job state=%s leaseNull=%v after resume, want sending/false", stateAfter, leaseNullAfter)
	}
}

// TestPanicSwitch_TryDispatchAlsoHalts — the Phase-3 legacy claim path (TryDispatch) honors the
// same switch, not just Tick — a second live claim path bypassing the gate would be a real hole.
func TestPanicSwitch_TryDispatchAlsoHalts(t *testing.T) {
	e := newDispatchEnv(t, "panicswitch2")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	job := e.seedQueuedJob(t, "ps2@example.com")

	e.setConfigBool(t, "dispatch_enabled", false)
	pushedID, err := e.d.TryDispatch(ctx)
	if err != nil {
		t.Fatalf("TryDispatch while paused: %v", err)
	}
	if pushedID != "" {
		t.Fatalf("TryDispatch pushed %q while paused, want \"\" (no-op)", pushedID)
	}
	state, _, leaseNull, _, _ := e.jobState(t, job)
	if state != "queued" || !leaseNull {
		t.Fatalf("job state=%s leaseNull=%v while paused, want queued/true", state, leaseNull)
	}

	// Positive control.
	e.setConfigBool(t, "dispatch_enabled", true)
	pushedID2, err := e.d.TryDispatch(ctx)
	if err != nil {
		t.Fatalf("TryDispatch after resume: %v", err)
	}
	if pushedID2 != job {
		t.Fatalf("TryDispatch after resume pushed %q, want %q", pushedID2, job)
	}
}

// TestPanicSwitch_ReclaimStillRuns (O1, P9.5-OD-1 RULED) — while dispatch is paused, the reclaim
// loop still reclaims a lease-expired in-flight job back to queued: a paused dispatcher must not
// strand in-flight work. This is the exact property the spec's §1.2 prose and O1 test require, and
// the resolution of the Chunk-1 instruction/spec contradiction (P9.5-OD-1).
func TestPanicSwitch_ReclaimStillRuns(t *testing.T) {
	e := newDispatchEnv(t, "panicswitch3")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	job := e.seedQueuedJob(t, "ps3@example.com")

	// Dispatch it for real (dispatch_enabled=true), then force its lease into the past to simulate
	// an expired lease while a worker holds it — the exact condition Detector B reclaims.
	if _, pushed, err := e.d.Tick(ctx); err != nil || pushed != 1 {
		t.Fatalf("initial dispatch: pushed=%d err=%v, want 1/nil", pushed, err)
	}
	if _, err := e.admin.Exec(ctx,
		`UPDATE send_managed.jobs SET lease_expires_at = NOW() - INTERVAL '1 hour' WHERE id=$1`, job); err != nil {
		t.Fatalf("force-expire lease: %v", err)
	}

	// Now pause dispatch AND run the reclaim tick — it must still reclaim the expired job.
	e.setConfigBool(t, "dispatch_enabled", false)
	_, reclaimed, err := e.d.ReclaimTick(ctx)
	if err != nil {
		t.Fatalf("ReclaimTick while dispatch paused: %v", err)
	}
	if reclaimed < 1 {
		t.Fatalf("reclaimed=%d while dispatch paused, want >=1 — the reclaim loop must keep running (§1.2, P9.5-OD-1)", reclaimed)
	}
	state, _, leaseNull, _, _ := e.jobState(t, job)
	if state != "queued" || !leaseNull {
		t.Fatalf("job state=%s leaseNull=%v after reclaim-while-paused, want queued/true", state, leaseNull)
	}
	_ = acct
}

// TestPanicSwitch_DefaultIsTrue — the seeded default preserves current (pre-9.5) behavior: a fresh
// LoadConfig with no operator write sees dispatch_enabled=true.
func TestPanicSwitch_DefaultIsTrue(t *testing.T) {
	e := newDispatchEnv(t, "panicswitchdefault")
	cfg, err := LoadConfig(context.Background(), e.pool)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.DispatchEnabled {
		t.Fatal("seeded default dispatch_enabled=false, want true (must preserve current behavior)")
	}
}
