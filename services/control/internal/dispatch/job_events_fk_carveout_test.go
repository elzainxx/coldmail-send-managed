package dispatch

// job_events_fk_carveout_test.go — send_managed_0024 / ADR-0012 / G12-OD-1 (ruled option A).
//
// WHY THIS EXISTS. send_managed.job_events declares two `ON DELETE SET NULL` FKs (account_id → accounts,
// box_id → workers). Postgres implements SET NULL as an internal `UPDATE ONLY … SET <col> = NULL`, and
// send_managed_0008's BEFORE UPDATE reject-trigger raised on EVERY update with no carve-out — so the FK's
// own maintenance statement aborted and took the parent DELETE with it:
//
//	code=internal_error reason=account_write_error:
//	  ERROR: send_managed.job_events is append-only: UPDATE forbidden (SQLSTATE 23000)
//	CONTEXT: SQL statement "UPDATE ONLY "send_managed"."job_events" SET "account_id" = NULL WHERE …"
//
// Found walking Stage 12 step 7 of Docs/install-and-deploy.md#part-7--developing-and-testing-locally, on real infra: cleaning up
// a throwaway account that had actually SENT mail returned 500 from an endpoint api-contract.md
// documents as "hard delete (audit row survives)" → 204. An account with no job_events rows returned
// 204, because SET NULL then matches no row and issues no UPDATE — which is exactly why the defect
// survived the build: Phase 2's T5/T9 delete accounts that were never dispatched to. send_managed_0024 gives
// the trigger the same narrow per-nullable-FK-column carve-out account_events has had since
// send_managed_0014.
//
// ANTI-VACUITY (Rule 10). A test that only proved "the delete now succeeds" would pass just as
// happily against a tree with the trigger DROPPED ALTOGETHER — which would be a far worse bug (a
// silently mutable audit log). So every positive here is paired with the negative that distinguishes
// a narrow carve-out from no trigger at all, and all negatives are driven as the ADMIN/superuser
// connection: send_managed_app has UPDATE revoked (send_managed_0008), so a rejection observed as that role
// would prove only the REVOKE and would pass with no trigger present. The negatives must fail on the
// TRIGGER, so they must run as a role privileged enough to reach it.
//
//	POSITIVE  — deleting an account that HAS job_events rows succeeds; the rows survive with
//	            account_id NULL and every CONTENT column byte-identical to a pre-delete fingerprint.
//	POSITIVE  — same for a worker (box_id → NULL), and for an account and worker deleted in ONE
//	            transaction, which is the two-sequential-per-constraint-UPDATE case.
//	NEGATIVE  — a content UPDATE still raises.
//	NEGATIVE  — account_id→NULL BUNDLED with a content change still raises (not smuggled through).
//	NEGATIVE  — setting an already-NULL FK column back to non-NULL still raises (no history rewrite).
//	NEGATIVE  — a bare no-op UPDATE (nothing changes at all) still raises.
//	CONTROL   — an account with NO job_events rows still deletes, so the fix is not credited for
//	            behavior that already worked before send_managed_0024.

import (
	"context"
	"strings"
	"testing"
)

