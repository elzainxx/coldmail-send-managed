package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// defaultWeights mirrors the send_managed_0017 seed (the documented V1 defaults, §3.2).
func defaultWeights() ScoreWeights {
	return ScoreWeights{WFail: 60, WProx: 15, WRecency: 25, FailSaturation: 5, RecencyHorizonHours: 72}
}

// TestComputeScore_Deterministic is the PURE-function oracle (§3.2, T5): the score is computed
// from the row + weights alone, so we assert exact equality against hand-computed values. This is
// the reproducibility guarantee the state-machine tests rely on.
func TestComputeScore_Deterministic(t *testing.T) {
	w := defaultWeights()
	today := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	now := today
	fresh := now // last_success_at == now => recency penalty 0

	cases := []struct {
		name string
		in   scoreInputs
		want int
	}{
		{
			// Perfect: 0 failures, 0 daily proximity, just-succeeded → full 100.
			name: "perfect",
			in:   scoreInputs{consecutiveFailures: 0, dailySentCount: 0, dailyLimit: 100, dailyResetDay: today, lastSuccessAt: &fresh, nowInTZ: today, now: now},
			want: 100,
		},
		{
			// 1 consecutive failure, just-succeeded-then-failed (recency ~0), no daily proximity:
			// 100 - 60*(1/5) = 100 - 12 = 88.
			name: "one_failure",
			in:   scoreInputs{consecutiveFailures: 1, dailySentCount: 0, dailyLimit: 100, dailyResetDay: today, lastSuccessAt: &fresh, nowInTZ: today, now: now},
			want: 88,
		},
		{
			// 5 failures saturates failPenalty at 1.0: 100 - 60 = 40 (recency 0, prox 0).
			name: "saturated_failures",
			in:   scoreInputs{consecutiveFailures: 5, dailySentCount: 0, dailyLimit: 100, dailyResetDay: today, lastSuccessAt: &fresh, nowInTZ: today, now: now},
			want: 40,
		},
		{
			// 8 failures still saturates at 1.0 (min): same 40 — proves the ramp saturates.
			name: "over_saturated",
			in:   scoreInputs{consecutiveFailures: 8, dailySentCount: 0, dailyLimit: 100, dailyResetDay: today, lastSuccessAt: &fresh, nowInTZ: today, now: now},
			want: 40,
		},
		{
			// Never-succeeded (NULL last_success) → full recency penalty 1.0: 100 - 25 = 75.
			name: "never_succeeded",
			in:   scoreInputs{consecutiveFailures: 0, dailySentCount: 0, dailyLimit: 100, dailyResetDay: today, lastSuccessAt: nil, nowInTZ: today, now: now},
			want: 75,
		},
		{
			// At daily limit (100/100): full proximity penalty 1.0: 100 - 15 = 85.
			name: "at_daily_limit",
			in:   scoreInputs{consecutiveFailures: 0, dailySentCount: 100, dailyLimit: 100, dailyResetDay: today, lastSuccessAt: &fresh, nowInTZ: today, now: now},
			want: 85,
		},
		{
			// STALE cycle (reset day yesterday) => proximity counts as 0 even though counter==limit:
			// 100 - 0 = 100 (recency 0).
			name: "stale_cycle_zero_prox",
			in:   scoreInputs{consecutiveFailures: 0, dailySentCount: 100, dailyLimit: 100, dailyResetDay: today.AddDate(0, 0, -1), lastSuccessAt: &fresh, nowInTZ: today, now: now},
			want: 100,
		},
		{
			// All three maxed: 100 - 60 - 15 - 25 = 0 (floors exactly at 0, weights sum to 100).
			name: "worst_case_floor",
			in:   scoreInputs{consecutiveFailures: 5, dailySentCount: 100, dailyLimit: 100, dailyResetDay: today, lastSuccessAt: nil, nowInTZ: today, now: now},
			want: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := computeScore(c.in, w); got != c.want {
				t.Fatalf("computeScore(%s) = %d, want %d", c.name, got, c.want)
			}
		})
	}
}

// TestScore_DegradesOnFailureResetsOnSuccess (real PG) — the stored score decreases monotonically
// across repeated transient failures (each recompute lowers it), then a success resets
// consecutive_failures to 0 and recomputes the score UPWARD. Cross-checks the stored column, not
// just the pure function.
func TestScore_DegradesOnFailureResetsOnSuccess(t *testing.T) {
	e := newDispatchEnv(t, "scoredeg")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")

	readScore := func() (int, int) {
		var score, cf int
		if err := e.admin.QueryRow(ctx, `SELECT account_status_score, consecutive_failures FROM send_managed.accounts WHERE id=$1`, acct).Scan(&score, &cf); err != nil {
			t.Fatalf("read score: %v", err)
		}
		return score, cf
	}

	// Seed a job and drive two transient (451 transport_4xx) failures; score must strictly drop.
	job := e.seedQueuedJob(t, "s@example.com")
	prev := 101
	for i := 0; i < 2; i++ {
		if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
			t.Fatalf("reset next: %v", err)
		}
		e.dispatchToSending(t, job)
		e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
			SMTPResponse: "451 4.3.0 temporary local error"})
		e.px.freeAllWorkers()
		score, cf := readScore()
		if cf != i+1 {
			t.Fatalf("after failure %d: consecutive_failures=%d, want %d", i+1, cf, i+1)
		}
		if score >= prev {
			t.Fatalf("after failure %d: score=%d did not drop below previous %d (monotone §3.2)", i+1, score, prev)
		}
		prev = score
	}

	// Now a success: consecutive_failures resets to 0 and the score recomputes upward.
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
		t.Fatalf("reset next: %v", err)
	}
	e.dispatchToSending(t, job)
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-SCORE-1", SMTPResponse: "250 2.0.0 Ok"})
	score, cf := readScore()
	if cf != 0 {
		t.Fatalf("after success: consecutive_failures=%d, want 0 (reset §3.3)", cf)
	}
	if score <= prev {
		t.Fatalf("after success: score=%d did not recover above the degraded %d", score, prev)
	}
}

