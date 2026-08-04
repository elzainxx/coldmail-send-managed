package dispatch

// Chunk 4 behavioral tests: pool resolution + fallback (§4). Real Postgres. Proves: dedicated is
// tried before shared and fallback fires ONLY on true exhaustion (T2); the P4-3 kind policy
// (dedicated never falls back onto another dedicated pool); P4-4's multi-shared resolution order
// (explicit fallback_pool_id wins over the config default); and the clean, crash-free
// shared_pool_not_configured state when default_shared_pool_id is unset/invalid (T10).

import (
	"context"
	"testing"
	"time"
)

// TestPoolChain_DedicatedTriedBeforeShared — a user with a dedicated pool (capacity 20) and a
// shared fallback (capacity 100). Enqueuing demand UNDER the dedicated capacity must send 100% via
// the dedicated account and ZERO via shared — "not before" (spec T2 first assertion).
func TestPoolChain_DedicatedTriedBeforeShared(t *testing.T) {
	e := newDispatchEnv(t, "chainorder")
	ctx := context.Background()
	userA := e.seedUser(t, "chain-a@tenant.example")
	shared := e.seedSharedPool(t, "shared-fallback")
	sharedAcct := e.seedAccount(t, shared, "sec")
	e.setAccountDailyState(t, sharedAcct, 100, 0, todayUTC(), false, nil)
	dedicated := e.seedDedicatedPool(t, userA, shared)
	dedicatedAcct := e.seedAccount(t, dedicated, "sec")
	e.setAccountDailyState(t, dedicatedAcct, 20, 0, todayUTC(), false, nil)

	for i := 0; i < 15; i++ {
		acct, ok, err := e.d.selectAccountForJob(ctx, claimedJob{id: "n/a", userID: userA}, e.mustLoadConfig(t))
		if err != nil {
			t.Fatalf("selectAccountForJob: %v", err)
		}
		if !ok {
			t.Fatalf("consume %d found nothing (dedicated has capacity)", i)
		}
		if acct.id != dedicatedAcct {
			t.Fatalf("consume %d picked %s, want the dedicated account %s (shared must not bleed traffic while dedicated has capacity)", i, acct.id, dedicatedAcct)
		}
	}
	if got := e.accountDailySentCount(t, sharedAcct); got != 0 {
		t.Fatalf("shared account daily_sent_count=%d, want 0 (dedicated capacity was never exhausted)", got)
	}
}

// TestPoolChain_SpilloverOnlyAfterExhaustion — enqueue MORE than the dedicated pool's capacity:
// exactly N go to dedicated, then the rest to shared — never an early split (spec T2 second
// assertion, the "not 12/13" anti-vacuity check).
func TestPoolChain_SpilloverOnlyAfterExhaustion(t *testing.T) {
	e := newDispatchEnv(t, "spillover")
	ctx := context.Background()
	userA := e.seedUser(t, "spill-a@tenant.example")
	shared := e.seedSharedPool(t, "shared-fallback")
	sharedAcct := e.seedAccount(t, shared, "sec")
	e.setAccountDailyState(t, sharedAcct, 100, 0, todayUTC(), false, nil)
	dedicated := e.seedDedicatedPool(t, userA, shared)
	dedicatedAcct := e.seedAccount(t, dedicated, "sec")
	e.setAccountDailyState(t, dedicatedAcct, 20, 0, todayUTC(), false, nil)

	var dedicatedCount, sharedCount int
	var sawSharedBeforeDedicatedExhausted bool
	dedicatedExhausted := false
	for i := 0; i < 25; i++ {
		acct, ok, err := e.d.selectAccountForJob(ctx, claimedJob{id: "n/a", userID: userA}, e.mustLoadConfig(t))
		if err != nil {
			t.Fatalf("selectAccountForJob: %v", err)
		}
		if !ok {
			t.Fatalf("consume %d found nothing (want 25 total capacity across dedicated+shared)", i)
		}
		switch acct.id {
		case dedicatedAcct:
			dedicatedCount++
			if dedicatedExhausted {
				t.Fatal("dedicated was consumed again AFTER it should have been exhausted")
			}
			if dedicatedCount == 20 {
				dedicatedExhausted = true
			}
		case sharedAcct:
			sharedCount++
			if !dedicatedExhausted {
				sawSharedBeforeDedicatedExhausted = true
			}
		}
	}
	if dedicatedCount != 20 || sharedCount != 5 {
		t.Fatalf("dedicatedCount=%d sharedCount=%d, want EXACTLY 20/5 (never an early split)", dedicatedCount, sharedCount)
	}
	if sawSharedBeforeDedicatedExhausted {
		t.Fatal("shared was consumed before dedicated reached its limit — fallback must fire only on true exhaustion")
	}

	// Make dedicated ineligible outright (paused) and enqueue 10 more: all 10 must go to shared,
	// dedicated's counter stays unchanged.
	e.setAccountDailyState(t, dedicatedAcct, 20, 20, todayUTC(), true, strPtr("daily_limit"))
	for i := 0; i < 10; i++ {
		acct, ok, err := e.d.selectAccountForJob(ctx, claimedJob{id: "n/a", userID: userA}, e.mustLoadConfig(t))
		if err != nil {
			t.Fatalf("post-exhaustion consume %d: %v", i, err)
		}
		if !ok || acct.id != sharedAcct {
			t.Fatalf("post-exhaustion consume %d: ok=%v got=%v, want shared account %s", i, ok, acct.id, sharedAcct)
		}
	}
	if got := e.accountDailySentCount(t, dedicatedAcct); got != 20 {
		t.Fatalf("dedicated daily_sent_count=%d after being paused, want unchanged at 20", got)
	}
}