// jobEventFingerprint returns every CONTENT column of every job_events row for a job, keyed by row
// id, plus the two FK columns separately. The content half is what must be byte-identical across a
// parent delete; the FK half is what is allowed to go NULL. Splitting them is the whole point — a
// single whole-row hash could not tell "the FK was nulled" (permitted) from "the actor was rewritten"
// (forbidden).
func (e *dispatchEnv) jobEventFingerprint(t *testing.T, jobID string) (content map[string]string, acct, worker map[string]*string) {
	t.Helper()
	rows, err := e.admin.Query(context.Background(),
		`SELECT id::text,
		        -- Every content column, concatenated. seq is included: it is GENERATED ALWAYS AS
		        -- IDENTITY and must not move either.
		        job_id::text || '|' || user_id::text || '|' || seq::text || '|' ||
		        COALESCE(from_state::text,'~') || '|' || to_state::text || '|' || actor::text || '|' ||
		        COALESCE(error_class::text,'~') || '|' || COALESCE(error_message,'~') || '|' ||
		        COALESCE(metadata::text,'~') || '|' || created_at::text,
		        account_id::text, box_id::text
		   FROM send_managed.job_events WHERE job_id=$1 ORDER BY seq`, jobID)
	if err != nil {
		t.Fatalf("fingerprint job_events: %v", err)
	}
	defer rows.Close()
	content = map[string]string{}
	acct = map[string]*string{}
	worker = map[string]*string{}
	for rows.Next() {
		var id, c string
		var a, b *string
		if err := rows.Scan(&id, &c, &a, &b); err != nil {
			t.Fatalf("scan fingerprint: %v", err)
		}
		content[id] = c
		acct[id] = a
		worker[id] = b
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate fingerprint: %v", err)
	}
	if len(content) == 0 {
		t.Fatalf("job %s has NO job_events rows — the test would prove nothing about SET NULL, which only issues an UPDATE when it matches a row", jobID)
	}
	return content, acct, worker
}

// oneEventID returns the id of one job_events row for jobID that has BOTH FK columns non-NULL — the
// only shape on which the negative controls are meaningful (a row with a NULL account_id could not
// exercise the non-null → NULL branch).
func (e *dispatchEnv) oneEventIDWithBothFKs(t *testing.T, jobID string) string {
	t.Helper()
	var id string
	if err := e.admin.QueryRow(context.Background(),
		`SELECT id::text FROM send_managed.job_events
		   WHERE job_id=$1 AND account_id IS NOT NULL AND box_id IS NOT NULL
		   ORDER BY seq LIMIT 1`, jobID).Scan(&id); err != nil {
		t.Fatalf("no job_events row with both FKs set for job %s: %v (the dispatch path should have stamped assigned_account_id and assigned_box_id)", jobID, err)
	}
	return id
}

// dispatchAndSettle seeds a pool + one eligible account, then drives one job all the way to a
// terminal state through the REAL dispatch and outcome paths, so its job_events rows carry genuinely
// trigger-written account_id and box_id rather than values a test INSERTed. That matters: the defect
// is in what the FK maintenance update does to rows the system itself wrote.
func (e *dispatchEnv) dispatchAndSettle(t *testing.T, recipient string) string {
	t.Helper()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "s3cret-carveout")
	job := e.seedQueuedJob(t, recipient)
	e.dispatchToSending(t, job)
	e.settleAllSending(t)
	return job
}

// TestFKCarveout_AccountDeleteWithJobHistory — the headline regression: an account that has been
// assigned a job can be hard-deleted, its job_events rows survive with account_id NULL, and every
// content column is byte-identical.
func TestFKCarveout_AccountDeleteWithJobHistory(t *testing.T) {
	e := newDispatchEnv(t, "fkcarve1")
	ctx := context.Background()

	job := e.dispatchAndSettle(t, "carveout-acct@example.com")
	before, acctBefore, workerBefore := e.jobEventFingerprint(t, job)

	// The row(s) really did point at an account, or the SET NULL below would match nothing and the
	// whole test would be vacuous.
	var pointing int
	for _, a := range acctBefore {
		if a != nil {
			pointing++
		}
	}
	if pointing == 0 {
		t.Fatal("no job_events row references an account — SET NULL would issue no UPDATE and the trigger would never be reached (vacuous)")
	}

	var acctID string
	if err := e.admin.QueryRow(ctx,
		`SELECT assigned_account_id::text FROM send_managed.jobs WHERE id=$1`, job).Scan(&acctID); err != nil {
		t.Fatalf("read assigned account: %v", err)
	}

	// THE DELETE. Before send_managed_0024 this failed with "send_managed.job_events is append-only: UPDATE
	// forbidden" and took the whole statement with it.
	if _, err := e.admin.Exec(ctx, `DELETE FROM send_managed.accounts WHERE id=$1`, acctID); err != nil {
		t.Fatalf("DELETE account with job history failed: %v — this is ADR-0012's wedge; send_managed_0024's carve-out is missing or too narrow", err)
	}

	after, acctAfter, workerAfter := e.jobEventFingerprint(t, job)

	// The audit SURVIVED — same rows, not fewer (nothing cascaded away).
	if len(after) != len(before) {
		t.Fatalf("job_events row count changed across the account delete: %d -> %d, want unchanged (the audit must survive, not cascade)", len(before), len(after))
	}
	for id, want := range before {
		got, ok := after[id]
		if !ok {
			t.Fatalf("job_events row %s disappeared across the account delete", id)
		}
		if got != want {
			t.Fatalf("job_events row %s CONTENT changed across the account delete:\n  before: %s\n  after:  %s\nthe carve-out permitted a content mutation — it is too wide", id, want, got)
		}
	}
	// ...with account_id nulled everywhere it pointed at the deleted account...
	for id, a := range acctAfter {
		if a != nil {
			t.Fatalf("job_events row %s still has account_id=%s after the account was deleted, want NULL", id, *a)
		}
	}
	// ...and box_id UNTOUCHED. This is the per-column half of the carve-out: nulling one FK must not
	// collaterally null the other, which a looser "an FK changed" predicate would have allowed.
	for id, b := range workerAfter {
		bb := workerBefore[id]
		if (b == nil) != (bb == nil) || (b != nil && bb != nil && *b != *bb) {
			t.Fatalf("job_events row %s box_id changed across an ACCOUNT delete (before=%v after=%v) — the carve-out is not per-column", id, deref(bb), deref(b))
		}
	}
}

