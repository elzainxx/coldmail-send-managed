package harness

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Phase 1 behavioral tests — claim concurrency + event-log triggers. REAL Postgres via the
// template-clone (Mode A): real committed rows, multiple distinct connections, exactly the
// SKIP-LOCKED / trigger semantics as prod (CLAUDE.md Rule 3; no mocks). Each test clones a
// fresh migrated DB so concurrency is genuine and isolation is per-test.

// claimQuery is the LOCKED dispatcher claim CTE (Phase 1 §6 / 02-claim-and-indexes.md), verbatim
// except LIMIT is parameterized. FOR UPDATE SKIP LOCKED => exactly one claimer wins each row.
const claimQuery = `
WITH claimed AS (
    SELECT id FROM send_managed.jobs
    WHERE state = 'queued' AND next_attempt_at <= NOW()
    ORDER BY next_attempt_at ASC, created_at ASC
    FOR UPDATE SKIP LOCKED
    LIMIT $1
)
UPDATE send_managed.jobs
SET state = 'assigned',
    lease_expires_at = NOW() + INTERVAL '120 seconds',
    updated_at = NOW()
WHERE id IN (SELECT id FROM claimed)
RETURNING id`

// claimOnce runs the claim CTE once, with the actor GUC set, in its own transaction on conn c,
// returning the ids it claimed. The GUC + UPDATE must be in the SAME tx so the state-change
// trigger sees the actor.
func claimOnce(ctx context.Context, c *pgx.Conn, limit int) ([]string, error) {
	tx, err := c.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('send_managed.actor', 'dispatcher', true)"); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, claimQuery, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ids, nil
}

// TestT1_ConcurrentClaimExactlyOneWinner — two concurrent transactions claim ONE queued row;
// exactly one wins, the other gets zero. Proven under genuine concurrency (two distinct TCP
// connections against a template clone, launched behind a barrier). Anti-vacuity: assert the
// job started 'queued' and ended 'assigned' exactly once.
func TestT1_ConcurrentClaimExactlyOneWinner(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "t1claim")
	defer cleanup()

	seed := Connect(t, cloneDSN)
	uid := SeedUser(t, seed, "t1@example.com")
	cid := uid // any uuid; reuse for campaign
	jobID := InsertQueuedJob(t, seed, uid, cid, "rcpt-t1@example.com")

	// Anti-vacuity precondition: the job is genuinely 'queued' before the race.
	var pre string
	if err := seed.QueryRow(ctx, "SELECT state FROM send_managed.jobs WHERE id=$1", jobID).Scan(&pre); err != nil {
		t.Fatalf("pre state: %v", err)
	}
	if pre != "queued" {
		t.Fatalf("anti-vacuity: job not queued before race (%s)", pre)
	}

	c1 := Connect(t, cloneDSN)
	c2 := Connect(t, cloneDSN) // distinct TCP session

	var wg sync.WaitGroup
	var mu sync.Mutex
	counts := map[int]int{}
	var firstErr error
	barrier := make(chan struct{})

	run := func(worker int, c *pgx.Conn) {
		defer wg.Done()
		<-barrier // both wait, then fire together
		ids, err := claimOnce(ctx, c, 1)
		mu.Lock()
		if err != nil && firstErr == nil {
			firstErr = err
		}
		counts[worker] = len(ids)
		mu.Unlock()
	}
	wg.Add(2)
	go run(1, c1)
	go run(2, c2)
	close(barrier)
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("claim error: %v", firstErr)
	}
	total := counts[1] + counts[2]
	if total != 1 {
		t.Fatalf("exactly-one-winner violated: worker1=%d worker2=%d total=%d, want 1", counts[1], counts[2], total)
	}

	// Post-state: the job is 'assigned' exactly once (not double-claimed, not left queued).
	var post string
	if err := seed.QueryRow(ctx, "SELECT state FROM send_managed.jobs WHERE id=$1", jobID).Scan(&post); err != nil {
		t.Fatalf("post state: %v", err)
	}
	if post != "assigned" {
		t.Fatalf("job state = %s after claim, want assigned", post)
	}
}