// TestPoolChain_KindPolicy_NeverDedicatedToDedicated (P4-3) — a dedicated pool's fallback_pool_id
// pointing at ANOTHER user's DEDICATED pool must be IGNORED for routing: the chain falls through
// to the config default_shared_pool_id instead, never touching the other user's dedicated account.
func TestPoolChain_KindPolicy_NeverDedicatedToDedicated(t *testing.T) {
	e := newDispatchEnv(t, "kindpolicy")
	ctx := context.Background()
	userA := e.seedUser(t, "kind-a@tenant.example")
	userB := e.seedUser(t, "kind-b@tenant.example")

	// User B's dedicated pool + account — must NEVER be touched by A's routing.
	othersDedicated := e.seedDedicatedPool(t, userB, "")
	othersAcct := e.seedAccount(t, othersDedicated, "sec")
	e.setAccountDailyState(t, othersAcct, 1000, 0, todayUTC(), false, nil)

	// The real (config) default shared pool + account — this is what A's chain SHOULD fall to.
	realShared := e.seedPool(t) // registers as default_shared_pool_id
	realSharedAcct := e.seedAccount(t, realShared, "sec")
	e.setAccountDailyState(t, realSharedAcct, 1000, 0, todayUTC(), false, nil)

	// A's dedicated pool: exhausted (paused), fallback_pool_id points at B's DEDICATED pool.
	mineDedicated := e.seedDedicatedPool(t, userA, othersDedicated)
	mineAcct := e.seedAccount(t, mineDedicated, "sec")
	e.setAccountDailyState(t, mineAcct, 10, 10, todayUTC(), true, strPtr("daily_limit"))

	acct, ok, err := e.d.selectAccountForJob(ctx, claimedJob{id: "n/a", userID: userA}, e.mustLoadConfig(t))
	if err != nil {
		t.Fatalf("selectAccountForJob: %v", err)
	}
	if !ok {
		t.Fatal("chain resolved to nothing — should have fallen through to the config default shared pool")
	}
	if acct.id == othersAcct {
		t.Fatal("routed onto another user's DEDICATED account — P4-3 kind policy violated")
	}
	if acct.id != realSharedAcct {
		t.Fatalf("consumed account=%s, want the config default shared account %s (dedicated fallback_pool_id pointing at a dedicated pool must be ignored)", acct.id, realSharedAcct)
	}
	if got := e.accountDailySentCount(t, othersAcct); got != 0 {
		t.Fatalf("another user's dedicated account daily_sent_count=%d, want 0 (untouched)", got)
	}
}

// TestPoolChain_ExplicitFallbackWinsOverConfigDefault (P4-4) — when a dedicated pool has its OWN
// explicit fallback_pool_id resolving to an enabled shared pool, that wins over
// default_shared_pool_id, even though the config default is a DIFFERENT (also valid) shared pool.
func TestPoolChain_ExplicitFallbackWinsOverConfigDefault(t *testing.T) {
	e := newDispatchEnv(t, "explicitwins")
	ctx := context.Background()
	userA := e.seedUser(t, "explicit-a@tenant.example")

	configDefault := e.seedPool(t) // registers as default_shared_pool_id
	configDefaultAcct := e.seedAccount(t, configDefault, "sec")
	e.setAccountDailyState(t, configDefaultAcct, 1000, 0, todayUTC(), false, nil)

	explicitFallback := e.seedSharedPool(t, "explicit-fallback")
	explicitFallbackAcct := e.seedAccount(t, explicitFallback, "sec")
	e.setAccountDailyState(t, explicitFallbackAcct, 1000, 0, todayUTC(), false, nil)

	dedicated := e.seedDedicatedPool(t, userA, explicitFallback)
	dedicatedAcct := e.seedAccount(t, dedicated, "sec")
	e.setAccountDailyState(t, dedicatedAcct, 5, 5, todayUTC(), true, strPtr("daily_limit")) // exhausted

	acct, ok, err := e.d.selectAccountForJob(ctx, claimedJob{id: "n/a", userID: userA}, e.mustLoadConfig(t))
	if err != nil {
		t.Fatalf("selectAccountForJob: %v", err)
	}
	if !ok {
		t.Fatal("chain resolved to nothing")
	}
	if acct.id != explicitFallbackAcct {
		t.Fatalf("consumed account=%s, want the EXPLICIT fallback account %s (it must win over the config default %s)", acct.id, explicitFallbackAcct, configDefaultAcct)
	}
}

