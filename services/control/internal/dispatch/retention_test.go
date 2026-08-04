package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// retention_test.go — Phase 7 §6 T3 (debug-log rollup/retention) + T5 part 1 (the P7-11 config
// ordering invariant). REAL infra: every test runs against a real Postgres clone of send_managed_template
// through the real send_managed_app pool, so the DELETE privilege, the write-once trigger, the RLS
// policy and the FK/CASCADE behavior are the real ones — nothing is mocked or asserted from a comment.

// seedSendLog inserts one raw send_logs row for an existing job with an EXPLICIT ttl_expires_at
// offset from NOW(), so a test can create genuinely-expired and genuinely-live raw content without
// waiting out debug_log_retention_days. Inserted as the ADMIN role deliberately: the sweep under test
// is the DELETE side, and using admin here keeps the fixture independent of the grant being proven.
func (e *dispatchEnv) seedSendLog(t *testing.T, jobID string, ttlOffset time.Duration) string {
	t.Helper()
	var id string
	if err := e.admin.QueryRow(context.Background(),
		`INSERT INTO send_managed.send_logs
		   (job_id, user_id, recipient_email, subject, body, headers, smtp_response, ttl_expires_at)
		 SELECT j.id, j.user_id, j.recipient_email, 'raw subject', 'raw body',
		        '{"X-Test":"1"}'::jsonb, '250 2.0.0 OK', NOW() + make_interval(secs => $2)
		   FROM send_managed.jobs j WHERE j.id = $1
		 RETURNING id`, jobID, int(ttlOffset.Seconds())).Scan(&id); err != nil {
		t.Fatalf("seed send_log: %v", err)
	}
	return id
}

func (e *dispatchEnv) sendLogTotal(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(context.Background(), `SELECT count(*) FROM send_managed.send_logs`).Scan(&n); err != nil {
		t.Fatalf("send_logs total: %v", err)
	}
	return n
}

