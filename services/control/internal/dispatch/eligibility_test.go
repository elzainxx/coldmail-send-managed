package dispatch

// Chunk 2 behavioral tests: account eligibility + the atomic consume (§2/§3/§7) — THE
// double-booking-prevention core. Real Postgres, GENUINE concurrency (multiple real pgx
// connections racing the same account row) — mocking this is disallowed (spec §9). Anti-vacuity:
// every guard test carries a paired positive/negative control, and the concurrency test asserts
// over-subscription (demand > capacity) as a precondition, since under-subscribed demand could
// never over-send even with a broken guard.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// setAccountDailyState directly overwrites an account's daily-limit bookkeeping (daily_limit,
// daily_sent_count, daily_reset_day, paused, pause_reason) for a test fixture. Bypasses the
// dispatcher entirely — this is fixture setup, not the code under test.
func (e *dispatchEnv) setAccountDailyState(t *testing.T, acctID string, limit, sent int, resetDay string, paused bool, reason *string) {
	t.Helper()
	if _, err := e.admin.Exec(context.Background(),
		`UPDATE send_managed.accounts
		   SET daily_limit=$2, daily_sent_count=$3, daily_reset_day=$4::date, paused=$5, pause_reason=$6
		 WHERE id=$1`, acctID, limit, sent, resetDay, paused, reason); err != nil {
		t.Fatalf("setAccountDailyState: %v", err)
	}
}

func (e *dispatchEnv) setAccountFlags(t *testing.T, acctID string, quarantined, disabled bool) {
	t.Helper()
	if _, err := e.admin.Exec(context.Background(),
		`UPDATE send_managed.accounts SET quarantined=$2, disabled=$3 WHERE id=$1`, acctID, quarantined, disabled); err != nil {
		t.Fatalf("setAccountFlags: %v", err)
	}
}

func (e *dispatchEnv) accountDailySentCount(t *testing.T, acctID string) int {
	t.Helper()
	var n int
	if err := e.admin.QueryRow(context.Background(),
		`SELECT daily_sent_count FROM send_managed.accounts WHERE id=$1`, acctID).Scan(&n); err != nil {
		t.Fatalf("accountDailySentCount: %v", err)
	}
	return n
}

func strPtr(s string) *string { return &s }