// TestPoolChain_SharedPoolNotConfigured (T10) — a BASE (non-dedicated) user with
// default_shared_pool_id UNSET: the chain resolves to EMPTY (not an error), selectAccountForJob
// returns (zero, false, nil) cleanly, and TryDispatch/resolveAndPush requeue the job to a legal
// queued state rather than crashing or churning. Paired sub-case: setting default_shared_pool_id
// to a real, enabled shared pool makes the SAME job dispatchable on the very next attempt (no
// re-enqueue needed — config is read fresh every call, §0 OD-14).
func TestPoolChain_SharedPoolNotConfigured(t *testing.T) {
	e := newDispatchEnv(t, "notconfigured")
	ctx := context.Background()
	userA := e.seedUser(t, "noconf-a@tenant.example")
	e.clearDefaultSharedPool(t) // the Phase 1 seed: NULL

	// No dedicated pool for A, and NO usable shared target — the chain must be empty.
	acct, ok, err := e.d.selectAccountForJob(ctx, claimedJob{id: "n/a", userID: userA}, e.mustLoadConfig(t))
	if err != nil {
		t.Fatalf("selectAccountForJob returned an error for the unconfigured state, want a clean (false, nil): %v", err)
	}
	if ok {
		t.Fatalf("selectAccountForJob found an account (%s) despite no configured shared pool", acct.id)
	}

	// End-to-end via TryDispatch: the job must requeue cleanly, never crash, never stay stranded
	// 'assigned' with a live lease.
	job := e.seedQueuedJobFor(t, userA, "noconf@example.com")
	pushed, err := e.d.TryDispatch(ctx)
	if err != nil {
		t.Fatalf("TryDispatch errored on the unconfigured state: %v", err)
	}
	if pushed == job {
		t.Fatal("TryDispatch pushed a job with no configured pool — should have requeued")
	}
	state, _, leaseNull, _, _ := e.jobState(t, job)
	if state != "queued" || !leaseNull {
		t.Fatalf("job state=%s leaseNull=%v after the unconfigured attempt, want queued/true (clean requeue, no crash, no stranded lease)", state, leaseNull)
	}

	// Recovery: set default_shared_pool_id to a REAL enabled shared pool. The SAME job (never
	// re-enqueued) must now dispatch on the very next attempt.
	shared := e.seedSharedPool(t, "recovery-shared")
	sharedAcct := e.seedAccount(t, shared, "sec")
	e.setAccountDailyState(t, sharedAcct, 100, 0, todayUTC(), false, nil)
	e.setDefaultSharedPool(t, shared)

	got, ok2, err := e.d.selectAccountForJob(ctx, claimedJob{id: job, userID: userA}, e.mustLoadConfig(t))
	if err != nil {
		t.Fatalf("selectAccountForJob after config set: %v", err)
	}
	if !ok2 || got.id != sharedAcct {
		t.Fatalf("after setting default_shared_pool_id, selectAccountForJob ok=%v got=%v, want the newly-configured shared account %s", ok2, got.id, sharedAcct)
	}
}

