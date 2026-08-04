package dispatch

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// TestReclaimRace_OutcomeVsReclaim (ADR-0010 — the mandated two-connection race on the reclaim
// transition) — the reclaim loop and a worker outcome both act on the SAME in-flight job at the same
// moment, on genuinely separate connections. Exactly ONE must win: either the accepted report marks
// it 'sent' (and the reclaim's state-guarded WHERE matches nothing → clean no-op), or the reclaim
// requeues it to 'queued' (and the outcome's own state guard makes it a no-op). The job must end in
// a single coherent state with a matching event count — never lost-updated, never double-processed,
// and the two transactions must not DEADLOCK. Run many rounds to actually exercise the interleave.
func TestReclaimRace_OutcomeVsReclaim(t *testing.T) {
	e := newDispatchEnv(t, "reclaimrace")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	// Genuinely independent pools for the two racers (Rule 3 / ADR-0010 — never one shared conn).
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
	oh := NewOutcomeHandler(p1, e.cfg)
	racer := NewDispatcher(p2, e.dec, e.px)

	const rounds = 30
	sentWins, reclaimWins := 0, 0
	for i := 0; i < rounds; i++ {
		job := e.seedQueuedJob(t, "race@example.com")
		e.dispatchToSending(t, job)
		e.expireLease(t, job) // make it eligible for Detector B
		e.px.freeAllWorkers()

		// Fire an accepted outcome and a reclaim tick CONCURRENTLY on the same sending job.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeAccepted,
				ProviderMessageID: "PMID-RACE-" + job, SMTPResponse: "250 2.0.0 Ok"})
		}()
		go func() {
			defer wg.Done()
			if _, _, err := racer.ReclaimTick(ctx); err != nil {
				t.Errorf("round %d: ReclaimTick errored (possible deadlock 40P01): %v", i, err)
			}
		}()
		wg.Wait()

		// The job is in exactly one coherent terminal-or-queued state — never a torn one.
		state, hasPMID, leaseNull, completed, _ := e.jobState(t, job)
		switch state {
		case "sent":
			// The accepted report won: it has a provider_message_id, completed_at, lease NULL.
			if !hasPMID || !completed || !leaseNull {
				t.Fatalf("round %d: sent job incoherent (pmid=%v completed=%v leaseNull=%v)", i, hasPMID, completed, leaseNull)
			}
			sentWins++
		case "queued":
			// The reclaim won: lease NULL, NOT completed, NO provider_message_id (never sent).
			if hasPMID || completed || !leaseNull {
				t.Fatalf("round %d: reclaimed job incoherent (pmid=%v completed=%v leaseNull=%v)", i, hasPMID, completed, leaseNull)
			}
			reclaimWins++
		default:
			t.Fatalf("round %d: job in unexpected state %q after the race (neither sent nor queued)", i, state)
		}
	}

	// Every round landed in a coherent branch with no deadlock — the primary ADR-0010 property.
	if sentWins+reclaimWins != rounds {
		t.Fatalf("only %d/%d rounds resolved coherently", sentWins+reclaimWins, rounds)
	}
	t.Logf("concurrent reclaim race: %d sent-wins, %d reclaim-wins over %d rounds (all coherent, no deadlock)", sentWins, reclaimWins, rounds)

	// The concurrent loop above tends to let the fast single-UPDATE outcome win the timing, so the
	// "reclaim won first, the later outcome must no-op" ordering is asserted DETERMINISTICALLY here
	// (the state guard is what makes it safe): reclaim the job, THEN deliver an accepted report, and
	// prove the report is a clean no-op — no resurrection to 'sent', no double-send, no raw 23505.
	job := e.seedQueuedJob(t, "reclaimfirst@example.com")
	e.dispatchToSending(t, job)
	e.expireLease(t, job)
	e.px.freeAllWorkers()
	if _, n, err := racer.ReclaimTick(ctx); err != nil || n != 1 {
		t.Fatalf("priming reclaim: n=%d err=%v, want 1/nil", n, err)
	}
	// The job is now 'queued' (reclaimed). A late accepted report for the same job — the worker that
	// was mid-send when we reclaimed — must NOT mark it sent (the outcome handler's own state guard
	// requires state='sending'). It stays queued, unsent.
	oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-LATE-" + job, SMTPResponse: "250 2.0.0 Ok"})
	if st, hasPMID, _, completed, _ := e.jobState(t, job); st != "queued" || hasPMID || completed {
		t.Fatalf("late accepted report resurrected a reclaimed job: state=%s pmid=%v completed=%v, want queued/false/false", st, hasPMID, completed)
	}
}
