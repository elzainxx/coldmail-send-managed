package dispatch

import (
	"context"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestPersistAck_ResolvedOnlyAfterCommit (Phase 7 §1.3, P7-4 — the persist-BEFORE-ack contract,
// Control side) — Handle returns resolved=true ONLY when the outcome is durably committed (or a clean
// idempotent no-op the Control owns), and resolved=false when it cannot determine/commit. The stream
// server keys report_ack on exactly this bool, so this proves the worker is told to drop its buffered
// outcome only after a real DB commit.
func TestPersistAck_ResolvedOnlyAfterCommit(t *testing.T) {
	e := newDispatchEnv(t, "persistack")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	// (1) Accepted report on a real in-flight job → committed 'sent' → resolved=true.
	job := e.seedQueuedJob(t, "ack@example.com")
	e.dispatchToSending(t, job)
	if resolved := e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-ACK-1", SMTPResponse: "250 2.0.0 Ok"}); !resolved {
		t.Fatal("accepted report: resolved=false, want true (it committed sent — worker may drop the buffer)")
	}
	if st, hasPMID, _, _, _ := e.jobState(t, job); st != "sent" || !hasPMID {
		t.Fatalf("job state=%s hasPMID=%v, want sent/true (the commit that justified the ack)", st, hasPMID)
	}

	// (2) A duplicate/replay of the SAME accepted report → clean idempotent no-op → resolved=true
	//     (the Control owns it; a replay must still be ackable so the worker stops replaying). No 2nd row.
	before := e.sendLogCount(t, job)
	if resolved := e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-ACK-1", SMTPResponse: "250 2.0.0 Ok"}); !resolved {
		t.Fatal("replayed accepted report: resolved=false, want true (idempotent no-op is ackable)")
	}
	if after := e.sendLogCount(t, job); after != before {
		t.Fatalf("replay wrote %d extra send_logs row(s), want 0 (idempotent)", after-before)
	}

	// (3) A report for a job that already left 'sending' (reclaimed to queued) → clean no-op →
	//     resolved=true (the Control resolved it another way; the worker may drop it).
	e.px.freeAllWorkers() // case 1's worker is still marked in-flight on the recording pusher
	other := e.seedQueuedJob(t, "ack2@example.com")
	e.dispatchToSending(t, other)
	e.expireLease(t, other)
	e.px.freeAllWorkers()
	if _, n, err := e.d.ReclaimTick(ctx); err != nil || n != 1 {
		t.Fatalf("prime reclaim: n=%d err=%v, want 1/nil", n, err)
	}
	if resolved := e.oh.Handle(e.px.workerID, workerproto.Report{JobID: other, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-ACK-LATE", SMTPResponse: "250 2.0.0 Ok"}); !resolved {
		t.Fatal("report for a reclaimed job: resolved=false, want true (Control owns it — worker may drop)")
	}
	// It stayed queued (the late accepted did NOT resurrect it — Rule 5, no invented outcome).
	if st, hasPMID, _, _, _ := e.jobState(t, other); st != "queued" || hasPMID {
		t.Fatalf("reclaimed job state=%s hasPMID=%v, want queued/false (no resurrection)", st, hasPMID)
	}

	// (4) NEGATIVE control — a DB failure means Handle cannot determine the job state, so it must
	//     return resolved=false (NO ack → the worker retains + replays). We simulate an unreachable DB by
	//     pointing a handler at a closed pool. This is the DB-outage path (§2) proven at unit level:
	//     the Control never acks an outcome it could not persist (Rule 5).
	deadPool, err := pgxpool.New(ctx, e.moduleDSN)
	if err != nil {
		t.Fatalf("dead pool: %v", err)
	}
	deadPool.Close() // every query on it now fails — a stand-in for "DB unreachable"
	deadHandler := NewOutcomeHandler(deadPool, e.cfg)
	if resolved := deadHandler.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-ACK-DEAD", SMTPResponse: "250 2.0.0 Ok"}); resolved {
		t.Fatal("report under a dead DB: resolved=true, want false (NO ack — worker must retain + replay; never ack an unpersisted outcome)")
	}
}