// TestScore_BounceNeverPenalizes (T4, the carve-out teeth) — a 550 bounce leaves the account's
// score, consecutive_failures, and last_failure_at BYTE-UNCHANGED. A naive "recompute on every
// outcome" impl that forgot the §3.3 carve-out would drop the score and fail here.
func TestScore_BounceNeverPenalizes(t *testing.T) {
	e := newDispatchEnv(t, "bounceScore")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")

	// Baseline snapshot (fresh account: score 100, cf 0, last_failure_at NULL).
	var score0, cf0 int
	var lastFail0 *time.Time
	if err := e.admin.QueryRow(ctx, `SELECT account_status_score, consecutive_failures, last_failure_at FROM send_managed.accounts WHERE id=$1`, acct).Scan(&score0, &cf0, &lastFail0); err != nil {
		t.Fatalf("baseline: %v", err)
	}

	job := e.seedQueuedJob(t, "d@example.com")
	e.dispatchToSending(t, job)
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeBounced,
		SMTPResponse: "550 5.1.1 No such user"})

	// Job bounced (terminal), account UNCHANGED.
	state, _, leaseNull, completed, _ := e.jobState(t, job)
	if state != "bounced" || !leaseNull || !completed {
		t.Fatalf("job state=%s leaseNull=%v completed=%v, want bounced/true/true", state, leaseNull, completed)
	}
	var score1, cf1 int
	var lastFail1 *time.Time
	if err := e.admin.QueryRow(ctx, `SELECT account_status_score, consecutive_failures, last_failure_at FROM send_managed.accounts WHERE id=$1`, acct).Scan(&score1, &cf1, &lastFail1); err != nil {
		t.Fatalf("after bounce: %v", err)
	}
	if score1 != score0 {
		t.Fatalf("bounce changed score %d -> %d (must be byte-unchanged §3.3)", score0, score1)
	}
	if cf1 != cf0 {
		t.Fatalf("bounce changed consecutive_failures %d -> %d (must be untouched §3.3)", cf0, cf1)
	}
	if (lastFail0 == nil) != (lastFail1 == nil) {
		t.Fatalf("bounce changed last_failure_at nullity (must be untouched §3.3)")
	}
	// And NO account_events row (a bounce writes none — §2.1).
	var aev int
	if err := e.admin.QueryRow(ctx, `SELECT count(*) FROM send_managed.account_events WHERE account_id=$1`, acct).Scan(&aev); err != nil {
		t.Fatalf("account_events: %v", err)
	}
	if aev != 0 {
		t.Fatalf("bounce wrote %d account_events, want 0", aev)
	}

	// PAIRED POSITIVE CONTROL (anti-vacuity) — without this, the "score unchanged" assertions above
	// would pass even if the scoring path were entirely broken/no-op. A NON-bounce failure
	// (transport_4xx here) driven through the SAME outcome handler, on a fresh account seeded
	// identically (score 100, cf 0), MUST move the score down and bump consecutive_failures. This
	// proves the handler that left the bounce untouched genuinely DOES recompute on a penalizing
	// failure — so the carve-out is a real exemption, not a dead code path.
	acct2 := e.seedAccount(t, pool, "sec2")
	var score2Base, cf2Base int
	if err := e.admin.QueryRow(ctx, `SELECT account_status_score, consecutive_failures FROM send_managed.accounts WHERE id=$1`, acct2).Scan(&score2Base, &cf2Base); err != nil {
		t.Fatalf("control baseline: %v", err)
	}
	// Quarantine the FIRST account so routing lands the next job on acct2 (the control target).
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET quarantined=TRUE WHERE id=$1`, acct); err != nil {
		t.Fatalf("park acct1: %v", err)
	}
	e.px.freeAllWorkers() // the bounce report freed the worker in reality; free the recordingPusher's marker
	jobCtl := e.seedQueuedJob(t, "ctl@example.com")
	e.dispatchToSending(t, jobCtl)
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: jobCtl, Outcome: workerproto.OutcomeError,
		SMTPResponse: "451 4.3.0 temporary local error"}) // transport_4xx — a penalizing (non-bounce) failure
	var score2After, cf2After int
	if err := e.admin.QueryRow(ctx, `SELECT account_status_score, consecutive_failures FROM send_managed.accounts WHERE id=$1`, acct2).Scan(&score2After, &cf2After); err != nil {
		t.Fatalf("control after: %v", err)
	}
	if score2After >= score2Base {
		t.Fatalf("POSITIVE CONTROL FAILED: a transport_4xx failure did NOT lower the score (%d -> %d) — the carve-out test would be vacuous; the scoring path may be broken", score2Base, score2After)
	}
	if cf2After != cf2Base+1 {
		t.Fatalf("POSITIVE CONTROL FAILED: a transport_4xx failure did NOT bump consecutive_failures (%d -> %d)", cf2Base, cf2After)
	}
}