// TestAccountConsume_NeverExceedsDailyLimit (T1, THE test) — ONE account, daily_limit=50, hammered
// by 8 GENUINE concurrent connections (8 separate pgxpool.New pools, so 8 independent physical
// connections — not 8 goroutines sharing one pooled connection) each trying to consume it
// repeatedly, with demand far exceeding capacity. Asserts: (1) daily_sent_count ends at EXACTLY 50,
// never more; (2) exactly 50 attempts observed a successful consume; (3) once capacity is gone,
// EVERY further attempt is refused — the 51st send never happens (the confirm phase below); (4) NO
// attempt ever surfaces accounts_daily_sent_check's SQLSTATE 23514 — the CHECK is the last-resort
// invariant and must NEVER fire in normal operation (a fire would mean SKIP LOCKED + the MVCC
// re-check failed to absorb the contention, a genuine finding, not something to paper over —
// CLAUDE.md closeout instruction, 2026-07-24). Anti-vacuity: asserts observed demand > capacity,
// that concurrency was genuinely exercised (more than one worker succeeded), AND — the strongest
// check — samples pg_stat_activity DURING the race and asserts at least 2 workers were observed
// simultaneously inside an active query against this account's row, proving overlap in wall-clock
// time, not just "multiple workers eventually ran."
//
// FIXTURE SHAPE (Phase 7 Chunk 6 fix). Two earlier flakes — "only 1 worker ever succeeded" in
// isolation and "consumes < 50" under full-suite connection pressure — were both the FIXTURE, not
// the invariant (an under-count and a single-winner never risk over-consumption). Cause: each
// worker used to open its pgxpool INSIDE the race, so the first worker to finish its TCP+TLS+auth
// handshake could drain all 50 slots while its peers were still connecting, and a fixed per-worker
// attempt budget could then be spent entirely on SKIP LOCKED skips. Both are fixed structurally
// rather than by loosening an assertion:
//   - every pool is created AND Ping()ed (forcing a real physical connection) BEFORE the race, and
//     all 8 workers are released together by a start barrier, so contention is simultaneous by
//     construction instead of by luck;
//   - the fixed budget is replaced by race-to-capacity (bounded by a hard attempt cap and a
//     wall-clock deadline, both safety bounds — not tuning dials) followed by a CONFIRM phase in
//     which every worker attempts again and must be refused. That is a STRONGER assertion than the
//     old budget: it proves the 51st consume is impossible, rather than hoping the budget was big
//     enough to fill 50.
func TestAccountConsume_NeverExceedsDailyLimit(t *testing.T) {
	e := newDispatchEnv(t, "nolimit")
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	const limit = 50
	e.setAccountDailyState(t, acct, limit, 0, todayUTC(), false, nil)

	const workers = 8
	// Safety bounds on the race, NOT tuning dials: the race ends when capacity is exhausted. These
	// only stop a broken implementation from spinning forever. maxAttemptsPerWorker is far above the
	// number of attempts a healthy run needs (skips are cheap, so a worker burns a few hundred at
	// most); raceDeadline is the wall-clock backstop.
	const maxAttemptsPerWorker = 4000
	const confirmAttemptsPerWorker = 5
	const raceDeadline = 60 * time.Second

	moduleDSN := poolDSN(t, e)

	// Pre-connect ALL workers before any of them races. Ping() forces the pool to establish a real
	// physical connection (handshake + auth complete), so no worker is still connecting while
	// another is already consuming — the cause of the single-winner flake.
	dispatchers := make([]*Dispatcher, workers)
	for w := 0; w < workers; w++ {
		p, err := pgxpool.New(context.Background(), moduleDSN)
		if err != nil {
			t.Fatalf("worker %d pool: %v", w, err)
		}
		defer p.Close()
		if err := p.Ping(context.Background()); err != nil {
			t.Fatalf("worker %d ping (pool must hold a live physical connection BEFORE the race): %v", w, err)
		}
		dispatchers[w] = NewDispatcher(p, e.dec, e.px)
	}

	// Concurrency-overlap sampler: while the 8 workers race, poll pg_stat_activity (via the ADMIN
	// connection, a 9th connection distinct from the 8 workers') for how many of THIS test's own
	// backends are simultaneously mid-query. This is the spec's own §9 T1 anti-vacuity requirement
	// — proving genuine time-overlap, not inferring it from an aggregate success count.
	sampleCtx, cancelSample := context.WithCancel(context.Background())
	var maxConcurrentActive int64
	var samplerWG sync.WaitGroup
	samplerWG.Add(1)
	go func() {
		defer samplerWG.Done()
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
				var n int64
				if err := e.admin.QueryRow(context.Background(),
					`SELECT count(*) FROM pg_stat_activity
					 WHERE datname = current_database() AND state = 'active'
					   AND query ILIKE '%eligible_accounts%'`).Scan(&n); err == nil {
					if n > atomic.LoadInt64(&maxConcurrentActive) {
						atomic.StoreInt64(&maxConcurrentActive, n)
					}
				}
			}
		}
	}()

	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	successByWorker := map[int]int{}
	var totalAttempts int64
	var checkViolations int64 // SQLSTATE 23514 (accounts_daily_sent_check) observed count
	var otherErrors int64

	// consumeOnce runs one real accountConsume and records the outcome. Shared by the race phase and
	// the confirm phase so BOTH exercise the identical code path.
	consumeOnce := func(worker int, d *Dispatcher) (ok bool, fatal bool) {
		atomic.AddInt64(&totalAttempts, 1)
		_, ok, err := d.accountConsume(context.Background(), pool, defaultAccountOrderBy, nil, "UTC", 0, 900)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23514" {
				atomic.AddInt64(&checkViolations, 1)
				t.Errorf("worker %d: accounts_daily_sent_check FIRED (SQLSTATE 23514) — the last-resort invariant was needed, meaning SKIP LOCKED + the MVCC re-check did NOT absorb the contention: %v", worker, err)
			} else {
				atomic.AddInt64(&otherErrors, 1)
				t.Errorf("worker %d: %v", worker, err)
			}
			return false, true
		}
		if ok {
			mu.Lock()
			successes++
			successByWorker[worker]++
			mu.Unlock()
		}
		return ok, false
	}

	// The start barrier: all 8 pre-connected workers are released at the same instant, so the
	// contention on the single account row is simultaneous by CONSTRUCTION, not by scheduling luck.
	start := make(chan struct{})
	deadline := time.Now().Add(raceDeadline)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int, d *Dispatcher) {
			defer wg.Done()
			<-start
			// RACE PHASE: keep attempting until capacity is provably gone (this worker sees a run of
			// consecutive refusals with the total already at the limit), bounded by the safety caps.
			for i := 0; i < maxAttemptsPerWorker; i++ {
				ok, fatal := consumeOnce(worker, d)
				if fatal {
					return
				}
				if !ok {
					// A refusal is either contention (a peer holds the row — SKIP LOCKED) or genuine
					// exhaustion. Stop only when the aggregate has reached capacity; otherwise keep
					// racing, so skips can never starve the fill of all 50 slots.
					mu.Lock()
					done := successes >= limit
					mu.Unlock()
					if done {
						return
					}
				}
				if time.Now().After(deadline) {
					t.Errorf("worker %d hit the %s race deadline with the aggregate still below capacity — the consume is not making progress", worker, raceDeadline)
					return
				}
			}
			t.Errorf("worker %d exhausted its %d-attempt safety cap without capacity being filled — the consume is not making progress", worker, maxAttemptsPerWorker)
		}(w, dispatchers[w])
	}
	close(start)
	wg.Wait()

	// CONFIRM PHASE (the real over-consumption assertion): capacity is spent, so EVERY further
	// concurrent attempt must be refused. This is what makes "never exceeds" observed rather than
	// assumed — a guard that only worked until the budget ran out would fail here.
	successesAfterRace := successes
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(worker int, d *Dispatcher) {
			defer wg.Done()
			for i := 0; i < confirmAttemptsPerWorker; i++ {
				if ok, fatal := consumeOnce(worker, d); fatal {
					return
				} else if ok {
					t.Errorf("worker %d consumed slot #%d AFTER the daily_limit of %d was reached — OVER-CONSUMPTION", worker, successes, limit)
				}
			}
		}(w, dispatchers[w])
	}
	wg.Wait()
	cancelSample()
	samplerWG.Wait()

	if successes != successesAfterRace {
		t.Fatalf("the confirm phase landed %d extra consume(s) past the limit — the daily cap is not enforced once capacity is spent", successes-successesAfterRace)
	}
	if successes != limit {
		t.Fatalf("total successful consumes=%d, want EXACTLY %d (the daily_limit)", successes, limit)
	}
	// Anti-vacuity #0: the run must genuinely have been over-subscribed — more attempts were made
	// than there was capacity to satisfy. (Replaces the old compile-time budget precondition, which
	// could not see what actually happened.)
	if att := atomic.LoadInt64(&totalAttempts); att <= int64(limit) {
		t.Fatalf("only %d attempt(s) were made against a capacity of %d — demand did not exceed capacity, so this run could not have detected over-consumption", att, limit)
	}
	got := e.accountDailySentCount(t, acct)
	if got != limit {
		t.Fatalf("daily_sent_count=%d, want EXACTLY %d — the double-booking invariant was violated", got, limit)
	}
	// Anti-vacuity #1: genuine concurrency means more than one worker landed a success (a
	// serialized/broken-locking implementation could still pass the count check by accident if,
	// say, only one worker ever ran — this rules that out).
	if len(successByWorker) < 2 {
		t.Fatalf("only %d worker(s) ever succeeded — concurrency was not genuinely exercised", len(successByWorker))
	}
	// Anti-vacuity #2 (the spec's own §9 T1 requirement): prove overlap in WALL-CLOCK time via a
	// live pg_stat_activity sample, not just an aggregate success count that could theoretically
	// arise from 8 workers that happened never to overlap.
	observed := atomic.LoadInt64(&maxConcurrentActive)
	t.Logf("pg_stat_activity sampler observed max %d workers simultaneously active mid-query", observed)
	if observed < 2 {
		t.Fatalf("pg_stat_activity never observed >=2 workers simultaneously active (max seen: %d) — the sampler either missed the race window or the workers ran serially, not genuinely concurrently", observed)
	}
	// CHUNK 1(b) explicit finding statement: report, do not paper over, whether the CHECK fired.
	if checkViolations > 0 {
		t.Fatalf("FINDING: accounts_daily_sent_check fired %d time(s) during a NORMAL healthy run — "+
			"this is NOT expected; it means SKIP LOCKED + the MVCC re-check did not fully absorb the "+
			"contention and the CHECK was load-bearing, not last-resort", checkViolations)
	}
	if otherErrors > 0 {
		t.Fatalf("%d non-CHECK error(s) occurred during the race (see logged worker errors above)", otherErrors)
	}
	att := atomic.LoadInt64(&totalAttempts)
	t.Logf("CHUNK 1(b) result: accounts_daily_sent_check fired 0 times across %d total attempts (%d successes, %d contention/exhaustion refusals, %d workers landed a success) — confirmed last-resort, never load-bearing in this run",
		att, successes, att-int64(successes), len(successByWorker))
}