// TestFKCarveout_WorkerDeleteWithJobHistory — the second SET NULL FK, driven independently. Deleting a
// worker that has processed a job nulls box_id and leaves account_id and all content alone.
func TestFKCarveout_WorkerDeleteWithJobHistory(t *testing.T) {
	e := newDispatchEnv(t, "fkcarve2")
	ctx := context.Background()

	job := e.dispatchAndSettle(t, "carveout-worker@example.com")
	before, acctBefore, workerBefore := e.jobEventFingerprint(t, job)

	var pointing int
	for _, b := range workerBefore {
		if b != nil {
			pointing++
		}
	}
	if pointing == 0 {
		t.Fatal("no job_events row references a worker — SET NULL would issue no UPDATE and the trigger would never be reached (vacuous)")
	}

	workerID := e.jobWorker(t, job)
	// send_managed.jobs.assigned_box_id is ALSO ON DELETE SET NULL, and that table's state trigger is not
	// involved (assigned_box_id is not `state`), so the worker delete reaches job_events directly.
	if _, err := e.admin.Exec(ctx, `DELETE FROM send_managed.boxes WHERE id=$1`, workerID); err != nil {
		t.Fatalf("DELETE worker with job history failed: %v — ADR-0012's second SET NULL FK is still wedged", err)
	}

	after, acctAfter, workerAfter := e.jobEventFingerprint(t, job)
	if len(after) != len(before) {
		t.Fatalf("job_events row count changed across the worker delete: %d -> %d, want unchanged", len(before), len(after))
	}
	for id, want := range before {
		if got := after[id]; got != want {
			t.Fatalf("job_events row %s CONTENT changed across the worker delete:\n  before: %s\n  after:  %s", id, want, got)
		}
	}
	for id, b := range workerAfter {
		if b != nil {
			t.Fatalf("job_events row %s still has box_id=%s after the worker was deleted, want NULL", id, *b)
		}
	}
	for id, a := range acctAfter {
		ab := acctBefore[id]
		if (a == nil) != (ab == nil) || (a != nil && ab != nil && *a != *ab) {
			t.Fatalf("job_events row %s account_id changed across a WORKER delete (before=%v after=%v) — the carve-out is not per-column", id, deref(ab), deref(a))
		}
	}
}

