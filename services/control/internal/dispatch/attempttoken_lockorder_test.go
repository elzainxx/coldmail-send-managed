package dispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// attempttoken_lockorder_test.go — Phase 8 CHUNK 1: the per-attempt identity under GENUINE
// CONCURRENCY, with the interleave FORCED rather than raced (CLAUDE.md Rule 10: a two-connection test
// that never contends cannot fail and is worthless).
//
// Two distinct properties are proven here, each on its own real connections against real Postgres:
//
//	(1) TestAttemptToken_StaleReplayRacingLiveDispatch — the TOCTOU window. Handle's token pre-check is
//	    a READ, so a reclaim + re-dispatch can bump the token between that read and the apply tx. The
//	    apply paths therefore re-assert the token INSIDE their own guarded UPDATE. This test holds the
//	    job row locked by an in-progress re-dispatch, drives the stale replay into it so it genuinely
//	    BLOCKS (asserted via pg_stat_activity), then releases — and the stale replay must still mutate
//	    nothing even though its pre-check ran when its token was still current.
//	(2) TestAttemptToken_MarkSendingVsAccountLockNoDeadlock — LOCK ORDER (Rule 9 / ADR-0010).
//	    markSending now writes one more column but still takes NO exclusive account lock: it takes the
//	    job row, then the account's implicit FK KEY SHARE (its `state` change fires the job_events
//	    trigger). Driven against a HELD exclusive account FOR UPDATE — the first step of
//	    applyClassifiedFailure/reclaimJobLocked — in both directions, proving no 40P01 and that the
//	    ordering claim in markSending's doc comment is asserted rather than asserted-in-a-comment.