// TestAccountConsume_SkipsIneligibleAccounts (T6-style) — paused/quarantined/disabled/at-limit
// accounts are NEVER selected. Anti-vacuity: TWO healthy accounts are present (the lower-id one
// given a small daily_limit so it exhausts within the run) so a "clean result" can't be explained
// by there being only one possible target — defaultAccountOrderBy's deterministic id-ASC tiebreak
// (P4-OD-1) means the lower-id healthy account is exhausted FIRST, then the second takes over;
// both healthy accounts get real selections and no ineligible one ever does.
func TestAccountConsume_SkipsIneligibleAccounts(t *testing.T) {
	e := newDispatchEnv(t, "skip")
	pool := e.seedPool(t)

	healthy := e.seedAccount(t, pool, "ok")
	healthy2 := e.seedAccount(t, pool, "ok2")
	// account ids are random UUIDs, so determine which sorts FIRST (the deterministic id-ASC
	// tiebreak, P4-OD-1, always picks it first) and give THAT one the small limit so it exhausts
	// within the run and the second one takes over — never assume creation order == id order.
	first, second := healthy, healthy2
	if healthy2 < healthy {
		first, second = healthy2, healthy
	}
	e.setAccountDailyState(t, first, 25, 0, todayUTC(), false, nil) // exhausts within 40 draws
	e.setAccountDailyState(t, second, 1000, 0, todayUTC(), false, nil)
	pausedManual := e.seedAccount(t, pool, "pm")
	e.setAccountDailyState(t, pausedManual, 100, 0, todayUTC(), true, strPtr("manual"))
	quarantined := e.seedAccount(t, pool, "q")
	e.setAccountFlags(t, quarantined, true, false)
	disabled := e.seedAccount(t, pool, "d")
	e.setAccountFlags(t, disabled, false, true)
	atLimit := e.seedAccount(t, pool, "al")
	e.setAccountDailyState(t, atLimit, 30, 30, todayUTC(), true, strPtr("daily_limit"))

	seen := map[string]int{}
	for i := 0; i < 40; i++ {
		acct, ok, err := e.d.accountConsume(context.Background(), pool, defaultAccountOrderBy, nil, "UTC", 0, 900)
		if err != nil {
			t.Fatalf("accountConsume: %v", err)
		}
		if !ok {
			break
		}
		seen[acct.id]++
	}
	for _, bad := range []string{pausedManual, quarantined, disabled, atLimit} {
		if seen[bad] > 0 {
			t.Fatalf("ineligible account %s was selected %d time(s)", bad, seen[bad])
		}
	}
	if seen[healthy]+seen[healthy2] != 40 {
		t.Fatalf("healthy accounts got %d of 40 selections, want all 40 split between them", seen[healthy]+seen[healthy2])
	}
	if seen[healthy] == 0 || seen[healthy2] == 0 {
		t.Fatalf("only one healthy account was ever picked (healthy=%d healthy2=%d) — clean result might be vacuous with a single candidate", seen[healthy], seen[healthy2])
	}
}

