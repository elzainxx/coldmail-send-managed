package dispatch

import (
	"context"
	"testing"
)

// jobFailedState reads a job's state + whether completed_at is set (for the fail assertions).
func (e *dispatchEnv) jobStateStr(t *testing.T, jobID string) string {
	t.Helper()
	var s string
	if err := e.admin.QueryRow(context.Background(), `SELECT state::text FROM send_managed.jobs WHERE id=$1`, jobID).Scan(&s); err != nil {
		t.Fatalf("read state: %v", err)
	}
	return s
}

// TestExhaustion_TerminalBlockedFails (T6) — a job whose whole resolved chain is non-empty and
// every account is terminal_blocked (quarantined/disabled/manual-paused) goes queued→failed on the
// tick, with completed_at set and exactly one job_events →failed row (actor='dispatcher'). ONLY
// then: the paired self-recovering case (below) proves it does NOT fail while capacity may return.
func TestExhaustion_TerminalBlockedFails(t *testing.T) {
	e := newDispatchEnv(t, "exhaustfail")
	ctx := context.Background()
	pool := e.seedPool(t)
	// Two accounts, both terminal_blocked: one quarantined, one manual-paused. Non-empty chain,
	// zero eligible, zero self-recovering → FAIL.
	q := e.seedAccount(t, pool, "sec")
	m := e.seedAccount(t, pool, "sec2")
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET quarantined=TRUE WHERE id=$1`, q); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET paused=TRUE, pause_reason='manual' WHERE id=$1`, m); err != nil {
		t.Fatalf("manual pause: %v", err)
	}
	job := e.seedQueuedJob(t, "x@example.com")

	// One tick: the gate resolves the chain, finds it all terminal_blocked, and fails the job.
	if _, _, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if s := e.jobStateStr(t, job); s != "failed" {
		t.Fatalf("job state=%s, want failed (all accounts terminal_blocked §5.1)", s)
	}
	// completed_at set (terminal CHECK), lease null, exactly one →failed event by dispatcher.
	var completed, leaseNull bool
	if err := e.admin.QueryRow(ctx, `SELECT completed_at IS NOT NULL, lease_expires_at IS NULL FROM send_managed.jobs WHERE id=$1`, job).Scan(&completed, &leaseNull); err != nil {
		t.Fatalf("terminal cols: %v", err)
	}
	if !completed || !leaseNull {
		t.Fatalf("failed job completed=%v leaseNull=%v, want true/true", completed, leaseNull)
	}
	var actor string
	var nEvents int
	if err := e.admin.QueryRow(ctx,
		`SELECT count(*), COALESCE(max(actor::text),'') FROM send_managed.job_events WHERE job_id=$1 AND to_state='failed'`, job).Scan(&nEvents, &actor); err != nil {
		t.Fatalf("fail event: %v", err)
	}
	if nEvents != 1 || actor != "dispatcher" {
		t.Fatalf("→failed events=%d actor=%q, want 1/dispatcher", nEvents, actor)
	}
	// The transition was queued→failed (never sending→failed — illegal).
	var fromState string
	if err := e.admin.QueryRow(ctx,
		`SELECT from_state::text FROM send_managed.job_events WHERE job_id=$1 AND to_state='failed'`, job).Scan(&fromState); err != nil {
		t.Fatalf("from_state: %v", err)
	}
	if fromState != "queued" {
		t.Fatalf("→failed from_state=%q, want queued (sending→failed is illegal §2.2)", fromState)
	}
}