// adminExecAsActor runs a setup statement on the admin connection inside a tx that sets
// `send_managed.actor`. Required for any UPDATE that changes send_managed.jobs.state: the send_managed_0008
// job_events trigger REJECTS a state transition when the GUC is unset (P0001), by design.
func (e *dispatchEnv) adminExecAsActor(t *testing.T, actor, sql string, args ...any) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin admin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT set_config('send_managed.actor',$1,true)`, actor); err != nil {
		t.Fatalf("set actor: %v", err)
	}
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("admin exec as %s: %v", actor, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit admin tx: %v", err)
	}
}

// TestAttemptToken_StaleReplayRacingLiveDispatch — the deterministic interleave for the TOCTOU window
// the token guard has to survive.
//
// Sequence, forced with a held transaction (no timing luck):
//
//	attempt 1 is dispatched (token T1) and produces an unacked outcome
//	  → connection A opens a tx, bumps the job's attempt_token to T2, and does NOT commit — it now
//	    holds the job row's write lock, and the row the rest of the world still sees is (sending, T1)
//	  → connection B replays attempt 1's outcome; its pre-check READ sees that pre-commit row, so the
//	    token MATCHES and the guard lets it through to its apply UPDATE, which BLOCKS on A's held row
//	  → A commits (the row is now (sending, T2))
//	  → B's UPDATE re-evaluates its predicate against the committed row and matches ZERO rows.
//
// This is the exact shape the in-UPDATE token predicate exists for: B's pre-check was TRUTHFUL when it
// ran and stale by the time it applied, so only the predicate carried INSIDE the apply statement can
// stop it. Without that predicate B's UPDATE would find state='sending' and apply attempt 1's outcome
// to attempt 2's live row.
//
// WHY A's held statement is a bare token bump rather than the literal reclaim + re-dispatch pair: those
// are separate committed transactions whose intermediate committed states are 'queued'/'assigned', and
// Handle's state guard rejects a report against a non-'sending' row BEFORE it ever reaches an apply
// UPDATE — so B would never block and the test would be vacuous (it was, on the first draft). Holding
// the row at 'sending' across the bump is what forces the decision to be made by the TOKEN alone, which
// is the property under test. It also keeps the assertions exact: an UPDATE that leaves `state`
// unchanged fires no job_events row (CLAUDE.md Rule 9 (a)), so every event/log delta below is B's.
//
// NEGATIVE CONTROL, run by hand before this test was committed (Rule 10 — a race test that cannot fail
// is worthless): replacing applyAccepted's token predicate with a tautology and re-running this test
// FAILS with "job state=sent, want sending — the stale replay resolved attempt 2's live row". So the
// pass below is the predicate doing work, not the interleave failing to occur.
func TestAttemptToken_StaleReplayRacingLiveDispatch(t *testing.T) {
	e := newDispatchEnv(t, "attoktrace")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET last_success_at=NOW() WHERE id=$1`, acct); err != nil {
		t.Fatalf("seed success: %v", err)
	}

	job := e.seedQueuedJob(t, "race@example.com")
	e.dispatchToSending(t, job)
	worker1 := e.jobWorker(t, job)
	token1 := e.jobAttemptToken(t, job)
	if token1 == 0 {
		t.Fatal("precondition: the dispatched attempt has no token — the race below would be vacuous")
	}

	// Attempt 1's outcome, produced but never acked — replayed below, after the token has moved.
	stale := workerproto.Report{
		Type: workerproto.TypeReport, JobID: job, AttemptToken: token1,
		Outcome: workerproto.OutcomeAccepted, ProviderMessageID: "race-stale-pmid",
		SMTPResponse: "250 2.0.0 stale OK",
	}

	pDispatch, err := pgxpool.New(ctx, e.moduleDSN)
	if err != nil {
		t.Fatalf("pool dispatch: %v", err)
	}
	defer pDispatch.Close()
	pOutcome, err := pgxpool.New(ctx, e.moduleDSN)
	if err != nil {
		t.Fatalf("pool outcome: %v", err)
	}
	defer pOutcome.Close()

	// (1) Connection A: the re-dispatch's token mint, inside a HELD tx. It owns the job row's write lock
	// until we commit, while every other session still reads (sending, T1).
	tx, err := pDispatch.Begin(ctx)
	if err != nil {
		t.Fatalf("begin dispatch tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('send_managed.actor', 'dispatcher', true)"); err != nil {
		t.Fatalf("set actor: %v", err)
	}
	var token2 int64
	if err := tx.QueryRow(ctx,
		`UPDATE send_managed.jobs SET attempt_token = attempt_token + 1, updated_at=NOW()
		  WHERE id=$1 AND state='sending'
		  RETURNING attempt_token`, job).Scan(&token2); err != nil {
		t.Fatalf("re-dispatch token mint: %v", err)
	}
	if token2 == token1 {
		t.Fatalf("the re-dispatch did not mint a new token (still %d) — vacuous", token1)
	}

	// Snapshot what the stale replay must not touch (read as admin — the uncommitted row is invisible).
	logsBefore := e.sendLogCount(t, job)
	eventsBefore := e.eventCount(t, job)
	// Closeout Chunk 2 addition: the account must not be scored either. The prior version of this
	// test asserted state/pmid/send_log/event deltas only; account-scoring non-mutation was proven
	// ONLY by the separate, non-racing TestAttemptToken_StaleReplaySameWorkerRejected — never under a
	// genuinely forced interleave. Snapshot here so the FORCED-interleave path proves it too.
	var cfBefore int
	var scoreBefore float64
	if err := e.admin.QueryRow(ctx,
		`SELECT consecutive_failures, account_status_score FROM send_managed.accounts WHERE id=$1`, acct).
		Scan(&cfBefore, &scoreBefore); err != nil {
		t.Fatalf("read account before: %v", err)
	}

	// (2) Connection B: the stale replay. Its pre-check read sees the pre-commit row (token still T1 →
	// a MATCH), so it proceeds into its apply UPDATE and must BLOCK on A's held row lock.
	type res struct{ resolved bool }
	done := make(chan res, 1)
	go func() {
		oh := NewOutcomeHandler(pOutcome, e.cfg)
		done <- res{resolved: oh.Handle(worker1, stale)}
	}()

	// ANTI-VACUITY (Rule 10): prove B genuinely BLOCKED. If it completed here, the interleave never
	// happened and this test proves nothing about the TOCTOU window.
	select {
	case <-done:
		t.Fatal("the stale replay completed while the re-dispatch held the job row — the interleaving did not occur, so this test is vacuous")
	case <-time.After(750 * time.Millisecond):
	}
	var blocked int
	if err := e.admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity
		  WHERE datname = current_database() AND wait_event_type='Lock' AND state='active'
		    AND query ILIKE '%send_managed.jobs%SET state=''sent''%'`).Scan(&blocked); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	if blocked == 0 {
		t.Fatal("no backend is lock-waiting — the stale replay is not contending with the held re-dispatch (vacuous)")
	}

	// (3) Release the re-dispatch. B's UPDATE now re-evaluates against the COMMITTED row (token T2).
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit dispatch tx: %v", err)
	}
	select {
	case r := <-done:
		if !r.resolved {
			t.Error("the stale replay was not acked — the worker would replay a permanently-inapplicable outcome until its TTL")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the stale replay never completed after the re-dispatch committed — the two paths are wedged")
	}

	// THE ASSERTION: attempt 1's outcome was NOT applied to attempt 2's live row, even though its
	// pre-check ran while its token was still current.
	st, hasPMID, _, _, _ := e.jobState(t, job)
	if st != "sending" {
		t.Fatalf("job state=%s, want sending — the stale replay resolved attempt 2's live row", st)
	}
	if hasPMID {
		t.Fatal("the stale replay set provider_message_id — the job is marked sent while attempt 2 is live (a DUPLICATE)")
	}
	if got := e.sendLogCount(t, job); got != logsBefore {
		t.Errorf("the stale replay wrote a send_log (%d→%d)", logsBefore, got)
	}
	if got := e.eventCount(t, job); got != eventsBefore {
		t.Errorf("the stale replay wrote a job_event (%d→%d) — it mutated state", eventsBefore, got)
	}
	if tok := e.jobAttemptToken(t, job); tok != token2 {
		t.Errorf("attempt_token=%d after the race, want %d (the live attempt's)", tok, token2)
	}
	var cfAfter int
	var scoreAfter float64
	if err := e.admin.QueryRow(ctx,
		`SELECT consecutive_failures, account_status_score FROM send_managed.accounts WHERE id=$1`, acct).
		Scan(&cfAfter, &scoreAfter); err != nil {
		t.Fatalf("read account after: %v", err)
	}
	if cfAfter != cfBefore || scoreAfter != scoreBefore {
		t.Errorf("the stale replay SCORED the account under the forced interleave: consecutive_failures %d→%d, score %.3f→%.3f",
			cfBefore, cfAfter, scoreBefore, scoreAfter)
	}

	// POSITIVE CONTROL: the LIVE token, same worker, same job — must apply in full. Proves the block above
	// was the token predicate at work, not a wedged/erroring path that rejects everything.
	if !e.oh.Handle(worker1, workerproto.Report{
		Type: workerproto.TypeReport, JobID: job, AttemptToken: token2,
		Outcome: workerproto.OutcomeAccepted, ProviderMessageID: "race-live-pmid", SMTPResponse: "250 OK",
	}) {
		t.Fatal("positive control: the live attempt's report was not resolved")
	}
	if st, hasPMID, _, _, _ := e.jobState(t, job); st != "sent" || !hasPMID {
		t.Fatalf("positive control: state=%s pmid=%v, want sent/true (vacuous otherwise)", st, hasPMID)
	}
}

// TestAttemptToken_MarkSendingVsAccountLockNoDeadlock — the ADR-0010 obligation for the path Chunk 1
// TOUCHED. markSending gained one column in its SET list; this asserts that changed nothing about its
// locking. Three directions are driven with a forced interleave:
//
//	(a) an exclusive account FOR UPDATE is HELD (applyClassifiedFailure/reclaimJobLocked's first step)
//	    and markSending runs against it. markSending's `state` change DOES fire the job_events trigger,
//	    whose INSERT takes the account's FK KEY SHARE — which CONFLICTS with FOR UPDATE — so markSending
//	    is EXPECTED to block here, and must then complete cleanly (never 40P01) once the lock releases.
//	    That is the account→job order working as designed, and the reason markSending needs no account
//	    pre-lock of its own: it never takes an exclusive account lock, so it can only ever be the
//	    WAITER, never half of a cycle.
//	(b) the JOB row is held by a generic `FOR UPDATE` and the account→job outcome path
//	    (applyClassifiedFailure) is driven against it: it must BLOCK on the job row and then commit,
//	    not cycle. NOTE (R4 review correction): this direction exercises applyClassifiedFailure's
//	    lock order against a job-row holder, NOT markSending — a generic holder is used here because
//	    markSending's own guard (`WHERE state='assigned'`) would match 0 rows against a job already
//	    `sending` (see (c)'s comment for why that made an EARLIER attempt at this direction using
//	    markSending itself impossible). This property is also covered by
//	    TestRescan_ForcedInterleave_JobHeldThenAccountFirstOutcome and
//	    TestReclaimRace_DetectorAVsClassifiedFailure; it is kept here as a targeted regression guard
//	    specifically alongside markSending's own token-minting test (c), not as (c)'s substitute.
//	(c) markSending ITSELF, called for real, driven against a HELD job row — the direction (b) was
//	    originally meant to cover but could not, because holding the row via markSending's own guarded
//	    UPDATE would require the job to already be `assigned` with nothing else touching it. Holding
//	    the row via a plain `SELECT ... FOR UPDATE` on a job still in `assigned` state sidesteps that:
//	    markSending is then called for real against the SAME row, must BLOCK on the plain row lock
//	    (no KEY SHARE/trigger involved — this is the ordinary row-level lock every UPDATE takes on its
//	    own target), and must complete cleanly (mint a token, never 40P01) once released.
func TestAttemptToken_MarkSendingVsAccountLockNoDeadlock(t *testing.T) {
	e := newDispatchEnv(t, "attoktlock")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET last_success_at=NOW() WHERE id=$1`, acct); err != nil {
		t.Fatalf("seed success: %v", err)
	}

	pHold, err := pgxpool.New(ctx, e.moduleDSN)
	if err != nil {
		t.Fatalf("pool hold: %v", err)
	}
	defer pHold.Close()
	pMark, err := pgxpool.New(ctx, e.moduleDSN)
	if err != nil {
		t.Fatalf("pool mark: %v", err)
	}
	defer pMark.Close()

	// --- direction (a): account lock held FIRST, then markSending ---
	jobA := e.seedQueuedJob(t, "lock-a@example.com")
	e.dispatchToSending(t, jobA) // gives us a real worker id; then reset to 'assigned' for the mark below
	workerID := e.jobWorker(t, jobA)
	e.adminExecAsActor(t, "dispatcher",
		`UPDATE send_managed.jobs SET state='assigned', lease_expires_at=NOW() + INTERVAL '120 seconds' WHERE id=$1`, jobA)

	holdTx, err := pHold.Begin(ctx)
	if err != nil {
		t.Fatalf("begin hold tx: %v", err)
	}
	if _, err := holdTx.Exec(ctx, `SELECT 1 FROM send_managed.accounts WHERE id=$1 FOR UPDATE`, acct); err != nil {
		holdTx.Rollback(ctx)
		t.Fatalf("hold account FOR UPDATE: %v", err)
	}

	dMark := NewDispatcher(pMark, e.dec, e.px)
	type markRes struct {
		moved bool
		token int64
		err   error
	}
	markDone := make(chan markRes, 1)
	go func() {
		moved, token, err := dMark.markSending(ctx, jobA, acct, workerID)
		markDone <- markRes{moved: moved, token: token, err: err}
	}()

	// ANTI-VACUITY: markSending must genuinely be waiting (its trigger's KEY SHARE vs the held FOR
	// UPDATE). If it sailed through, the two lock claims never met and direction (a) proves nothing.
	select {
	case r := <-markDone:
		holdTx.Rollback(ctx)
		t.Fatalf("markSending completed (moved=%v err=%v) while an exclusive account lock was held — the interleaving did not occur, so this direction is vacuous", r.moved, r.err)
	case <-time.After(750 * time.Millisecond):
	}
	var blocked int
	if err := e.admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity
		  WHERE datname = current_database() AND wait_event_type='Lock' AND state='active'
		    AND query ILIKE '%send_managed.jobs%SET state=''sending''%'`).Scan(&blocked); err != nil {
		holdTx.Rollback(ctx)
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	if blocked == 0 {
		holdTx.Rollback(ctx)
		t.Fatal("no backend is lock-waiting — markSending is not contending with the held account lock (vacuous)")
	}

	// Release the account lock; markSending must complete cleanly and mint its token.
	if err := holdTx.Commit(ctx); err != nil {
		t.Fatalf("commit hold tx: %v", err)
	}
	select {
	case r := <-markDone:
		if r.err != nil {
			var pgErr *pgconn.PgError
			if errors.As(r.err, &pgErr) && pgErr.Code == "40P01" {
				t.Fatalf("markSending DEADLOCKED (40P01) against a held account FOR UPDATE: %v", r.err)
			}
			t.Fatalf("markSending failed after the account lock released: %v", r.err)
		}
		if !r.moved || r.token == 0 {
			t.Fatalf("markSending moved=%v token=%d, want true/non-zero", r.moved, r.token)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("markSending never completed after the account lock released — wedged")
	}

	// --- direction (b): the JOB row held by markSending, then the account→job outcome path ---
	jobB := e.seedQueuedJob(t, "lock-b@example.com")
	e.px.freeAllWorkers()
	e.dispatchToSending(t, jobB)
	tokenB := e.jobAttemptToken(t, jobB)
	workerB := e.jobWorker(t, jobB)

	jobTx, err := pMark.Begin(ctx)
	if err != nil {
		t.Fatalf("begin job tx: %v", err)
	}
	defer jobTx.Rollback(ctx)
	// Hold the JOB row with the same mark-shaped write (a no-op transition guarded on 'sending' would
	// match 0 rows, so lock the row explicitly — the lock is what the counterparty must wait on).
	if _, err := jobTx.Exec(ctx, `SELECT 1 FROM send_managed.jobs WHERE id=$1 FOR UPDATE`, jobB); err != nil {
		t.Fatalf("hold job row: %v", err)
	}

	outDone := make(chan bool, 1)
	go func() {
		oh := NewOutcomeHandler(pHold, e.cfg) // account FOR UPDATE first, then the job requeue
		outDone <- oh.Handle(workerB, workerproto.Report{
			Type: workerproto.TypeReport, JobID: jobB, AttemptToken: tokenB,
			Outcome: workerproto.OutcomeError, SMTPResponse: "451 4.3.0 temporary local error",
		})
	}()
	select {
	case <-outDone:
		t.Fatal("the outcome handler completed while the job row was held — direction (b) is vacuous")
	case <-time.After(750 * time.Millisecond):
	}
	// The statement being waited on is the outcome handler's failure UPDATE. It used to be recognised by
	// its text `SET state='queued'`; since send_managed_0028 (the attempt cap) the SAME statement chooses
	// failed-or-queued in a CASE, so the stable fragment unique to it is the counter it bumps. Scoped to this
	// clone (datname) AND to that statement (query ILIKE), per CLAUDE.md Rule 10.
	if err := e.admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity
		  WHERE datname = current_database() AND wait_event_type='Lock' AND state='active'
		    AND query ILIKE '%send_managed.jobs%failed_attempt_count = failed_attempt_count + 1%'`).Scan(&blocked); err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	if blocked == 0 {
		t.Fatal("no backend is lock-waiting in direction (b) — vacuous")
	}
	if err := jobTx.Commit(ctx); err != nil {
		t.Fatalf("commit job tx: %v", err)
	}
	select {
	case resolved := <-outDone:
		if !resolved {
			t.Fatal("the outcome was NOT resolved after the job row released — a deadlock/abort looks exactly like this")
		}
		if st, _, _, _, _ := e.jobState(t, jobB); st != "queued" {
			t.Fatalf("job state=%s after the classified failure, want queued", st)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the outcome handler never completed after the job row released — wedged")
	}

	// --- direction (c): markSending ITSELF, called for real, driven against a HELD job row ---
	// (R4 review addition: direction (b) above never actually calls markSending — a generic FOR
	// UPDATE stood in for it, so markSending's own SQL had never been raced against a job-row holder.
	// This closes that gap directly.)
	jobC := e.seedQueuedJob(t, "lock-c@example.com")
	e.px.freeAllWorkers()
	e.dispatchToSending(t, jobC) // gives us a real worker id + a queued job to reset to 'assigned'
	workerC := e.jobWorker(t, jobC)
	e.adminExecAsActor(t, "dispatcher",
		`UPDATE send_managed.jobs SET state='assigned', lease_expires_at=NOW() + INTERVAL '120 seconds' WHERE id=$1`, jobC)

	jobTxC, err := pHold.Begin(ctx)
	if err != nil {
		t.Fatalf("begin job tx (c): %v", err)
	}
	defer jobTxC.Rollback(ctx)
	// Hold the job row's ordinary write lock via a plain FOR UPDATE — the SAME kind of lock
	// markSending's own guarded UPDATE would take on this row, so this genuinely stands in the way of
	// the real call below (no KEY SHARE/trigger involved; this is the plain row-level lock every
	// UPDATE takes on its own target row).
	if _, err := jobTxC.Exec(ctx, `SELECT 1 FROM send_managed.jobs WHERE id=$1 FOR UPDATE`, jobC); err != nil {
		t.Fatalf("hold job row (c): %v", err)
	}

	markDoneC := make(chan markRes, 1)
	go func() {
		moved, token, err := dMark.markSending(ctx, jobC, acct, workerC)
		markDoneC <- markRes{moved: moved, token: token, err: err}
	}()

	// ANTI-VACUITY: markSending must genuinely be waiting on the held job row.
	select {
	case r := <-markDoneC:
		jobTxC.Rollback(ctx)
		t.Fatalf("markSending completed (moved=%v err=%v) while the job row was held — direction (c) is vacuous", r.moved, r.err)
	case <-time.After(750 * time.Millisecond):
	}
	if err := e.admin.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity
		  WHERE datname = current_database() AND wait_event_type='Lock' AND state='active'
		    AND query ILIKE '%send_managed.jobs%SET state=''sending''%'`).Scan(&blocked); err != nil {
		jobTxC.Rollback(ctx)
		t.Fatalf("read pg_stat_activity (c): %v", err)
	}
	if blocked == 0 {
		jobTxC.Rollback(ctx)
		t.Fatal("no backend is lock-waiting in direction (c) — markSending is not contending with the held job row (vacuous)")
	}

	// Release the job row; markSending must complete cleanly and mint its token — never 40P01.
	if err := jobTxC.Commit(ctx); err != nil {
		t.Fatalf("commit job tx (c): %v", err)
	}
	select {
	case r := <-markDoneC:
		if r.err != nil {
			var pgErr *pgconn.PgError
			if errors.As(r.err, &pgErr) && pgErr.Code == "40P01" {
				t.Fatalf("markSending DEADLOCKED (40P01) against a held job row: %v", r.err)
			}
			t.Fatalf("markSending failed after the job row released: %v", r.err)
		}
		if !r.moved || r.token == 0 {
			t.Fatalf("markSending (c) moved=%v token=%d, want true/non-zero", r.moved, r.token)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("markSending never completed after the job row released — wedged")
	}
}
