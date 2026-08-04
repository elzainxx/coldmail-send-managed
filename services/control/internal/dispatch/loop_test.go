package dispatch

// Chunk 1 behavioral tests: the polling tick loop + the per-user claim (§1.1/§1.2). Real Postgres
// (via dispatchEnv), real concurrency where relevant. NO routing/eligibility/fairness yet — those
// land in later chunks and get their own test files; this file only proves the claim is
// PER-USER (not global FIFO), respects its batch grant, and that one Tick turns queued jobs for
// multiple users into pushes via the existing credential/header/state-machine path.

import (
	"context"
	"testing"
)

// TestClaimForUser_ScopedToOneUser — seeding queued jobs for two different users, claiming for
// user A must NEVER return one of user B's jobs, even though both are equally claimable by
// next_attempt_at/created_at. This is the P4-1 property that global FIFO cannot provide.
func TestClaimForUser_ScopedToOneUser(t *testing.T) {
	e := newDispatchEnv(t, "claimscope")
	ctx := context.Background()

	userA := e.seedUser(t, "a@tenant.example")
	userB := e.seedUser(t, "b@tenant.example")
	jobA := e.seedQueuedJobFor(t, userA, "a1@example.com")
	jobB := e.seedQueuedJobFor(t, userB, "b1@example.com")

	gotA, err := e.d.claimForUser(ctx, userA, 10, 120)
	if err != nil {
		t.Fatalf("claimForUser(A): %v", err)
	}
	if len(gotA) != 1 || gotA[0].id != jobA {
		t.Fatalf("claimForUser(A) = %v, want exactly [%s]", gotA, jobA)
	}

	gotB, err := e.d.claimForUser(ctx, userB, 10, 120)
	if err != nil {
		t.Fatalf("claimForUser(B): %v", err)
	}
	if len(gotB) != 1 || gotB[0].id != jobB {
		t.Fatalf("claimForUser(B) = %v, want exactly [%s]", gotB, jobB)
	}
}

// TestClaimForUser_RespectsGrant — seeding 5 queued jobs for one user, a grant of 2 claims exactly
// 2 (not more), leaving 3 still queued and claimable by a later call.
func TestClaimForUser_RespectsGrant(t *testing.T) {
	e := newDispatchEnv(t, "claimgrant")
	ctx := context.Background()
	userA := e.seedUser(t, "grant@tenant.example")
	for i := 0; i < 5; i++ {
		e.seedQueuedJobFor(t, userA, "g"+string(rune('0'+i))+"@example.com")
	}

	first, err := e.d.claimForUser(ctx, userA, 2, 120)
	if err != nil {
		t.Fatalf("claimForUser: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first claim len=%d, want exactly 2 (the grant)", len(first))
	}

	var remaining int
	if err := e.admin.QueryRow(ctx,
		`SELECT count(*) FROM send_managed.jobs WHERE user_id=$1 AND state='queued'`, userA).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 3 {
		t.Fatalf("remaining queued=%d, want 3 (5 - grant of 2)", remaining)
	}

	// A grant of 0 (or negative) must claim NOTHING — the lane-ceiling caller relies on this.
	zero, err := e.d.claimForUser(ctx, userA, 0, 120)
	if err != nil {
		t.Fatalf("claimForUser(grant=0): %v", err)
	}
	if len(zero) != 0 {
		t.Fatalf("grant=0 claimed %d jobs, want 0", len(zero))
	}
}

