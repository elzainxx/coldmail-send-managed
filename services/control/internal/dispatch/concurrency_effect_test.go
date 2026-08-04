package dispatch

import (
	"context"
	"sync"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestConcurrentOutcomes_QuarantineSurvivesThrottle (R4 HIGH regression) — two concurrent outcome
// transactions on the SAME account, via genuinely separate connections, must not lose the
// quarantine: an auth_failure that quarantines the account must NOT be silently overwritten back to
// quarantined=FALSE by a concurrent throttle outcome that read the pre-quarantine snapshot. This is
// the exact lost-update the FOR UPDATE row lock in readAccountEffectState prevents (the LOCKED
// "quarantine is manual-recovery-only" invariant, §4.2). Without the lock this test flakes to
// quarantined=FALSE; with it, the two transactions serialize on the account row and quarantine holds.
func TestConcurrentOutcomes_QuarantineSurvivesThrottle(t *testing.T) {
	e := newDispatchEnv(t, "concurfx")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	// Give a recent success so the score stays high — we want the quarantine to come ONLY from the
	// auth_failure trigger (§4.2a), not from a score dip, so the test isolates the flag race.
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET last_success_at=NOW() WHERE id=$1`, acct); err != nil {
		t.Fatalf("seed success: %v", err)
	}

	// Two jobs both assigned to the SAME account, both in-flight (sending) — the real condition
	// where two worker reports for one account arrive concurrently (per-user lanes allow it).
	jobAuth := e.seedQueuedJob(t, "auth@example.com")
	jobThr := e.seedQueuedJob(t, "thr@example.com")
	for _, j := range []string{jobAuth, jobThr} {
		// A state UPDATE fires the job_events trigger, which RAISES unless the actor GUC is set —
		// so drive the transition inside a tx with set_config, exactly as a real dispatcher push does.
		tx, err := e.admin.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('send_managed.actor','dispatcher',true)`); err != nil {
			t.Fatalf("set actor: %v", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE send_managed.jobs SET state='assigned', assigned_account_id=$2, assigned_box_id=$3,
			   lease_expires_at=NOW()+INTERVAL '120 seconds' WHERE id=$1`, j, acct, e.px.workerID); err != nil {
			t.Fatalf("assign: %v", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE send_managed.jobs SET state='sending' WHERE id=$1`, j); err != nil {
			t.Fatalf("sending: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	// Two OutcomeHandlers on two INDEPENDENT pools (genuine concurrency — not one shared conn).
	p1, err := pgxpool.New(ctx, e.moduleDSN)
	if err != nil {
		t.Fatalf("pool1: %v", err)
	}
	defer p1.Close()
	p2, err := pgxpool.New(ctx, e.moduleDSN)
	if err != nil {
		t.Fatalf("pool2: %v", err)
	}
	defer p2.Close()
	oh1 := NewOutcomeHandler(p1, e.cfg)
	oh2 := NewOutcomeHandler(p2, e.cfg)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		oh1.Handle(e.px.workerID, workerproto.Report{JobID: jobAuth, Outcome: workerproto.OutcomeError, SMTPResponse: "535 5.7.8 auth failed"})
	}()
	go func() {
		defer wg.Done()
		oh2.Handle(e.px.workerID, workerproto.Report{JobID: jobThr, Outcome: workerproto.OutcomeError, SMTPResponse: "421 4.7.0 too many, rate limited"})
	}()
	wg.Wait()

	// The auth_failure quarantine MUST have survived the concurrent throttle outcome.
	var quarantined bool
	if err := e.admin.QueryRow(ctx, `SELECT quarantined FROM send_managed.accounts WHERE id=$1`, acct).Scan(&quarantined); err != nil {
		t.Fatalf("read quarantined: %v", err)
	}
	if !quarantined {
		t.Fatalf("quarantine was LOST under a concurrent throttle outcome — the FOR UPDATE row lock failed to serialize (violates manual-recovery-only §4.2)")
	}

	// Exactly one system quarantine event exists (idempotent; the race did not produce two or zero).
	var quarEvents int
	if err := e.admin.QueryRow(ctx,
		`SELECT count(*) FROM send_managed.account_events WHERE account_id=$1 AND actor='system' AND action='quarantine'`, acct).Scan(&quarEvents); err != nil {
		t.Fatalf("count quar events: %v", err)
	}
	if quarEvents != 1 {
		t.Fatalf("system quarantine events=%d, want exactly 1", quarEvents)
	}
}