// TestPoolChain_InvalidDefaultBehavesLikeUnset (T10 Action C) — default_shared_pool_id pointing at
// a DISABLED or DEDICATED pool (not just NULL) must behave IDENTICALLY to unset: empty chain,
// clean (false, nil), never an error — proving the VALIDITY check, not just the null check, gates.
func TestPoolChain_InvalidDefaultBehavesLikeUnset(t *testing.T) {
	e := newDispatchEnv(t, "invaliddefault")
	ctx := context.Background()
	userA := e.seedUser(t, "invalid-a@tenant.example")

	// Case 1: default points at a DISABLED shared pool.
	disabledShared := e.seedSharedPool(t, "disabled-shared")
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.pools SET enabled = FALSE WHERE id = $1`, disabledShared); err != nil {
		t.Fatalf("disable pool: %v", err)
	}
	e.setDefaultSharedPool(t, disabledShared)
	if _, ok, err := e.d.selectAccountForJob(ctx, claimedJob{id: "n/a", userID: userA}, e.mustLoadConfig(t)); err != nil || ok {
		t.Fatalf("disabled default: ok=%v err=%v, want (false, nil)", ok, err)
	}

	// Case 2: default points at a DEDICATED pool (wrong kind) — belonging to some other user.
	userB := e.seedUser(t, "invalid-b@tenant.example")
	wrongKind := e.seedDedicatedPool(t, userB, "")
	e.setDefaultSharedPool(t, wrongKind)
	if _, ok, err := e.d.selectAccountForJob(ctx, claimedJob{id: "n/a", userID: userA}, e.mustLoadConfig(t)); err != nil || ok {
		t.Fatalf("dedicated-as-default: ok=%v err=%v, want (false, nil)", ok, err)
	}

	// Case 3: default points at a NON-EXISTENT pool id.
	e.setDefaultSharedPool(t, "11111111-2222-3333-4444-555555555555")
	if _, ok, err := e.d.selectAccountForJob(ctx, claimedJob{id: "n/a", userID: userA}, e.mustLoadConfig(t)); err != nil || ok {
		t.Fatalf("nonexistent-default: ok=%v err=%v, want (false, nil)", ok, err)
	}
}

// TestPoolChain_NotConfiguredDiagnosticIsThrottled (review finding, spec §4.3 / T10 assertion 5)
// — the shared_pool_not_configured internal diagnostic fires on the FIRST empty-chain resolution
// but NOT again immediately after (rate-limited), and DOES fire again once the throttle window
// has genuinely elapsed — proving the gate is time-based, not "never again" or "always."
func TestPoolChain_NotConfiguredDiagnosticIsThrottled(t *testing.T) {
	e := newDispatchEnv(t, "diagthrottle")
	ctx := context.Background()
	userA := e.seedUser(t, "diag-a@tenant.example")
	e.clearDefaultSharedPool(t)

	if !e.d.lastSharedPoolNotConfiguredLog.IsZero() {
		t.Fatal("precondition: lastSharedPoolNotConfiguredLog should start zero")
	}
	if _, _, err := e.d.selectAccountForJob(ctx, claimedJob{id: "n/a", userID: userA}, e.mustLoadConfig(t)); err != nil {
		t.Fatalf("selectAccountForJob: %v", err)
	}
	first := e.d.lastSharedPoolNotConfiguredLog
	if first.IsZero() {
		t.Fatal("the diagnostic never fired on the FIRST empty-chain resolution")
	}

	// A second empty-chain resolution immediately after must NOT update the timestamp (throttled).
	if _, _, err := e.d.selectAccountForJob(ctx, claimedJob{id: "n/a", userID: userA}, e.mustLoadConfig(t)); err != nil {
		t.Fatalf("selectAccountForJob (2nd): %v", err)
	}
	if e.d.lastSharedPoolNotConfiguredLog != first {
		t.Fatal("the diagnostic fired again immediately — it must be rate-limited, not once-per-call")
	}

	// Once the throttle window has genuinely elapsed (simulated by backdating the timestamp), the
	// NEXT empty-chain resolution must fire it again — proving the gate is time-based, not a
	// one-shot latch.
	e.d.diagMu.Lock()
	e.d.lastSharedPoolNotConfiguredLog = time.Now().Add(-2 * sharedPoolNotConfiguredLogInterval)
	e.d.diagMu.Unlock()
	if _, _, err := e.d.selectAccountForJob(ctx, claimedJob{id: "n/a", userID: userA}, e.mustLoadConfig(t)); err != nil {
		t.Fatalf("selectAccountForJob (3rd): %v", err)
	}
	if !e.d.lastSharedPoolNotConfiguredLog.After(first) {
		t.Fatal("the diagnostic did not fire again after the throttle window elapsed")
	}
}

// TestPoolChain_MaxFallbackHopsCaps — defense-in-depth: the resolved chain never exceeds
// maxFallbackHops even if constructed with more candidates than that (a hypothetical future chain
// shape). Directly exercises dedupeCapped since the current dedicated+shared construction never
// naturally produces more than 2 entries.
func TestPoolChain_MaxFallbackHopsCaps(t *testing.T) {
	ids := make([]string, 0, maxFallbackHops+5)
	for i := 0; i < maxFallbackHops+5; i++ {
		ids = append(ids, "pool-"+string(rune('a'+i)))
	}
	got := dedupeCapped(ids, maxFallbackHops)
	if len(got) != maxFallbackHops {
		t.Fatalf("dedupeCapped returned %d entries, want exactly %d (the cap)", len(got), maxFallbackHops)
	}
	// Anti-vacuity: duplicates collapse rather than each counting toward the cap independently.
	dup := append(append([]string{}, ids[:3]...), ids[:3]...) // 3 unique, each repeated once = 6 entries, 3 unique
	got2 := dedupeCapped(dup, maxFallbackHops)
	if len(got2) != 3 {
		t.Fatalf("dedupeCapped(dup)=%d entries, want 3 (duplicates collapsed, not double-counted)", len(got2))
	}
}