func (e *dispatchEnv) sendLogExists(t *testing.T, id string) bool {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(context.Background(), `SELECT count(*) FROM send_managed.send_logs WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatalf("send_log exists: %v", err)
	}
	return n == 1
}

// eventTotal / eventFingerprint capture the PERMANENT ledger so a test can prove the sweep did not
// touch it. The fingerprint is every (job, from→to, actor, class) tuple concatenated — a byte-level
// witness, not just a count, so a delete-and-reinsert would also be caught.
func (e *dispatchEnv) eventFingerprint(t *testing.T) (int, string) {
	t.Helper()
	var n int
	var fp string
	if err := e.admin.QueryRow(context.Background(),
		`SELECT count(*), COALESCE(string_agg(job_id::text||':'||COALESCE(from_state::text,'-')||'>'||to_state::text||':'||actor::text||':'||COALESCE(error_class::text,'-'), '|' ORDER BY created_at, id), '')
		   FROM send_managed.job_events`).Scan(&n, &fp); err != nil {
		t.Fatalf("event fingerprint: %v", err)
	}
	return n, fp
}

// TestRetention_SweepDropsExpiredRawInBoundedBatchesAndKeepsOutcomes is T3 (§6). It proves the four
// locked properties of §3/§3.1/§3.2 at once:
//
//	(1) EXPIRED raw send_logs content is DELETED — and by the send_managed_app app role, so the ADR-0008
//	    grant trap is genuinely closed (CONFIRMED, not assumed: send_managed_0012's
//	    `GRANT SELECT, INSERT, DELETE ON send_managed.send_logs` is what makes this pass).
//	(2) It happens in BOUNDED BATCHES: with retention_delete_batch_size=2 and 7 expired rows, the
//	    sweep issues multiple bounded DELETEs and drains — a single unbounded DELETE would be a
//	    long-lock hazard (§3.1).
//	(3) ANTI-VACUITY (the paired negative control): a NOT-yet-expired row in the SAME table survives,
//	    so the test cannot pass by deleting everything. A permanently-retained job's outcome ledger
//	    (job_events) and final jobs state are byte-unchanged.
//	(4) The derive-on-demand per-day summary (§3.2, P7-1 schema-neutral — NO summary table) returns
//	    the SAME outcome counts BEFORE and AFTER the raw drop, because it aggregates the permanent
//	    job_events. That is the whole point of the schema-neutral choice: outcomes are permanent,
//	    only raw content is dropped.
func TestRetention_SweepDropsExpiredRawInBoundedBatchesAndKeepsOutcomes(t *testing.T) {
	e := newDispatchEnv(t, "retdrop")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "s3cr3t")

	// A real job driven to a real terminal outcome, so job_events holds real transitions
	// (queued→assigned→sending→sent) and send_logs holds the real raw row the send path wrote.
	// Drive the real send path with debug_log_retention_days = 0 (config-not-code) so the raw row the
	// send path itself writes is born AT its retention boundary and is genuinely expired a moment later.
	// This is how the real row gets aged: send_logs is write-once and its BEFORE UPDATE reject trigger
	// fires for EVERY role including the table owner, so no fixture may UPDATE ttl_expires_at — which is
	// itself the immutability property holding. The handler must be rebuilt to see the changed key
	// (NewOutcomeHandler captures the Config it is given).
	e.setConfigInt(t, "debug_log_retention_days", 0)
	oh := NewOutcomeHandler(e.pool, e.mustLoadConfig(t))

	job := e.seedQueuedJob(t, "retention@example.com")
	e.dispatchToSending(t, job)
	if !oh.Handle(e.px.workerID, workerproto.Report{
		Type: workerproto.TypeReport, JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "ret-pmid-0001", SMTPResponse: "250 2.0.0 OK queued as ret-pmid-0001",
	}) {
		t.Fatal("outcome not acked — precondition for the retention test failed")
	}
	state, hasPMID, _, completed, _ := e.jobState(t, job)
	if state != "sent" || !hasPMID || !completed {
		t.Fatalf("precondition: job state=%s hasPMID=%v completed=%v, want sent/true/true", state, hasPMID, completed)
	}
	if n := e.sendLogCount(t, job); n != 1 {
		t.Fatalf("precondition: %d send_logs for the sent job, want 1 (the real send path's raw row)", n)
	}

	var userID string
	if err := e.admin.QueryRow(ctx, `SELECT user_id FROM send_managed.jobs WHERE id=$1`, job).Scan(&userID); err != nil {
		t.Fatalf("read job user: %v", err)
	}

	// Derive the summary BEFORE any sweep: this is the baseline the post-sweep derivation must match.
	cfg := e.mustLoadConfig(t)
	from, to := time.Now().Add(-24*time.Hour), time.Now().Add(24*time.Hour)
	before, err := e.d.SummarizeByPeriod(ctx, cfg, userID, from, to)
	if err != nil {
		t.Fatalf("SummarizeByPeriod (before sweep): %v", err)
	}
	beforeSent := summaryStateCount(before, "sent")
	if beforeSent != 1 {
		t.Fatalf("derived summary before sweep counts %d sent transitions, want 1 (derivation is broken, not the sweep)", beforeSent)
	}

	// The real row is already at its boundary (retention_days=0 ⇒ ttl_expires_at = its NOW()); confirm it
	// has actually crossed rather than assuming clock progress.
	deadline := time.Now().Add(5 * time.Second)
	for {
		n, err := e.d.countExpiredSendLogs(ctx)
		if err != nil {
			t.Fatalf("count expired: %v", err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the send path's own raw row never became expired (count=%d) with debug_log_retention_days=0", n)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Add 6 more already-expired rows so the sweep must run MULTIPLE bounded batches
	// (7 expired rows / batch 2 → 2,2,2,1 then drained).
	for i := 0; i < 6; i++ {
		e.seedSendLog(t, job, -time.Duration(i+1)*time.Hour)
	}
	// ANTI-VACUITY CONTROL: raw content that is NOT yet expired, in the same table, same job.
	live := e.seedSendLog(t, job, +72*time.Hour)

	if total := e.sendLogTotal(t); total != 8 {
		t.Fatalf("precondition: %d send_logs rows, want 8 (7 expired + 1 live)", total)
	}
	if n, err := e.d.countExpiredSendLogs(ctx); err != nil || n != 7 {
		t.Fatalf("precondition: countExpiredSendLogs=%d err=%v, want 7", n, err)
	}

	// Capture the PERMANENT ledger + final job row before the sweep.
	evBefore, fpBefore := e.eventFingerprint(t)
	if evBefore == 0 {
		t.Fatal("precondition: no job_events at all — the ledger witness would be vacuous")
	}

	// Bound the batch to 2 so the sweep MUST loop. This is the seeded dial, not a code constant.
	e.setConfigInt(t, "retention_delete_batch_size", 2)

	deleted, batches, err := e.d.sweepExpiredSendLogs(ctx, e.mustLoadConfig(t))
	if err != nil {
		t.Fatalf("retention sweep: %v", err)
	}
	if deleted != 7 {
		t.Fatalf("sweep deleted %d rows, want exactly 7 (the expired ones)", deleted)
	}
	// (2) OBSERVED batching: 7 rows at 2 per batch is 4 statements (2,2,2,1). Asserting the count —
	// not just the total — is what proves the DELETE is genuinely bounded rather than one big statement.
	if batches != 4 {
		t.Fatalf("sweep issued %d DELETE batches for 7 expired rows at batch size 2, want 4 (2,2,2,1) — the DELETE must be bounded by retention_delete_batch_size (§3.1)", batches)
	}

	// (1)+(2): expired raw content is gone, deleted by the module role in bounded batches.
	if n, err := e.d.countExpiredSendLogs(ctx); err != nil || n != 0 {
		t.Fatalf("after sweep countExpiredSendLogs=%d err=%v, want 0", n, err)
	}
	// (3) ANTI-VACUITY: the not-yet-expired row SURVIVED.
	if !e.sendLogExists(t, live) {
		t.Fatal("the NOT-yet-expired send_log was deleted — the sweep is not respecting ttl_expires_at (anti-vacuity control failed)")
	}
	if total := e.sendLogTotal(t); total != 1 {
		t.Fatalf("%d send_logs remain, want exactly 1 (the live one)", total)
	}

	// (3) OUTCOMES ARE PERMANENT: the ledger is byte-identical and the job's final row is untouched.
	evAfter, fpAfter := e.eventFingerprint(t)
	if evAfter != evBefore || fpAfter != fpBefore {
		t.Fatalf("job_events changed across the sweep: %d→%d rows; fingerprint changed=%v — outcomes must be PERMANENT (§3.2)",
			evBefore, evAfter, fpAfter != fpBefore)
	}
	state2, hasPMID2, _, completed2, _ := e.jobState(t, job)
	if state2 != state || hasPMID2 != hasPMID || completed2 != completed {
		t.Fatalf("final job row changed across the sweep: state %s→%s pmid %v→%v completed %v→%v",
			state, state2, hasPMID, hasPMID2, completed, completed2)
	}

	// (4) The derived summary is UNCHANGED after the raw drop — it comes from job_events, not send_logs.
	after, err := e.d.SummarizeByPeriod(ctx, cfg, userID, from, to)
	if err != nil {
		t.Fatalf("SummarizeByPeriod (after sweep): %v", err)
	}
	if got := summaryStateCount(after, "sent"); got != beforeSent {
		t.Fatalf("derived summary after sweep counts %d sent transitions, want %d (same as before) — the summary must survive the raw drop (§3.2)", got, beforeSent)
	}
	if len(after) == 0 {
		t.Fatal("derived summary is empty after the sweep — a swept day must still summarize (§3.2)")
	}
	// Buckets are day-truncated at the seeded rollup_granularity=day.
	if cfg.RollupGranularity != "day" {
		t.Fatalf("seeded rollup_granularity=%q, want day (send_managed_0019 seed drifted)", cfg.RollupGranularity)
	}
	for _, b := range after {
		if !b.Bucket.Equal(b.Bucket.Truncate(0)) || b.Total == 0 {
			t.Fatalf("bucket %v total=%d is malformed", b.Bucket, b.Total)
		}
	}

	// A second sweep is a clean no-op (idempotent; nothing left expired).
	_, again, err := e.d.RetentionTick(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again != 0 {
		t.Fatalf("second sweep deleted %d rows, want 0 (idempotent)", again)
	}
	if total := e.sendLogTotal(t); total != 1 {
		t.Fatalf("second sweep changed the live row count to %d, want 1", total)
	}
}

func summaryStateCount(buckets []DaySummary, state string) int {
	n := 0
	for _, b := range buckets {
		n += b.ByState[state]
	}
	return n
}

// TestRetention_ModuleRoleCanDeleteButStillCannotUpdate is the grant confirmation the phase demands,
// stated as behavior rather than as a claim about a migration file. POSITIVE: the send_managed_app role
// CAN DELETE a send_logs row (so the sweep is not silently a no-op behind a missing grant — the
// ADR-0008 trap). NEGATIVE control: the same role STILL cannot UPDATE one, so send_logs remains
// write-once and Phase 7 did not weaken the immutability property to get its DELETE.
func TestRetention_ModuleRoleCanDeleteButStillCannotUpdate(t *testing.T) {
	e := newDispatchEnv(t, "retgrant")
	ctx := context.Background()
	job := e.seedQueuedJob(t, "grant@example.com")
	doomed := e.seedSendLog(t, job, -time.Hour)
	keep := e.seedSendLog(t, job, +time.Hour)

	// POSITIVE: the app role's own DELETE works.
	tag, err := e.pool.Exec(ctx, `DELETE FROM send_managed.send_logs WHERE id=$1`, doomed)
	if err != nil {
		t.Fatalf("send_managed_app DELETE on send_logs failed (%v) — the retention sweep would be a silent no-op; grants are the ADR-0008 trap", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("send_managed_app DELETE affected %d rows, want 1 (RLS may be filtering it away)", tag.RowsAffected())
	}
	if e.sendLogExists(t, doomed) {
		t.Fatal("row still present after a reportedly-successful DELETE")
	}

	// NEGATIVE CONTROL: write-once still holds — UPDATE is refused (REVOKE + BEFORE UPDATE trigger).
	if _, err := e.pool.Exec(ctx, `UPDATE send_managed.send_logs SET subject='tampered' WHERE id=$1`, keep); err == nil {
		t.Fatal("send_managed_app UPDATE on send_logs SUCCEEDED — send_logs must stay write-once (Phase 7 must not weaken it to gain DELETE)")
	}
	if e.sendLogSubject(t, keep) != "raw subject" {
		t.Fatal("send_logs row was mutated despite the write-once guard")
	}
}

func (e *dispatchEnv) sendLogSubject(t *testing.T, id string) string {
	t.Helper()
	var s string
	if err := e.admin.QueryRow(context.Background(), `SELECT subject FROM send_managed.send_logs WHERE id=$1`, id).Scan(&s); err != nil {
		t.Fatalf("read send_log subject: %v", err)
	}
	return s
}

// TestConfigOrdering_RejectsShortOutcomeBufferTTL is T5 part 1 (§6) — the P7-11 invariant enforced at
// config-LOAD with a REJECT, never a clamp (CLAUDE.md Rule 1). If box_outcome_buffer_ttl_sec were
// shorter than startup_reconnect_window_sec, a worker reconnecting late-but-in-window would have already
// DROPPED its buffered outcome → the job gets lease-reclaimed → RE-SENT → silent duplicate. A refused
// boot is loud and instantly fixable; that duplicate is silent and unrecoverable.
//
// The invariant is NON-STRICT (§5.3 states `≥`), so the equality case must LOAD — that is asserted
// here as a distinct case, not assumed, because a `>` typo would still pass a reject-only test.
func TestConfigOrdering_RejectsShortOutcomeBufferTTL(t *testing.T) {
	e := newDispatchEnv(t, "ordbuf")
	ctx := context.Background()

	// The seeded pair (900 >= 120) is safe: loads clean. Paired positive control.
	cfg, err := LoadConfig(ctx, e.pool)
	if err != nil {
		t.Fatalf("seeded config pair failed to load: %v", err)
	}
	if cfg.WorkerOutcomeBufferTTLSec != 900 || cfg.StartupReconnectWindowSec != 120 {
		t.Fatalf("seeded pair is buffer=%d window=%d, want 900/120 (send_managed_0019 seed drifted)",
			cfg.WorkerOutcomeBufferTTLSec, cfg.StartupReconnectWindowSec)
	}

	// VIOLATION: widen the rescan window past the buffer TTL by ONE second.
	e.setConfigInt(t, "startup_reconnect_window_sec", 901)
	got, err := LoadConfig(ctx, e.pool)
	if err == nil {
		t.Fatal("LoadConfig ACCEPTED box_outcome_buffer_ttl_sec < startup_reconnect_window_sec — must reject (P7-11, Rule 1)")
	}
	if err.Error() != "config_ordering_violation: box_outcome_buffer_ttl_sec (900) must be >= startup_reconnect_window_sec (901)" {
		t.Fatalf("unexpected error: %v", err)
	}
	// REJECT, NOT CLAMP: the returned Config is zeroed, so no caller can accidentally run on a
	// silently-corrected pair.
	if got.StartupReconnectWindowSec != 0 || got.WorkerOutcomeBufferTTLSec != 0 {
		t.Fatalf("rejected load returned a populated Config (window=%d buffer=%d) — a reject must not hand back usable values",
			got.StartupReconnectWindowSec, got.WorkerOutcomeBufferTTLSec)
	}
	// And the stored config was NOT auto-corrected (Rule 1: reject, never clamp).
	if v := e.configInt(t, "startup_reconnect_window_sec"); v != 901 {
		t.Fatalf("stored startup_reconnect_window_sec is %d, want 901 — LoadConfig must not mutate config to fix it", v)
	}

	// EQUALITY loads (the invariant is `>=`, not `>`): the theoretical floor is allowed; the ~2×
	// margin is an operator GUIDELINE (§5.3), deliberately not a computed key.
	e.setConfigInt(t, "startup_reconnect_window_sec", 900)
	if _, err := LoadConfig(ctx, e.pool); err != nil {
		t.Fatalf("LoadConfig rejected the EQUAL pair (900 >= 900) — the invariant is non-strict: %v", err)
	}

	// Raising the buffer TTL also fixes it (proves it is the RELATION being checked, not one key).
	e.setConfigInt(t, "startup_reconnect_window_sec", 901)
	e.setConfigInt(t, "box_outcome_buffer_ttl_sec", 1800)
	if _, err := LoadConfig(ctx, e.pool); err != nil {
		t.Fatalf("LoadConfig rejected a safe pair after raising the buffer TTL (1800 >= 901): %v", err)
	}

	// The OTHER invariant is still enforced independently (P6-11) — one reject does not mask the other.
	e.setConfigInt(t, "smtp_operation_timeout_sec", 999)
	if _, err := LoadConfig(ctx, e.pool); err == nil {
		t.Fatal("LoadConfig accepted an inverted smtp/lease pair while the buffer pair was safe — both invariants must hold")
	}
}

func (e *dispatchEnv) configInt(t *testing.T, key string) int {
	t.Helper()
	var v int
	if err := e.admin.QueryRow(context.Background(),
		`SELECT (value->>'v')::int FROM send_managed.config WHERE key=$1`, key).Scan(&v); err != nil {
		t.Fatalf("read config %s: %v", key, err)
	}
	return v
}

// TestRetention_SweepPausesUnderOutageAndInventsNothing ties the sweep to the Chunk-3 §2 gate: while
// the DB is unreachable the retention loop PAUSES on bounded backoff (it does not crash-loop, and it
// certainly does not fabricate a deletion it never performed), and on recovery it drops the backlog.
// Retention is a CEILING on raw-content lifetime, not a deadline, so pausing is the correct behavior.
func TestRetention_SweepPausesUnderOutageAndInventsNothing(t *testing.T) {
	e := newDispatchEnv(t, "retoutage")
	ctx := context.Background()
	job := e.seedQueuedJob(t, "retoutage@example.com")
	e.seedSendLog(t, job, -time.Hour)
	e.seedSendLog(t, job, -2*time.Hour)

	proxy, proxyDSN := startPGProxy(t, e.moduleDSN)
	ppool := proxiedPool(t, proxyDSN)
	pd := NewDispatcher(ppool, e.dec, e.px)
	rr := NewRetentionRunner(pd)

	proxy.cut()
	// One tick with the DB down: it must return a backoff wait, not panic, and delete nothing.
	wait := rr.tick(ctx)
	if wait <= 0 || wait > outageBackoffMax {
		t.Fatalf("retention tick under outage returned wait=%s, want a bounded backoff in (0, %s]", wait, outageBackoffMax)
	}
	if !pd.health.Halted() {
		t.Fatal("retention tick under outage did not trip the shared outage gate")
	}
	if n := e.sendLogTotal(t); n != 2 {
		t.Fatalf("%d send_logs after a sweep tick during the outage, want 2 untouched (the sweep must invent nothing)", n)
	}

	// Recovery: the very next tick drops the backlog and returns the normal seeded cadence.
	proxy.restore()
	deadline := time.Now().Add(30 * time.Second)
	for {
		wait = rr.tick(ctx)
		if !pd.health.Halted() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retention loop never resumed after the DB came back")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if n := e.sendLogTotal(t); n != 0 {
		t.Fatalf("%d expired send_logs remain after recovery, want 0 (the backlog must drain on resume)", n)
	}
	if want := time.Duration(e.cfg.RetentionSweepIntervalMS) * time.Millisecond; wait != want {
		t.Fatalf("post-recovery tick wait=%s, want the seeded retention_sweep_interval_ms (%s)", wait, want)
	}
}
