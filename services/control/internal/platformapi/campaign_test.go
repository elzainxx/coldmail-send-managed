package platformapi

// Chunk 5 behavioral tests (Phase 4, §8, P4-7): one-active-campaign-per-user is enforced at `run`
// under a per-user advisory lock. Real Postgres; the concurrency variant fires TWO real concurrent
// HTTP requests against the SAME running Server to prove the check-then-insert is genuinely
// serialized, not just correct in the single-threaded case.

import (
	"context"
	"sync"
	"testing"
)

// newCampaignID returns a fresh random UUID (via the admin connection) to use as a DISTINCT
// campaign_id — seedJobItem's own default (campaign_id == user_id) can't express "two campaigns,
// same user."
func (e *testEnv) newCampaignID(t *testing.T) string {
	t.Helper()
	var id string
	if err := e.admin.QueryRow(context.Background(), `SELECT gen_random_uuid()::text`).Scan(&id); err != nil {
		t.Fatalf("gen campaign id: %v", err)
	}
	return id
}

// TestCampaign_SecondConcurrentCampaignRejectedCleanly — user U has an active campaign C1
// (queued job). Starting a DIFFERENT campaign C2 for U is rejected with a clean 409 conflict, and
// crucially creates ZERO job rows for C2 (no partial enqueue) — a later tick must never pick up an
// orphaned C2 job. C1 continues completely unaffected. Positive control: a SECOND job_item under
// the SAME campaign C1 is accepted normally (repeat-run, not a new campaign).
func TestCampaign_SecondConcurrentCampaignRejectedCleanly(t *testing.T) {
	e := newTestEnv(t, "campreject")
	userU := e.seedUser(t, "campU@example.com")
	c1 := e.newCampaignID(t)
	c2 := e.newCampaignID(t)

	item1 := e.seedJobItemCampaign(t, c1, userU, "c1-item1@example.com")
	run1 := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item1, "user_id": userU}).
		requireStatus(t, 202)
	job1 := run1.str("job_id")
	if job1 == "" {
		t.Fatalf("run(C1) returned no job_id: %s", run1.raw)
	}

	// A different campaign for the SAME user → clean 409, zero job rows for C2.
	item2 := e.seedJobItemCampaign(t, c2, userU, "c2-item1@example.com")
	rejected := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item2, "user_id": userU}).
		requireStatus(t, 409)
	if rejected.errorCode() != codeConflict {
		t.Fatalf("second campaign run: code=%s, want conflict", rejected.errorCode())
	}
	var n int
	if err := e.admin.QueryRow(bgctx(), `SELECT count(*) FROM send_managed.jobs WHERE campaign_id=$1`, c2).Scan(&n); err != nil {
		t.Fatalf("count C2 jobs: %v", err)
	}
	if n != 0 {
		t.Fatalf("C2 has %d job row(s) after a rejected run — no partial enqueue is allowed", n)
	}
	// No job_events reference C2 either (nothing was ever created to transition).
	var evN int
	if err := e.admin.QueryRow(bgctx(),
		`SELECT count(*) FROM send_managed.job_events je JOIN send_managed.jobs j ON j.id = je.job_id WHERE j.campaign_id = $1`, c2).Scan(&evN); err != nil {
		t.Fatalf("count C2 events: %v", err)
	}
	if evN != 0 {
		t.Fatalf("C2 has %d job_events row(s) despite the rejected run", evN)
	}

	// C1 is completely unaffected: still there, still queued.
	stillC1 := e.jobUserID(t, job1)
	if stillC1 != userU {
		t.Fatalf("C1's job user_id=%s after the rejected C2 attempt, want unaffected (%s)", stillC1, userU)
	}

	// Positive control: a SECOND job_item under the SAME campaign C1 is a normal accept (repeat
	// run for the active campaign, not a "second campaign").
	item1b := e.seedJobItemCampaign(t, c1, userU, "c1-item2@example.com")
	ok := e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item1b, "user_id": userU}).
		requireStatus(t, 202)
	if ok.str("job_id") == "" {
		t.Fatalf("same-campaign second item was rejected: %s", ok.raw)
	}
}

// TestCampaign_ConcurrentSecondCampaignStartsBothRejected — fire TWO concurrent `run` calls for a
// DIFFERENT second campaign for the SAME user, both racing against an already-active first
// campaign. The per-user advisory lock must serialize the check-then-insert so BOTH concurrent
// attempts are rejected (never "one slips through because both read 'no active campaign' before
// either commits") — proving genuine concurrency safety, not just single-threaded correctness.
func TestCampaign_ConcurrentSecondCampaignStartsBothRejected(t *testing.T) {
	e := newTestEnv(t, "campconcurrent")
	userU := e.seedUser(t, "concU@example.com")
	c1 := e.newCampaignID(t)
	c2 := e.newCampaignID(t)

	item1 := e.seedJobItemCampaign(t, c1, userU, "cc1-item1@example.com")
	e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item1, "user_id": userU}).requireStatus(t, 202)

	item2a := e.seedJobItemCampaign(t, c2, userU, "cc2-item1@example.com")
	item2b := e.seedJobItemCampaign(t, c2, userU, "cc2-item2@example.com")

	var wg sync.WaitGroup
	codes := make([]int, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		codes[0] = e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item2a, "user_id": userU}).status
	}()
	go func() {
		defer wg.Done()
		codes[1] = e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": item2b, "user_id": userU}).status
	}()
	wg.Wait()

	for i, code := range codes {
		if code != 409 {
			t.Fatalf("concurrent second-campaign attempt #%d returned %d, want 409 (both must be rejected)", i, code)
		}
	}
	var n int
	if err := e.admin.QueryRow(bgctx(), `SELECT count(*) FROM send_managed.jobs WHERE campaign_id=$1`, c2).Scan(&n); err != nil {
		t.Fatalf("count C2 jobs: %v", err)
	}
	if n != 0 {
		t.Fatalf("C2 has %d job row(s) after BOTH concurrent attempts were supposedly rejected — the advisory lock did not serialize the check-then-insert", n)
	}
}

// TestCampaign_DifferentUsersNeverConflict — anti-vacuity: two DIFFERENT users each starting a
// campaign concurrently never interfere with each other (the lock is per-user, not global).
func TestCampaign_DifferentUsersNeverConflict(t *testing.T) {
	e := newTestEnv(t, "campdifferent")
	userA := e.seedUser(t, "diffA@example.com")
	userB := e.seedUser(t, "diffB@example.com")
	itemA := e.seedJobItem(t, userA, "diffA-item@example.com")
	itemB := e.seedJobItem(t, userB, "diffB-item@example.com")

	var wg sync.WaitGroup
	statusA, statusB := 0, 0
	wg.Add(2)
	go func() {
		defer wg.Done()
		statusA = e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": itemA, "user_id": userA}).status
	}()
	go func() {
		defer wg.Done()
		statusB = e.do(t, "POST", "/v1/run", map[string]any{"job_item_id": itemB, "user_id": userB}).status
	}()
	wg.Wait()

	if statusA != 202 || statusB != 202 {
		t.Fatalf("statusA=%d statusB=%d, want both 202 (different users never conflict)", statusA, statusB)
	}
}