// TestT1b_NWorkersMJobsEachClaimedOnce — K concurrent workers drain M queued jobs; every job
// is claimed exactly once (no duplicates, none lost). Generalizes the exactly-once guarantee.
func TestT1b_NWorkersMJobsEachClaimedOnce(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "t1bclaim")
	defer cleanup()

	seed := Connect(t, cloneDSN)
	uid := SeedUser(t, seed, "t1b@example.com")
	const M = 20
	const K = 4
	want := map[string]bool{}
	for i := 0; i < M; i++ {
		id := InsertQueuedJob(t, seed, uid, uid, "rcpt-t1b@example.com")
		want[id] = true
	}

	conns := make([]*pgx.Conn, K)
	for i := range conns {
		conns[i] = Connect(t, cloneDSN)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	claimed := map[string]int{} // job id -> number of workers that claimed it
	barrier := make(chan struct{})

	worker := func(c *pgx.Conn) {
		defer wg.Done()
		<-barrier
		for {
			ids, err := claimOnce(ctx, c, 3)
			if err != nil {
				mu.Lock()
				claimed["__err__"]++
				mu.Unlock()
				return
			}
			if len(ids) == 0 {
				return // drained
			}
			mu.Lock()
			for _, id := range ids {
				claimed[id]++
			}
			mu.Unlock()
		}
	}
	wg.Add(K)
	for i := 0; i < K; i++ {
		go worker(conns[i])
	}
	close(barrier)
	wg.Wait()

	if claimed["__err__"] > 0 {
		t.Fatalf("worker errors during concurrent claim: %d", claimed["__err__"])
	}
	// Every job claimed exactly once; no id claimed twice; no extra ids.
	for id := range want {
		if claimed[id] != 1 {
			t.Fatalf("job %s claimed %d times, want exactly 1", id, claimed[id])
		}
	}
	total := 0
	for id, n := range claimed {
		if id == "__err__" {
			continue
		}
		if n != 1 {
			t.Fatalf("job %s claimed %d times (duplicate claim)", id, n)
		}
		total += n
	}
	if total != M {
		t.Fatalf("claimed %d jobs total, want %d", total, M)
	}

	// Post-state: all M jobs are 'assigned'.
	var assignedCount int
	if err := seed.QueryRow(ctx, "SELECT count(*) FROM send_managed.jobs WHERE state='assigned'").Scan(&assignedCount); err != nil {
		t.Fatalf("count assigned: %v", err)
	}
	if assignedCount != M {
		t.Fatalf("%d jobs assigned, want %d", assignedCount, M)
	}
}

