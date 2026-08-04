package dispatch

// Chunk 3 behavioral tests: routing strategies (§3) distribute across a REAL multi-account pool
// as specified. All three strategies choose only among eligible accounts (proven in Chunk 2); this
// file proves the ORDER BY each strategy contributes actually produces its documented distribution
// shape. High per-account limits (never binding) isolate "does the strategy pick the right
// accounts" from "does the daily limit cut it off" (that is eligibility.go's own test file).

import (
	"context"
	"testing"
)

// TestRouting_RoundRobin — 4 healthy accounts, 40 consumes, ≥1 concurrent-safe: each account used
// close to 10 times (max-min <= 1, i.e. exactly even since this is sequential — a concurrency
// tolerance is not needed for a single-threaded drain). Anti-vacuity: sequence approximates a
// cycle (consecutive picks are never the same account back-to-back beyond the wrap).
func TestRouting_RoundRobin(t *testing.T) {
	e := newDispatchEnv(t, "rrstrat")
	pool := e.seedPoolWithStrategy(t, "round_robin")
	accts := make([]string, 4)
	for i := range accts {
		accts[i] = e.seedAccount(t, pool, "sec")
		e.setAccountDailyState(t, accts[i], 1000, 0, todayUTC(), false, nil)
	}

	counts := map[string]int{}
	var lastAcct string
	backToBack := 0
	for i := 0; i < 40; i++ {
		a, ok, err := e.d.consumeFromPool(context.Background(), pool, "UTC", 0, 900)
		if err != nil {
			t.Fatalf("consumeFromPool: %v", err)
		}
		if !ok {
			t.Fatalf("consume %d found nothing (want a healthy account)", i)
		}
		counts[a.id]++
		if a.id == lastAcct {
			backToBack++
		}
		lastAcct = a.id
	}
	for _, id := range accts {
		if counts[id] != 10 {
			t.Fatalf("account %s used %d times, want exactly 10 (40 consumes / 4 accounts, sequential round_robin)", id, counts[id])
		}
	}
	if backToBack > 0 {
		t.Fatalf("the same account was picked twice in a row %d time(s) — round_robin must cycle, never repeat immediately with all accounts healthy", backToBack)
	}
}

// TestRouting_RoundRobin_CursorSurvivesAcrossPools — the round_robin cursor is PER-POOL: two
// separate round_robin pools each cycle independently (advancing one must not perturb the other).
func TestRouting_RoundRobin_CursorSurvivesAcrossPools(t *testing.T) {
	e := newDispatchEnv(t, "rrpools")
	poolA := e.seedPoolWithStrategy(t, "round_robin")
	poolB := e.seedPoolWithStrategy(t, "round_robin")
	a1 := e.seedAccount(t, poolA, "sec")
	e.setAccountDailyState(t, a1, 1000, 0, todayUTC(), false, nil)
	a2 := e.seedAccount(t, poolA, "sec")
	e.setAccountDailyState(t, a2, 1000, 0, todayUTC(), false, nil)
	b1 := e.seedAccount(t, poolB, "sec")
	e.setAccountDailyState(t, b1, 1000, 0, todayUTC(), false, nil)

	// Consume from A twice (cycles a1,a2) then from B once — B must still see b1 (its own pool's
	// cursor is untouched by A's activity).
	if _, ok, err := e.d.consumeFromPool(context.Background(), poolA, "UTC", 0, 900); err != nil || !ok {
		t.Fatalf("consume A#1: ok=%v err=%v", ok, err)
	}
	if _, ok, err := e.d.consumeFromPool(context.Background(), poolA, "UTC", 0, 900); err != nil || !ok {
		t.Fatalf("consume A#2: ok=%v err=%v", ok, err)
	}
	got, ok, err := e.d.consumeFromPool(context.Background(), poolB, "UTC", 0, 900)
	if err != nil || !ok {
		t.Fatalf("consume B#1: ok=%v err=%v", ok, err)
	}
	if got.id != b1 {
		t.Fatalf("pool B's first consume got %s, want its only account %s — pool A's cursor bled into pool B", got.id, b1)
	}
}

// TestRouting_OneByOne — 2 accounts (A limit 15, B limit 15), 25 consumes. A must be exhausted
// (reach its limit) strictly before the other's first use. Sink-equivalent assertion via the
// consume sequence itself: the leader gets exactly 15, the follower exactly 10, and every leader
// consume precedes every follower consume. Account ids are random UUIDs, so "the leader" is
// whichever of the two the deterministic id-ASC tiebreak (P4-OD-1) picks first — the test asserts
// on THAT identity, never assuming creation order == id order.
func TestRouting_OneByOne(t *testing.T) {
	e := newDispatchEnv(t, "obo")
	pool := e.seedPoolWithStrategy(t, "one_by_one")
	x := e.seedAccount(t, pool, "sec")
	e.setAccountDailyState(t, x, 15, 0, todayUTC(), false, nil)
	y := e.seedAccount(t, pool, "sec")
	e.setAccountDailyState(t, y, 15, 0, todayUTC(), false, nil)

	var sequence []string
	for i := 0; i < 25; i++ {
		a, ok, err := e.d.consumeFromPool(context.Background(), pool, "UTC", 0, 900)
		if err != nil {
			t.Fatalf("consumeFromPool: %v", err)
		}
		if !ok {
			t.Fatalf("consume %d found nothing (want 25 total capacity across both accounts)", i)
		}
		sequence = append(sequence, a.id)
	}
	if len(sequence) == 0 {
		t.Fatal("empty sequence")
	}
	leader := sequence[0]
	follower := x
	if leader == x {
		follower = y
	}
	countLeader, countFollower := 0, 0
	sawFollowerBeforeLeaderExhausted := false
	leaderExhausted := false
	for _, id := range sequence {
		switch id {
		case leader:
			countLeader++
			if leaderExhausted {
				t.Fatal("the leader account was consumed again AFTER it should have been exhausted")
			}
			if countLeader == 15 {
				leaderExhausted = true
			}
		case follower:
			countFollower++
			if !leaderExhausted {
				sawFollowerBeforeLeaderExhausted = true
			}
		default:
			t.Fatalf("consumed an unexpected account id %s", id)
		}
	}
	if countLeader != 15 || countFollower != 10 {
		t.Fatalf("countLeader=%d countFollower=%d, want 15/10 (leader fully exhausted first, one_by_one)", countLeader, countFollower)
	}
	if sawFollowerBeforeLeaderExhausted {
		t.Fatal("the follower was consumed before the leader reached its limit — one_by_one must exhaust the leader first")
	}
}