// TestFKCarveout_AccountAndWorkerDeletedTogether — both FKs nulled for the SAME rows in ONE transaction.
// Postgres issues one `UPDATE ONLY … SET <col> = NULL` per FK CONSTRAINT rather than one combined
// statement, so the two fire SEQUENTIALLY and the second sees the first's NULL already in place. That
// is why the carve-out's unchanged-side comparisons use IS NOT DISTINCT FROM: with a plain `=`, the
// second update's `NEW.account_id = OLD.account_id` would be NULL (not TRUE) and the whole predicate
// would fail closed, re-wedging the delete. This test is what makes that reasoning load-bearing
// instead of a comment — a control run with `=` substituted fails HERE and, unanticipated, in
// TestFKCarveout_WorkerDeleteWithJobHistory too: the genesis event row is born with account_id NULL (the
// job is not yet assigned when fn_job_events_on_insert fires), so `=` wedges even a plain worker delete.
func TestFKCarveout_AccountAndWorkerDeletedTogether(t *testing.T) {
	e := newDispatchEnv(t, "fkcarve3")
	ctx := context.Background()

	job := e.dispatchAndSettle(t, "carveout-both@example.com")
	before, _, _ := e.jobEventFingerprint(t, job)
	// Prove at least one row carries BOTH, or "both at once" is not actually being exercised.
	e.oneEventIDWithBothFKs(t, job)

	var acctID string
	if err := e.admin.QueryRow(ctx,
		`SELECT assigned_account_id::text FROM send_managed.jobs WHERE id=$1`, job).Scan(&acctID); err != nil {
		t.Fatalf("read assigned account: %v", err)
	}
	workerID := e.jobWorker(t, job)

	tx, err := e.admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `DELETE FROM send_managed.accounts WHERE id=$1`, acctID); err != nil {
		t.Fatalf("DELETE account (combined tx): %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM send_managed.boxes WHERE id=$1`, workerID); err != nil {
		t.Fatalf("DELETE worker (combined tx) failed: %v — the SECOND per-constraint SET NULL was rejected, which is the NULL-vs-`=` trap the carve-out's IS NOT DISTINCT FROM exists to avoid", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit combined delete: %v", err)
	}

	after, acctAfter, workerAfter := e.jobEventFingerprint(t, job)
	if len(after) != len(before) {
		t.Fatalf("job_events row count changed across the combined delete: %d -> %d, want unchanged", len(before), len(after))
	}
	for id, want := range before {
		if got := after[id]; got != want {
			t.Fatalf("job_events row %s CONTENT changed across the combined delete:\n  before: %s\n  after:  %s", id, want, got)
		}
	}
	for id := range after {
		if acctAfter[id] != nil || workerAfter[id] != nil {
			t.Fatalf("job_events row %s: account_id=%v box_id=%v after BOTH parents were deleted, want both NULL", id, deref(acctAfter[id]), deref(workerAfter[id]))
		}
	}
}

// TestFKCarveout_NegativeControls — THE TEETH. Everything the carve-out must still reject, driven as
// the ADMIN (superuser) connection so the rejection can only come from the TRIGGER: send_managed_app has
// UPDATE revoked, so observing a failure as that role would prove the REVOKE and would pass against a
// tree with no trigger at all.
//
// Each case is paired against the positives above: if the carve-out were widened to "any update that
// touches an FK column", cases (b) and (c) would pass and this test fails. If the trigger were
// dropped, all four pass and this test fails. If the carve-out were reverted to unconditional, the
// three positive tests above fail. No single regression satisfies both halves.
func TestFKCarveout_NegativeControls(t *testing.T) {
	e := newDispatchEnv(t, "fkcarve4")
	ctx := context.Background()

	job := e.dispatchAndSettle(t, "carveout-neg@example.com")
	evID := e.oneEventIDWithBothFKs(t, job)

	const wantMsg = "append-only: UPDATE forbidden"
	mustReject := func(label, sql string, args ...any) {
		t.Helper()
		_, err := e.admin.Exec(ctx, sql, args...)
		if err == nil {
			t.Fatalf("%s: UPDATE succeeded, want rejection — the carve-out is too wide (or the trigger is gone)", label)
		}
		// Assert it is the APPEND-ONLY trigger rejecting, not an incidental type/constraint error
		// that would make the test pass for the wrong reason.
		if !strings.Contains(err.Error(), wantMsg) {
			t.Fatalf("%s: rejected, but not by the append-only trigger: %v (want a message containing %q)", label, err, wantMsg)
		}
	}

	// (a) A plain content mutation. The core immutability guarantee, unchanged by send_managed_0024.
	mustReject("content UPDATE (actor)",
		`UPDATE send_managed.job_events SET actor='admin' WHERE id=$1`, evID)
	mustReject("content UPDATE (error_message)",
		`UPDATE send_managed.job_events SET error_message='tampered' WHERE id=$1`, evID)

	// (b) A content change SMUGGLED IN alongside a legitimate null-out. This is the case the
	// account_events carve-out was written to block (ADR-0009) and the reason the predicate compares
	// every content column rather than trusting the FK change.
	mustReject("account_id->NULL bundled with a content change",
		`UPDATE send_managed.job_events SET account_id=NULL, actor='admin' WHERE id=$1`, evID)
	mustReject("box_id->NULL bundled with a content change",
		`UPDATE send_managed.job_events SET box_id=NULL, to_state='failed' WHERE id=$1`, evID)

	// (c) A bare no-op UPDATE. Nothing changes, so no referential action performs it — it is just an
	// UPDATE on an append-only table and must still raise. Without the "at least one FK actually
	// nulled" clause this would be silently blessed.
	mustReject("no-op UPDATE (nothing changes)",
		`UPDATE send_managed.job_events SET actor=actor WHERE id=$1`, evID)

	// (d) Re-pointing a nulled FK back at a real row — rewriting provenance to blame an account that
	// was never involved. Null it legitimately first (via a real parent delete, the only permitted
	// path), then try to restore it.
	var acctID string
	if err := e.admin.QueryRow(ctx,
		`SELECT assigned_account_id::text FROM send_managed.jobs WHERE id=$1`, job).Scan(&acctID); err != nil {
		t.Fatalf("read assigned account: %v", err)
	}
	pool := e.seedPool(t)
	other := e.seedAccount(t, pool, "s3cret-other")
	if _, err := e.admin.Exec(ctx, `DELETE FROM send_managed.accounts WHERE id=$1`, acctID); err != nil {
		t.Fatalf("legitimate account delete (setup for case d): %v", err)
	}
	var nowNull bool
	if err := e.admin.QueryRow(ctx,
		`SELECT account_id IS NULL FROM send_managed.job_events WHERE id=$1`, evID).Scan(&nowNull); err != nil {
		t.Fatalf("re-read event: %v", err)
	}
	if !nowNull {
		t.Fatal("account_id is not NULL after the parent delete — case (d) would test nothing")
	}
	mustReject("NULL account_id -> non-NULL (history rewrite)",
		`UPDATE send_managed.job_events SET account_id=$1 WHERE id=$2`, other, evID)
}

// TestFKCarveout_NoHistoryAccountStillDeletes — the regression guard, so send_managed_0024 is not credited
// for behavior that already worked. An account with ZERO job_events rows always deleted cleanly
// (SET NULL matches no row, issues no UPDATE, never reaches the trigger) — that split is precisely
// what hid the defect from Phase 2's T5/T9. It must STILL work.
func TestFKCarveout_NoHistoryAccountStillDeletes(t *testing.T) {
	e := newDispatchEnv(t, "fkcarve5")
	ctx := context.Background()

	pool := e.seedPool(t)
	fresh := e.seedAccount(t, pool, "s3cret-fresh")

	var n int
	if err := e.admin.QueryRow(ctx,
		`SELECT count(*) FROM send_managed.job_events WHERE account_id=$1`, fresh).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	if n != 0 {
		t.Fatalf("the 'no history' account has %d job_events rows, want 0 — this control is not testing the no-UPDATE path", n)
	}
	if _, err := e.admin.Exec(ctx, `DELETE FROM send_managed.accounts WHERE id=$1`, fresh); err != nil {
		t.Fatalf("DELETE account with NO job history failed: %v — send_managed_0024 broke a path that already worked", err)
	}
}

func deref(s *string) string {
	if s == nil {
		return "NULL"
	}
	return *s
}