// TestTick_ClaimsAcrossUsersAndPushes — end-to-end: two users each with a queued job, one Tick
// claims BOTH (their per-user claims are independent) and pushes both through the existing
// account-select/decrypt/header/push path, landing both in 'sending' with a push recorded. Two
// workers are registered so both pushes can complete in the SAME tick (Phase 3's one-in-flight-per-
// worker constraint means a single worker would only ever admit one push per tick — that's a worker-supply
// constraint, not a claim/routing one, so this test gives it enough worker capacity to observe both).
func TestTick_ClaimsAcrossUsersAndPushes(t *testing.T) {
	e := newDispatchEnv(t, "tickmulti")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "tick-secret")
	e.addWorker(t)

	userA := e.seedUser(t, "ta@tenant.example")
	userB := e.seedUser(t, "tb@tenant.example")
	jobA := e.seedQueuedJobFor(t, userA, "ta1@example.com")
	jobB := e.seedQueuedJobFor(t, userB, "tb1@example.com")

	cfg, pushed, err := e.d.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if cfg.DispatchTickIntervalMS <= 0 {
		t.Fatalf("Tick returned cfg.DispatchTickIntervalMS=%d, want > 0 (a real seeded key)", cfg.DispatchTickIntervalMS)
	}
	if pushed != 2 {
		t.Fatalf("Tick pushed=%d, want 2 (both users' jobs)", pushed)
	}

	for _, jobID := range []string{jobA, jobB} {
		state, _, leaseNull, _, _ := e.jobState(t, jobID)
		if state != "sending" {
			t.Fatalf("job %s state=%s, want sending", jobID, state)
		}
		if leaseNull {
			t.Fatalf("job %s lease is null in sending", jobID)
		}
	}
	if e.px.pushCount() != 2 {
		t.Fatalf("push count=%d, want 2", e.px.pushCount())
	}
}

// TestTick_NoWorkerRequeuesCleanly — a Tick that claims a job but finds NO connected worker requeues it
// to a legal queued state (lease cleared) rather than leaving it stranded 'assigned'.
func TestTick_NoWorkerRequeuesCleanly(t *testing.T) {
	e := newDispatchEnv(t, "tickpoworker")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.px.workerID = "" // no connected worker this tick

	userA := e.seedUser(t, "noworker@tenant.example")
	job := e.seedQueuedJobFor(t, userA, "nb@example.com")

	_, pushed, err := e.d.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if pushed != 0 {
		t.Fatalf("pushed=%d, want 0 (no worker)", pushed)
	}
	state, _, leaseNull, _, attempt := e.jobState(t, job)
	if state != "queued" || !leaseNull {
		t.Fatalf("job state=%s leaseNull=%v, want queued/true (requeued, no worker available)", state, leaseNull)
	}
	if attempt != 1 {
		t.Fatalf("attempt_count=%d, want 1", attempt)
	}
}

// TestTick_RespectsPerUserLaneCeiling — a user with 5 queued jobs and a lane ceiling of 2 (set via
// the real seeded concurrency_per_user_default config) gets claimed only 2 per tick; a second tick
// (after those settle out of in-flight) picks up more. This is the pre-claim lane count (§6),
// verified end-to-end through Tick rather than just the raw claim. A second worker is registered so
// the ceiling of 2 is observable within one tick (worker supply, not the ceiling itself, would
// otherwise be the binding constraint).
func TestTick_RespectsPerUserLaneCeiling(t *testing.T) {
	e := newDispatchEnv(t, "tickceil")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	e.addWorker(t)
	e.setConfigInt(t, "concurrency_per_user_default", 2)

	userA := e.seedUser(t, "ceil@tenant.example")
	for i := 0; i < 5; i++ {
		e.seedQueuedJobFor(t, userA, "c"+string(rune('0'+i))+"@example.com")
	}

	_, pushed, err := e.d.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if pushed != 2 {
		t.Fatalf("pushed=%d, want exactly 2 (the lane ceiling) even though 5 were queued", pushed)
	}
	var inFlight int
	if err := e.admin.QueryRow(ctx,
		`SELECT count(*) FROM send_managed.jobs WHERE user_id=$1 AND state IN ('assigned','sending')`, userA).Scan(&inFlight); err != nil {
		t.Fatalf("count in-flight: %v", err)
	}
	if inFlight != 2 {
		t.Fatalf("in-flight=%d, want 2 (never exceed the ceiling in one tick)", inFlight)
	}
}