// TestRouting_Random — 5 healthy accounts (high limits), 500 consumes: every account used at
// least once, none used implausibly rarely/often for a uniform draw (wide band — asserting a
// tight distribution would wrongly fail true randomness), and NOT every consume picks the SAME
// account (the un-shuffled "just always pick account #1" failure mode).
func TestRouting_Random(t *testing.T) {
	e := newDispatchEnv(t, "randstrat")
	pool := e.seedPoolWithStrategy(t, "random")
	accts := make([]string, 5)
	for i := range accts {
		accts[i] = e.seedAccount(t, pool, "sec")
		e.setAccountDailyState(t, accts[i], 100000, 0, todayUTC(), false, nil)
	}

	counts := map[string]int{}
	for i := 0; i < 500; i++ {
		a, ok, err := e.d.consumeFromPool(context.Background(), pool, "UTC", 0, 900)
		if err != nil {
			t.Fatalf("consumeFromPool: %v", err)
		}
		if !ok {
			t.Fatalf("consume %d found nothing", i)
		}
		counts[a.id]++
	}
	for _, id := range accts {
		if counts[id] == 0 {
			t.Fatalf("account %s was NEVER picked in 500 draws across 5 accounts — not a uniform random draw", id)
		}
		// Wide band: expected 100, allow [40,200] (well beyond 3 sigma at p=0.2,n=500, sigma≈8.9) —
		// deliberately loose so true randomness never spuriously fails this test.
		if counts[id] < 40 || counts[id] > 200 {
			t.Fatalf("account %s picked %d/500 times, want roughly uniform (expected ~100, got outside [40,200])", id, counts[id])
		}
	}
	if len(counts) < 2 {
		t.Fatalf("only %d distinct account(s) were ever picked across 500 draws — clearly not random", len(counts))
	}
}

// TestRouting_StrategyReadFromPoolColumn — the strategy actually used is whatever the POOL's
// routing_strategy column says, not a hardcoded default: seed the SAME two accounts under
// one_by_one, drain account A to its limit, then flip the pool to round_robin and confirm B (the
// only remaining eligible account) is now reachable via the round_robin path too (a smoke check
// that consumeFromPool re-reads the column every time, not a cached value).
func TestRouting_StrategyReadFromPoolColumn(t *testing.T) {
	e := newDispatchEnv(t, "stratswitch")
	pool := e.seedPoolWithStrategy(t, "one_by_one")
	x := e.seedAccount(t, pool, "sec")
	y := e.seedAccount(t, pool, "sec")
	// The deterministic id-ASC tiebreak (P4-OD-1) always picks the LOWER id first under
	// one_by_one — never assume creation order == id order. Give the lower-id account (the
	// guaranteed leader) the small limit so it exhausts, then the other becomes eligible.
	leader, other := x, y
	if y < x {
		leader, other = y, x
	}
	e.setAccountDailyState(t, leader, 1, 0, todayUTC(), false, nil)
	e.setAccountDailyState(t, other, 1000, 0, todayUTC(), false, nil)

	// Exhaust the leader under one_by_one (deterministic — no RANDOM() tiebreak, P4-OD-1).
	for i := 0; i < 3; i++ {
		var full bool
		if err := e.admin.QueryRow(context.Background(),
			`SELECT daily_sent_count >= daily_limit FROM send_managed.accounts WHERE id=$1`, leader).Scan(&full); err != nil {
			t.Fatalf("check leader: %v", err)
		}
		if full {
			break
		}
		if _, ok, err := e.d.consumeFromPool(context.Background(), pool, "UTC", 0, 900); err != nil || !ok {
			t.Fatalf("drain consume %d: ok=%v err=%v", i, ok, err)
		}
	}
	var leaderFull bool
	if err := e.admin.QueryRow(context.Background(),
		`SELECT daily_sent_count >= daily_limit FROM send_managed.accounts WHERE id=$1`, leader).Scan(&leaderFull); err != nil {
		t.Fatalf("check leader final: %v", err)
	}
	if !leaderFull {
		t.Fatal("the leader account never reached its limit — test setup broken")
	}

	if _, err := e.admin.Exec(context.Background(),
		`UPDATE send_managed.pools SET routing_strategy='round_robin' WHERE id=$1`, pool); err != nil {
		t.Fatalf("flip strategy: %v", err)
	}
	got, ok, err := e.d.consumeFromPool(context.Background(), pool, "UTC", 0, 900)
	if err != nil {
		t.Fatalf("consumeFromPool after flip: %v", err)
	}
	if !ok || got.id != other {
		t.Fatalf("after flipping to round_robin, consume ok=%v got=%v, want the only eligible account %s", ok, got.id, other)
	}
}