// TestAccountConsume_LazyResetSelfHeals (T7-style) — an account at its limit from a STALE day
// (daily_reset_day = yesterday, paused for daily_limit) is admitted and self-heals in the SAME
// UPDATE: counter resets to 0 then increments to 1, daily_reset_day becomes today, paused clears.
// Negative control: a stale-day account paused for 'manual' is NOT admitted by the reset (manual
// pauses never self-clear).
func TestAccountConsume_LazyResetSelfHeals(t *testing.T) {
	e := newDispatchEnv(t, "lazyreset")
	pool := e.seedPool(t)

	stale := e.seedAccount(t, pool, "stale")
	e.setAccountDailyState(t, stale, 50, 50, yesterdayUTC(), true, strPtr("daily_limit"))
	staleManual := e.seedAccount(t, pool, "stalemanual")
	e.setAccountDailyState(t, staleManual, 50, 10, yesterdayUTC(), true, strPtr("manual"))

	acct, ok, err := e.d.accountConsume(context.Background(), pool, defaultAccountOrderBy, nil, "UTC", 0, 900)
	if err != nil {
		t.Fatalf("accountConsume: %v", err)
	}
	if !ok {
		t.Fatal("no account consumed — the stale-day self-heal did not admit the eligible account")
	}
	if acct.id != stale {
		t.Fatalf("consumed account=%s, want the stale self-healing account %s (not the manual-paused one)", acct.id, stale)
	}

	var sent int
	var resetDay time.Time
	var paused bool
	var reason *string
	if err := e.admin.QueryRow(context.Background(),
		`SELECT daily_sent_count, daily_reset_day, paused, pause_reason::text FROM send_managed.accounts WHERE id=$1`, stale).
		Scan(&sent, &resetDay, &paused, &reason); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if sent != 1 {
		t.Fatalf("daily_sent_count=%d after self-heal consume, want 1 (reset to 0 then incremented)", sent)
	}
	if paused || reason != nil {
		t.Fatalf("paused=%v reason=%v after self-heal, want FALSE/NULL", paused, reason)
	}
	if resetDay.Format("2006-01-02") != todayUTC() {
		t.Fatalf("daily_reset_day=%s after self-heal, want today (%s)", resetDay.Format("2006-01-02"), todayUTC())
	}

	// Negative control: the manual-paused stale account must NEVER be admitted by this consume —
	// drain any remaining eligibility and confirm it was never selected.
	for i := 0; i < 5; i++ {
		a, ok, err := e.d.accountConsume(context.Background(), pool, defaultAccountOrderBy, nil, "UTC", 0, 900)
		if err != nil {
			t.Fatalf("accountConsume: %v", err)
		}
		if !ok {
			break
		}
		if a.id == staleManual {
			t.Fatalf("manual-paused stale account was selected — the lazy reset must never clear a manual pause")
		}
	}
}

