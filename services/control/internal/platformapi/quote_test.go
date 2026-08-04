package platformapi

// quote_test.go — Phase 9 Chunk 1 (§1.2): quote never under- or over-quotes, reconciled against
// what run + report actually consume. Real Postgres, real HTTP against the actual production
// Server (CLAUDE.md Rule 3) — no mocks.
//
// The correctness claim under test: cost = recipient_count * unit_price_credits, and that number
// is EXACTLY what gets billed regardless of retries/requeues/reclaims/duplicate-run attempts,
// because none of those create a second send_managed.jobs row for the same job_item (Phase 3 §6.2's
// job_item_id UNIQUE constraint) — they re-dispatch the SAME job. A rare accepted-duplicate
// delivery (crash-after-250) is an extra DELIVERY, never an extra billed JOB (Phase 6 §2.6).

import (
	"testing"
)

// TestQuote_MatchesRecipientCountTimesUnit — quote(N) == N * unit_price_credits, and the additive
// unit_price_credits/tier fields are present (Phase 9 §1.1).
func TestQuote_MatchesRecipientCountTimesUnit(t *testing.T) {
	e := newTestEnv(t, "quotebasic")
	resp := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": 250}).requireStatus(t, 200)
	if cost, ok := resp.body["cost"].(float64); !ok || cost != 250 {
		t.Fatalf("cost=%v, want 250 (default unit_price_credits=1): %s", resp.body["cost"], resp.raw)
	}
	if unit, ok := resp.body["unit_price_credits"].(float64); !ok || unit != 1 {
		t.Fatalf("unit_price_credits=%v, want 1: %s", resp.body["unit_price_credits"], resp.raw)
	}
	if tier, ok := resp.body["tier"].(string); !ok || tier == "" {
		t.Fatalf("tier missing from quote response: %s", resp.raw)
	}
	// quote is pure and idempotent — calling it again yields the SAME number, reserves nothing.
	resp2 := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": 250}).requireStatus(t, 200)
	if resp2.body["cost"] != resp.body["cost"] {
		t.Fatalf("quote is not idempotent: first=%v second=%v", resp.body["cost"], resp2.body["cost"])
	}
	var jobCount int
	if err := e.admin.QueryRow(bgctx(), `SELECT count(*) FROM send_managed.jobs`).Scan(&jobCount); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	if jobCount != 0 {
		t.Fatalf("quote created %d job row(s) — quote must be side-effect-free", jobCount)
	}
}

// TestQuote_NeverOverOrUnderQuotesAcrossRetryAndDuplicateRun — the reconciliation claim (§1.2):
// N recipients quoted at N*unit; after creating the N jobs via run, forcing one job through
// several retry/attempt-count cycles (simulating Phase 5/6 requeue+reclaim) and attempting a
// duplicate run for one job_item, the BILLABLE unit count (one send_managed.jobs row per job_item) is
// still exactly N — never more (over-quote: retries would have to fabricate extra billable units)
// and never fewer (under-quote: a job must not vanish under retry).
func TestQuote_NeverOverOrUnderQuotesAcrossRetryAndDuplicateRun(t *testing.T) {
	e := newTestEnv(t, "quoterecon")
	userU := e.seedUser(t, "quoterecon@example.com")
	const n = 5

	quoted := e.do(t, "POST", "/v1/quote", map[string]any{"recipient_count": n}).requireStatus(t, 200)
	if cost, ok := quoted.body["cost"].(float64); !ok || int(cost) != n {
		t.Fatalf("quote(%d) cost=%v, want %d", n, quoted.body["cost"], n)
	}

	jobIDs := make([]string, 0, n)
	for i := 0; i < n; i++ {
		item := e.seedJobItem(t, userU, "recon"+string(rune('a'+i))+"@example.com")
		run := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item, "user_id": userU}).
			requireStatus(t, 202)
		jobIDs = append(jobIDs, run.str("job_id"))
	}

	// Simulate retry/reclaim churn on the FIRST job: bump attempt_count and cycle it back to
	// queued several times, exactly as the outcome handler / reclaim loop would (Phase 5 §6,
	// Phase 6 §2) — this must NOT create any new send_managed.jobs row for the same job_item.
	for cycle := 0; cycle < 3; cycle++ {
		e.actorExec(t, "dispatcher",
			`UPDATE send_managed.jobs SET state='sending', lease_expires_at=NOW()+interval '30 seconds' WHERE id=$1`, jobIDs[0])
		e.actorExec(t, "reclaim_loop",
			`UPDATE send_managed.jobs SET state='queued', lease_expires_at=NULL, attempt_count=attempt_count+1 WHERE id=$1`, jobIDs[0])
	}

	// Duplicate run for the SAME job_item (same tenant) must return the existing job, not create
	// a new row — proving retries/duplicates never inflate the billable unit count.
	firstItem, _ := e.jobItemIDForJob(t, jobIDs[0])
	dup := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": firstItem, "user_id": userU}).
		requireStatus(t, 200)
	if dup.str("job_id") != jobIDs[0] {
		t.Fatalf("duplicate run returned job_id=%s, want the original %s (no second job created)", dup.str("job_id"), jobIDs[0])
	}

	var totalJobs int
	if err := e.admin.QueryRow(bgctx(), `SELECT count(*) FROM send_managed.jobs WHERE user_id=$1`, userU).Scan(&totalJobs); err != nil {
		t.Fatalf("count total jobs: %v", err)
	}
	if totalJobs != n {
		t.Fatalf("billable job rows=%d after retry churn + duplicate run, want exactly %d (never over- or under-quotes)", totalJobs, n)
	}
}

// actorExec runs a state-changing UPDATE inside a transaction with send_managed.actor set (required by
// the send_managed_0008 job_events trigger for any non-genesis transition, per its own RAISE EXCEPTION).
func (e *testEnv) actorExec(t *testing.T, actor, sql string, args ...any) {
	t.Helper()
	tx, err := e.admin.Begin(bgctx())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(bgctx())
	if _, err := tx.Exec(bgctx(), `SELECT set_config('send_managed.actor', $1, true)`, actor); err != nil {
		t.Fatalf("set actor: %v", err)
	}
	if _, err := tx.Exec(bgctx(), sql, args...); err != nil {
		t.Fatalf("actorExec(%s): %v", sql, err)
	}
	if err := tx.Commit(bgctx()); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// jobItemIDForJob reads back the job_item_id for a send_managed.jobs row (test helper).
func (e *testEnv) jobItemIDForJob(t *testing.T, jobID string) (string, bool) {
	t.Helper()
	var id string
	if err := e.admin.QueryRow(bgctx(), `SELECT job_item_id::text FROM send_managed.jobs WHERE id=$1`, jobID).Scan(&id); err != nil {
		t.Fatalf("job_item_id for job: %v", err)
	}
	return id, true
}