// TestExhaustion_SelfRecoveringWaits (T6) — a job whose only accounts are self-recovering
// (daily_limit-paused / at-limit / in-window throttle) STAYS queued across several ticks, with NO
// job_events beyond genesis (no fail, no churn) — capacity may return, so it waits, never fails.
func TestExhaustion_SelfRecoveringWaits(t *testing.T) {
	e := newDispatchEnv(t, "exhaustwait")
	ctx := context.Background()
	pool := e.seedPool(t)
	// daily_limit-paused (clears at reset) + in-window throttle (clears at timer) — both
	// self-recovering, neither eligible now.
	dl := e.seedAccount(t, pool, "sec")
	th := e.seedAccount(t, pool, "sec2")
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET paused=TRUE, pause_reason='daily_limit' WHERE id=$1`, dl); err != nil {
		t.Fatalf("daily_limit pause: %v", err)
	}
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET paused=TRUE, pause_reason='throttle', throttle_paused_at=NOW() WHERE id=$1`, th); err != nil {
		t.Fatalf("throttle pause: %v", err)
	}
	job := e.seedQueuedJob(t, "w@example.com")

	// Several ticks: the job must stay queued, untouched, with no fail and no churn.
	for i := 0; i < 4; i++ {
		if _, _, err := e.d.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if s := e.jobStateStr(t, job); s != "queued" {
		t.Fatalf("job state=%s after ticks, want queued (self-recovering → WAIT §5.1)", s)
	}
	// No churn: exactly the genesis event, nothing else (no claim/requeue/fail).
	if n := e.eventCount(t, job); n != 1 {
		t.Fatalf("job_events=%d, want 1 (genesis only — no churn while waiting §5.3)", n)
	}
}

// TestExhaustion_EmptyChainWaits (T6, P5-4b) — a base user whose default_shared_pool_id is UNSET
// has an EMPTY resolved chain; its job WAITS (stays queued, no fail), never failing over a config
// gap. Then setting the pool lets it dispatch — proving the empty-chain branch is WAIT, not FAIL.
func TestExhaustion_EmptyChainWaits(t *testing.T) {
	e := newDispatchEnv(t, "exhaustempty")
	ctx := context.Background()
	// Seed a healthy shared pool + account but do NOT set it as default_shared_pool_id, so a base
	// user's chain resolves to empty (§4.3). seedPool sets the default; use the lower-level seeders.
	pool := e.seedSharedPool(t, "empties")
	e.seedAccount(t, pool, "sec")
	e.clearDefaultSharedPool(t) // ensure unset

	job := e.seedQueuedJob(t, "e@example.com")
	for i := 0; i < 3; i++ {
		if _, _, err := e.d.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if s := e.jobStateStr(t, job); s != "queued" {
		t.Fatalf("job state=%s, want queued (empty/unconfigured chain → WAIT, never fail §5.1 P5-4b)", s)
	}
	if n := e.eventCount(t, job); n != 1 {
		t.Fatalf("job_events=%d on empty-chain wait, want 1 (genesis only, no churn)", n)
	}

	// Now configure the default shared pool → the SAME job dispatches (never re-enqueued).
	e.setDefaultSharedPool(t, pool)
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("dispatch after config: %v", err)
	}
	if s := e.jobStateStr(t, job); s != "sending" && s != "sent" {
		t.Fatalf("job state=%s after setting default pool, want sending/sent (recovered via config)", s)
	}
}

// TestExhaustion_EligiblePresentClaimsNotFails (T6 anti-vacuity) — with an eligible account
// present alongside terminal_blocked ones, the job CLAIMS (dispatches), never fails. Proves FAIL
// fires ONLY on true exhaustion, not whenever some accounts are blocked.
func TestExhaustion_EligiblePresentClaimsNotFails(t *testing.T) {
	e := newDispatchEnv(t, "exhausteligible")
	ctx := context.Background()
	pool := e.seedPool(t)
	// One quarantined (terminal_blocked) + one healthy (eligible). Chain non-empty, ≥1 eligible
	// → CLAIM.
	bad := e.seedAccount(t, pool, "sec")
	e.seedAccount(t, pool, "sec2")
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET quarantined=TRUE WHERE id=$1`, bad); err != nil {
		t.Fatalf("quarantine: %v", err)
	}
	job := e.seedQueuedJob(t, "c@example.com")
	if _, _, err := e.d.Tick(ctx); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if s := e.jobStateStr(t, job); s == "failed" {
		t.Fatalf("job failed despite an eligible account present — FAIL must fire ONLY on true exhaustion (§5.1)")
	}
	if s := e.jobStateStr(t, job); s != "sending" {
		t.Fatalf("job state=%s, want sending (claimed via the eligible account)", s)
	}
}