// TestAccountConsume_ExhaustedPoolReturnsNotFound — a pool where every account is ineligible
// returns (false, nil), never an error, and poolHasEligible reports false for the SAME pool (§4.1:
// exhaustion is defined identically for both).
func TestAccountConsume_ExhaustedPoolReturnsNotFound(t *testing.T) {
	e := newDispatchEnv(t, "exhausted")
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	e.setAccountDailyState(t, acct, 10, 10, todayUTC(), true, strPtr("daily_limit"))

	_, ok, err := e.d.accountConsume(context.Background(), pool, defaultAccountOrderBy, nil, "UTC", 0, 900)
	if err != nil {
		t.Fatalf("accountConsume: %v", err)
	}
	if ok {
		t.Fatal("consumed an account from a fully-exhausted pool")
	}
	has, err := e.d.poolHasEligible(context.Background(), pool, "UTC", 0, 900)
	if err != nil {
		t.Fatalf("poolHasEligible: %v", err)
	}
	if has {
		t.Fatal("poolHasEligible=true for a pool the consume just found exhausted — predicates disagree")
	}

	// Positive control: add ONE healthy account to the SAME pool; now both report eligible.
	e.seedAccount(t, pool, "healthy")
	has2, err := e.d.poolHasEligible(context.Background(), pool, "UTC", 0, 900)
	if err != nil {
		t.Fatalf("poolHasEligible: %v", err)
	}
	if !has2 {
		t.Fatal("poolHasEligible=false after adding a healthy account — predicate over-rejects")
	}
	_, ok2, err := e.d.accountConsume(context.Background(), pool, defaultAccountOrderBy, nil, "UTC", 0, 900)
	if err != nil {
		t.Fatalf("accountConsume: %v", err)
	}
	if !ok2 {
		t.Fatal("consume found nothing after adding a healthy account")
	}
}

// TestDispatch_IncrementsAtConsumeNotAtAccept (Phase 4 behavior change) — daily_sent_count is
// incremented the moment TryDispatch consumes the account (BEFORE any report arrives), not later
// at accept time. This is the ordering the double-booking core depends on.
func TestDispatch_IncrementsAtConsumeNotAtAccept(t *testing.T) {
	e := newDispatchEnv(t, "incratconsume")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	job := e.seedQueuedJob(t, "inc@example.com")

	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("TryDispatch: %v", err)
	}
	if got := e.accountDailySentCount(t, acct); got != 1 {
		t.Fatalf("daily_sent_count=%d immediately after dispatch (before any report), want 1", got)
	}

	// The subsequent accept must NOT increment it again.
	e.oh.Handle(e.px.workerID, mustReportAccepted(job))
	if got := e.accountDailySentCount(t, acct); got != 1 {
		t.Fatalf("daily_sent_count=%d after accept, want STILL 1 (no double-increment)", got)
	}
}

func todayUTC() string     { return time.Now().UTC().Format("2006-01-02") }
func yesterdayUTC() string { return time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02") }

func mustReportAccepted(jobID string) workerproto.Report {
	return workerproto.Report{Type: workerproto.TypeReport, JobID: jobID, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-" + jobID, SMTPResponse: "250 2.0.0 Ok"}
}
