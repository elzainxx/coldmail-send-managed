package dispatch

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// TestLockOrderAudit_ConcurrentAcceptedSameAccount (closeout Chunk 1 — empirical confirmation) —
// applyAccepted is job→account (it UPDATEs the job first, firing the job_events trigger's account
// FK KEY SHARE, THEN mutates the account via recomputeScoreOnSuccess). Two successful sends on the
// SAME account (different jobs — per-user lanes allow one account to serve several in-flight jobs),
// reported concurrently on separate connections, must NOT deadlock. This grounds the closeout's
// honest lock-order enumeration: applyAccepted does not take the strong account FOR UPDATE (only the
// FK KEY SHARE + a FOR NO KEY UPDATE via the score UPDATE), so it serializes rather than cycling —
// but it is the one outcome sub-path that does NOT follow the account-first rule (see P6-CLOSE-1).
func TestLockOrderAudit_ConcurrentAcceptedSameAccount(t *testing.T) {
	e := newDispatchEnv(t, "lockaudit_acc")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")

	// Two jobs both in-flight (sending) on the SAME account.
	jobs := []string{e.seedQueuedJob(t, "a1@example.com"), e.seedQueuedJob(t, "a2@example.com")}
	for _, j := range jobs {
		tx, err := e.admin.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('send_managed.actor','dispatcher',true)`); err != nil {
			t.Fatalf("actor: %v", err)
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

	p1, _ := pgxpool.New(ctx, e.moduleDSN)
	defer p1.Close()
	p2, _ := pgxpool.New(ctx, e.moduleDSN)
	defer p2.Close()
	oh1 := NewOutcomeHandler(p1, e.cfg)
	oh2 := NewOutcomeHandler(p2, e.cfg)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		oh1.Handle(e.px.workerID, workerproto.Report{JobID: jobs[0], Outcome: workerproto.OutcomeAccepted, ProviderMessageID: "PMID-LA-1", SMTPResponse: "250 ok"})
	}()
	go func() {
		defer wg.Done()
		oh2.Handle(e.px.workerID, workerproto.Report{JobID: jobs[1], Outcome: workerproto.OutcomeAccepted, ProviderMessageID: "PMID-LA-2", SMTPResponse: "250 ok"})
	}()
	wg.Wait()

	// Both must have committed sent (no deadlock aborted either). A 40P01 would have left one job
	// un-sent (the handler logs+swallows the error).
	for _, j := range jobs {
		if st, hasPMID, _, _, _ := e.jobState(t, j); st != "sent" || !hasPMID {
			t.Fatalf("job %s state=%s hasPMID=%v after concurrent accepts, want sent/true (a deadlock would abort one)", j, st, hasPMID)
		}
	}
}

// TestLockOrderAudit_AcceptedVsClassifiedSameAccount (closeout Chunk 1) — applyAccepted (job→account)
// racing applyClassifiedFailure (account→job) on the SAME account but DIFFERENT jobs must not
// deadlock. Confirms the mixed-order pair is safe because applyAccepted never takes the strong
// account FOR UPDATE.
func TestLockOrderAudit_AcceptedVsClassifiedSameAccount(t *testing.T) {
	e := newDispatchEnv(t, "lockaudit_mix")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET last_success_at=NOW() WHERE id=$1`, acct); err != nil {
		t.Fatalf("seed success: %v", err)
	}

	jobAcc := e.seedQueuedJob(t, "mix-acc@example.com")
	jobFail := e.seedQueuedJob(t, "mix-fail@example.com")
	for _, j := range []string{jobAcc, jobFail} {
		tx, _ := e.admin.Begin(ctx)
		_, _ = tx.Exec(ctx, `SELECT set_config('send_managed.actor','dispatcher',true)`)
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

	p1, _ := pgxpool.New(ctx, e.moduleDSN)
	defer p1.Close()
	p2, _ := pgxpool.New(ctx, e.moduleDSN)
	defer p2.Close()
	oh1 := NewOutcomeHandler(p1, e.cfg)
	oh2 := NewOutcomeHandler(p2, e.cfg)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		oh1.Handle(e.px.workerID, workerproto.Report{JobID: jobAcc, Outcome: workerproto.OutcomeAccepted, ProviderMessageID: "PMID-MIX-1", SMTPResponse: "250 ok"})
	}()
	go func() {
		defer wg.Done()
		oh2.Handle(e.px.workerID, workerproto.Report{JobID: jobFail, Outcome: workerproto.OutcomeError, SMTPResponse: "451 4.3.0 temporary local error"})
	}()
	wg.Wait()

	if st, hasPMID, _, _, _ := e.jobState(t, jobAcc); st != "sent" || !hasPMID {
		t.Fatalf("accepted job state=%s hasPMID=%v, want sent/true (deadlock would abort it)", st, hasPMID)
	}
	if st, _, _, _, _ := e.jobState(t, jobFail); st != "queued" {
		t.Fatalf("classified-failure job state=%s, want queued (requeued)", st)
	}
}