// TestT2_EventPerTransition — drive queued->assigned->sending->sent; assert EXACTLY 4 events
// (genesis + 3 transitions), each with a unique seq, in chronological order.
func TestT2_EventPerTransition(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "t2events")
	defer cleanup()

	c := Connect(t, cloneDSN)
	uid := SeedUser(t, c, "t2@example.com")
	poolID := SeedSharedPool(t, c, uid, "t2-pool")
	workerID := SeedWorker(t, c, uid)                               // real worker for provenance propagation
	acctID := SeedAccount(t, c, uid, poolID)                        // real account for provenance propagation
	jobID := InsertQueuedJob(t, c, uid, uid, "rcpt-t2@example.com") // genesis event written

	// Anti-vacuity: exactly 1 event (genesis) right after creation.
	if got := eventCount(t, c, jobID); got != 1 {
		t.Fatalf("after insert: %d events, want 1 (genesis)", got)
	}

	// Each transition in its own tx with the actor GUC (sanctioned path). The assigned
	// transition sets a REAL assigned_box_id so we can assert provenance propagation below.
	transition(t, c, jobID, "dispatcher",
		"UPDATE send_managed.jobs SET state='assigned', assigned_box_id='"+workerID+"', lease_expires_at=NOW()+INTERVAL '120 seconds' WHERE id=$1")
	if got := eventCount(t, c, jobID); got != 2 {
		t.Fatalf("after assigned: %d events, want 2", got)
	}
	// Provenance: the assigned event captured the worker the job was assigned to (04-event-log.md
	// "complete provenance"). This guards against a trigger refactor dropping the box_id copy.
	var evWorker string
	if err := c.QueryRow(ctx, "SELECT box_id::text FROM send_managed.job_events WHERE job_id=$1 AND to_state='assigned'", jobID).Scan(&evWorker); err != nil {
		t.Fatalf("read assigned event box_id: %v", err)
	}
	if evWorker != workerID {
		t.Fatalf("assigned event box_id=%s, want %s (provenance not propagated)", evWorker, workerID)
	}
	transition(t, c, jobID, "box",
		"UPDATE send_managed.jobs SET state='sending', assigned_account_id='"+acctID+"' WHERE id=$1")
	if got := eventCount(t, c, jobID); got != 3 {
		t.Fatalf("after sending: %d events, want 3", got)
	}
	// Provenance: the sending event captured the account.
	var evAcct string
	if err := c.QueryRow(ctx, "SELECT account_id::text FROM send_managed.job_events WHERE job_id=$1 AND to_state='sending'", jobID).Scan(&evAcct); err != nil {
		t.Fatalf("read sending event account_id: %v", err)
	}
	if evAcct != acctID {
		t.Fatalf("sending event account_id=%s, want %s (provenance not propagated)", evAcct, acctID)
	}
	transition(t, c, jobID, "box",
		"UPDATE send_managed.jobs SET state='sent', provider_message_id='250-OK-t2', lease_expires_at=NULL, completed_at=NOW() WHERE id=$1")
	if got := eventCount(t, c, jobID); got != 4 {
		t.Fatalf("after sent: %d events, want 4", got)
	}

	// Chronological correctness + unique seq.
	rows, err := c.Query(ctx,
		"SELECT COALESCE(from_state::text,'NULL'), to_state::text, actor::text, seq FROM send_managed.job_events WHERE job_id=$1 ORDER BY seq", jobID)
	if err != nil {
		t.Fatalf("query events: %v", err)
	}
	defer rows.Close()
	type ev struct{ from, to, actor string }
	var got []ev
	seqs := map[int64]bool{}
	for rows.Next() {
		var e ev
		var seq int64
		if err := rows.Scan(&e.from, &e.to, &e.actor, &seq); err != nil {
			t.Fatal(err)
		}
		if seqs[seq] {
			t.Fatalf("duplicate seq %d", seq)
		}
		seqs[seq] = true
		got = append(got, e)
	}
	want := []ev{
		{"NULL", "queued", "system"},
		{"queued", "assigned", "dispatcher"},
		{"assigned", "sending", "box"},
		{"sending", "sent", "box"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestT3_NoStateChangeWithoutEvent — a raw state UPDATE with NO actor GUC is REJECTED by the
// trigger and the state does not move; the paired positive control (WITH the GUC) succeeds and
// writes exactly one event. Proves state cannot move silently.
func TestT3_NoStateChangeWithoutEvent(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "t3guard")
	defer cleanup()

	c := Connect(t, cloneDSN)
	uid := SeedUser(t, c, "t3@example.com")
	jobID := InsertQueuedJob(t, c, uid, uid, "rcpt-t3@example.com")

	// NEGATIVE: bypass attempt — state change with NO actor GUC in a fresh tx.
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	_, execErr := tx.Exec(ctx, "UPDATE send_managed.jobs SET state='assigned', lease_expires_at=NOW()+INTERVAL '120 seconds' WHERE id=$1", jobID)
	_ = tx.Rollback(ctx)
	if execErr == nil {
		t.Fatal("expected the no-actor UPDATE to be rejected by the trigger, got nil error")
	}

	// State unchanged (still queued), event count still 1 (genesis only).
	var state string
	if err := c.QueryRow(ctx, "SELECT state FROM send_managed.jobs WHERE id=$1", jobID).Scan(&state); err != nil {
		t.Fatalf("state after reject: %v", err)
	}
	if state != "queued" {
		t.Fatalf("state moved to %s despite trigger reject, want queued", state)
	}
	if got := eventCount(t, c, jobID); got != 1 {
		t.Fatalf("event count %d after rejected update, want 1 (genesis only)", got)
	}

	// POSITIVE control: same UPDATE WITH the actor GUC succeeds and writes exactly one event.
	transition(t, c, jobID, "dispatcher",
		"UPDATE send_managed.jobs SET state='assigned', lease_expires_at=NOW()+INTERVAL '120 seconds' WHERE id=$1")
	if err := c.QueryRow(ctx, "SELECT state FROM send_managed.jobs WHERE id=$1", jobID).Scan(&state); err != nil {
		t.Fatalf("state after control: %v", err)
	}
	if state != "assigned" {
		t.Fatalf("positive control did not apply: state=%s, want assigned", state)
	}
	if got := eventCount(t, c, jobID); got != 2 {
		t.Fatalf("event count %d after sanctioned update, want 2", got)
	}
}

// --- helpers ---

func requireTemplate(t *testing.T) string {
	t.Helper()
	tpl := templateDB()
	if tpl == "" {
		t.Skip("TEST_TEMPLATE_DB not set")
	}
	return tpl
}

func templateDB() string {
	return getenv("TEST_TEMPLATE_DB")
}

func eventCount(t *testing.T, c *pgx.Conn, jobID string) int {
	t.Helper()
	var n int
	if err := c.QueryRow(context.Background(), "SELECT count(*) FROM send_managed.job_events WHERE job_id=$1", jobID).Scan(&n); err != nil {
		t.Fatalf("event count: %v", err)
	}
	return n
}

// transition runs a state-changing UPDATE with the actor GUC set, in one tx (so the trigger
// sees the actor), and fails the test on error.
func transition(t *testing.T, c *pgx.Conn, jobID, actor, updateSQL string) {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('send_managed.actor', $1, true)", actor); err != nil {
		t.Fatalf("set actor: %v", err)
	}
	if _, err := tx.Exec(ctx, updateSQL, jobID); err != nil {
		t.Fatalf("transition (%s): %v", actor, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit transition: %v", err)
	}
}
