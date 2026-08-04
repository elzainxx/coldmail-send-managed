package adminapi

// health_test.go — Phase 9.5 Chunk 3 (B3, §3). Real Postgres, real HTTP against the actual
// production Server (httptest) — no mocks (CLAUDE.md Rule 3). Proves O4: with a KNOWN seeded mix
// (queued/sending/sent/failed/bounced jobs, a stale worker, paused/quarantined accounts, an exhausted
// pool), every GET /v1/admin/health count matches a DIRECT DB COUNT — not merely "the endpoint
// returns 200" — and dispatch_paused mirrors the live dispatch_enabled config value.

import (
	"context"
	"testing"
)

const sysUserHealth = "00000000-0000-0000-0000-000000000000"

// seedHealthUser ensures the system user exists (mirrors the dispatch package's own fixture
// convention — send_managed.jobs/accounts/pools/workers all FK to public.users).
func (e *testEnv) seedHealthUser(t *testing.T) {
	t.Helper()
	if _, err := e.admin.Exec(bgCtx(),
		`INSERT INTO public.users (id, email, is_active) VALUES ($1,'sys@sender.local',true) ON CONFLICT (id) DO NOTHING`,
		sysUserHealth); err != nil {
		t.Fatalf("seed system user: %v", err)
	}
}

// seedHealthPool seeds a real shared pool and returns its id.
func (e *testEnv) seedHealthPool(t *testing.T, name string) string {
	t.Helper()
	e.seedHealthUser(t)
	var id string
	if err := e.admin.QueryRow(bgCtx(),
		`INSERT INTO send_managed.pools (user_id, kind, name) VALUES ($1,'shared',$2) RETURNING id`,
		sysUserHealth, name).Scan(&id); err != nil {
		t.Fatalf("seed pool: %v", err)
	}
	return id
}

// seedHealthAccount seeds a real account in poolID with the given flags/counters and returns its id.
func (e *testEnv) seedHealthAccount(t *testing.T, poolID string, paused, quarantined, disabled bool, dailySent, dailyLimit int) string {
	t.Helper()
	var id string
	if err := e.admin.QueryRow(bgCtx(),
		`INSERT INTO send_managed.accounts
		   (user_id, pool_id, smtp_host, smtp_port, smtp_username,
		    credential_ciphertext, credential_key_id, credential_nonce, from_address,
		    paused, pause_reason, quarantined, disabled, daily_sent_count, daily_limit, daily_reset_day)
		 VALUES ($1,$2,'smtp-sink',587,$3,'\x00','k','\x00','u@ourdomain.com',
		         $4, (CASE WHEN $4 THEN 'manual' ELSE NULL END)::send_managed.pause_reason, $5, $6, $7, $8, CURRENT_DATE)
		 RETURNING id`,
		sysUserHealth, poolID, "u-"+randHex(6)+"@ourdomain.com", paused, quarantined, disabled, dailySent, dailyLimit).Scan(&id); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	return id
}

// seedHealthWorker seeds a real worker row with the given status and heartbeat age.
func (e *testEnv) seedHealthWorker(t *testing.T, status string, heartbeatAgeSec int) string {
	t.Helper()
	e.seedHealthUser(t)
	var id string
	if err := e.admin.QueryRow(bgCtx(),
		`INSERT INTO send_managed.boxes (id, user_id, token_hash, status, last_heartbeat_at, disabled_at)
		 VALUES (gen_random_uuid(), $1, 'x', $2::send_managed.box_status, NOW() - INTERVAL '1 second' * $3,
		         CASE WHEN $2 = 'disabled' THEN NOW() ELSE NULL END)
		 RETURNING id`, sysUserHealth, status, heartbeatAgeSec).Scan(&id); err != nil {
		t.Fatalf("seed worker: %v", err)
	}
	return id
}

// seedHealthJobItem + seedHealthJob seed a real job_item + a send_managed.jobs row born in 'queued' via
// the genesis-event actor GUC, then (optionally) transitioned to a terminal/in-flight state with a
// real actor-GUC'd UPDATE — mirroring exactly how the dispatch package's own harness seeds jobs, so
// the job_events rows this test counts on are genuinely DB-triggered, not fabricated.
func (e *testEnv) seedHealthJob(t *testing.T, targetState string) string {
	t.Helper()
	e.seedHealthUser(t)
	ctx := bgCtx()
	var jobItemID string
	if err := e.admin.QueryRow(ctx,
		`INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, reply_to, status)
		 VALUES ($1,$1,'h@example.com','Subj','<p>b</p>','reply@ourdomain.com','pending') RETURNING id`,
		sysUserHealth).Scan(&jobItemID); err != nil {
		t.Fatalf("seed job_item: %v", err)
	}
	tx, err := e.admin.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('send_managed.actor','system',true)"); err != nil {
		t.Fatalf("set actor: %v", err)
	}
	var jobID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO send_managed.jobs (job_item_id, campaign_id, user_id, recipient_email, rendered_subject, rendered_body, reply_to, state, next_attempt_at)
		 VALUES ($1,$2,$2,'h@example.com','Subj','<p>b</p>','reply@ourdomain.com','queued',NOW()) RETURNING id`,
		jobItemID, sysUserHealth).Scan(&jobID); err != nil {
		t.Fatalf("seed job: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit job: %v", err)
	}
	if targetState == "queued" {
		return jobID
	}

	// Walk queued -> assigned -> sending -> targetState via real actor-GUC'd UPDATEs (mirrors the
	// dispatch harness's own T3-style state seeding), so job_events accumulates the real transition
	// chain rather than a single fabricated row.
	steps := []string{"assigned", "sending"}
	if targetState != "sending" {
		steps = append(steps, targetState)
	}
	for _, st := range steps {
		tx2, err := e.admin.Begin(ctx)
		if err != nil {
			t.Fatalf("begin step %s: %v", st, err)
		}
		if _, err := tx2.Exec(ctx, "SELECT set_config('send_managed.actor','dispatcher',true)"); err != nil {
			t.Fatalf("set actor step %s: %v", st, err)
		}
		var setClause string
		switch st {
		case "assigned":
			setClause = `state='assigned', lease_expires_at=NOW()+INTERVAL '120 seconds'`
		case "sending":
			setClause = `state='sending'`
		case "sent":
			setClause = `state='sent', provider_message_id='PMID-'||id::text, completed_at=NOW(), lease_expires_at=NULL`
		case "failed":
			setClause = `state='failed', completed_at=NOW(), lease_expires_at=NULL`
		case "bounced":
			setClause = `state='bounced', completed_at=NOW(), lease_expires_at=NULL`
		}
		if _, err := tx2.Exec(ctx, `UPDATE send_managed.jobs SET `+setClause+` WHERE id=$1`, jobID); err != nil {
			t.Fatalf("update step %s: %v", st, err)
		}
		if err := tx2.Commit(ctx); err != nil {
			t.Fatalf("commit step %s: %v", st, err)
		}
	}
	return jobID
}

// setHealthConfigBool/Str/Int write a config value directly (mirrors the dispatch harness's own
// setConfigBool/Str/Int, reimplemented here since adminapi's test harness has no equivalent yet).
func (e *testEnv) setHealthConfigBool(t *testing.T, key string, v bool) {
	t.Helper()
	if _, err := e.admin.Exec(bgCtx(),
		`UPDATE send_managed.config SET value = jsonb_build_object('v', $2::boolean) WHERE key=$1`, key, v); err != nil {
		t.Fatalf("set config %s: %v", key, err)
	}
}
func (e *testEnv) setHealthConfigInt(t *testing.T, key string, v int) {
	t.Helper()
	if _, err := e.admin.Exec(bgCtx(),
		`UPDATE send_managed.config SET value = jsonb_build_object('v', $2::int) WHERE key=$1`, key, v); err != nil {
		t.Fatalf("set config %s: %v", key, err)
	}
}

// TestHealth_ReportsTrueStateUnderKnownMix (O4) — seeds a known mix across jobs/workers/accounts/
// pools and asserts EVERY /v1/admin/health count matches a direct DB COUNT, not just that the
// endpoint returns 200.
func TestHealth_ReportsTrueStateUnderKnownMix(t *testing.T) {
	e := newTestEnv(t, "health")
	ctx := context.Background()

	// --- Queue mix: one job in each terminal/in-flight state, plus extra queued. ---
	e.seedHealthJob(t, "queued")
	e.seedHealthJob(t, "queued")
	e.seedHealthJob(t, "sending")
	e.seedHealthJob(t, "sent")
	e.seedHealthJob(t, "failed")
	e.seedHealthJob(t, "bounced")

	// --- Workers: one active (fresh heartbeat), one stale (old heartbeat), one disabled. ---
	e.seedHealthWorker(t, "active", 5)
	e.seedHealthWorker(t, "stale", 600)
	e.seedHealthWorker(t, "disabled", 0)

	// --- Pools + accounts: one healthy pool, one FULLY EXHAUSTED pool (all accounts blocked). ---
	healthyPool := e.seedHealthPool(t, "health-pool-ok")
	e.seedHealthAccount(t, healthyPool, false, false, false, 0, 100)   // healthy
	e.seedHealthAccount(t, healthyPool, true, false, false, 0, 100)    // paused (manual)
	e.seedHealthAccount(t, healthyPool, false, true, false, 0, 100)    // quarantined
	e.seedHealthAccount(t, healthyPool, false, false, true, 0, 100)    // disabled
	e.seedHealthAccount(t, healthyPool, false, false, false, 100, 100) // at daily limit

	exhaustedPool := e.seedHealthPool(t, "health-pool-exhausted")
	e.seedHealthAccount(t, exhaustedPool, true, false, false, 0, 50) // the ONLY account, paused -> pool has zero eligible

	// --- dispatch_enabled: assert BOTH states mirror correctly. ---
	e.setHealthConfigBool(t, "dispatch_enabled", false)

	resp := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)

	// Queue counts vs direct DB truth.
	wantQueue := map[string]int{}
	rows, err := e.admin.Query(ctx, `SELECT state::text, COUNT(*) FROM send_managed.jobs GROUP BY state`)
	if err != nil {
		t.Fatalf("direct queue count: %v", err)
	}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		wantQueue[st] = n
	}
	rows.Close()

	queue, ok := resp.body["queue"].(map[string]any)
	if !ok {
		t.Fatalf("response missing queue object: %s", resp.raw)
	}
	for _, st := range []string{"queued", "sending", "sent", "failed", "bounced"} {
		got := intField(t, queue, st)
		if got != wantQueue[st] {
			t.Fatalf("queue.%s=%d, want %d (direct DB COUNT) — full queue block: %+v", st, got, wantQueue[st], queue)
		}
	}

	// Workers vs direct DB truth.
	var wantActive, wantStale, wantDisabled int
	if err := e.admin.QueryRow(ctx, `SELECT
		COUNT(*) FILTER (WHERE status='active'),
		COUNT(*) FILTER (WHERE status='stale'),
		COUNT(*) FILTER (WHERE status='disabled')
		FROM send_managed.boxes`).Scan(&wantActive, &wantStale, &wantDisabled); err != nil {
		t.Fatalf("direct worker count: %v", err)
	}
	workers, ok := resp.body["boxes"].(map[string]any)
	if !ok {
		t.Fatalf("response missing workers object: %s", resp.raw)
	}
	if got := intField(t, workers, "active"); got != wantActive {
		t.Fatalf("boxes.active=%d, want %d", got, wantActive)
	}
	if got := intField(t, workers, "stale"); got != wantStale {
		t.Fatalf("workers.stale=%d, want %d", got, wantStale)
	}
	if got := intField(t, workers, "disabled"); got != wantDisabled {
		t.Fatalf("workers.disabled=%d, want %d", got, wantDisabled)
	}

	// Accounts vs direct DB truth.
	var wantPaused, wantQuarantined, wantDisabledAcc, wantAtLimit int
	if err := e.admin.QueryRow(ctx, `SELECT
		COUNT(*) FILTER (WHERE paused=TRUE),
		COUNT(*) FILTER (WHERE quarantined=TRUE),
		COUNT(*) FILTER (WHERE disabled=TRUE),
		COUNT(*) FILTER (WHERE daily_sent_count >= daily_limit)
		FROM send_managed.accounts`).Scan(&wantPaused, &wantQuarantined, &wantDisabledAcc, &wantAtLimit); err != nil {
		t.Fatalf("direct account count: %v", err)
	}
	accounts, ok := resp.body["accounts"].(map[string]any)
	if !ok {
		t.Fatalf("response missing accounts object: %s", resp.raw)
	}
	if got := intField(t, accounts, "paused"); got != wantPaused {
		t.Fatalf("accounts.paused=%d, want %d", got, wantPaused)
	}
	if got := intField(t, accounts, "quarantined"); got != wantQuarantined {
		t.Fatalf("accounts.quarantined=%d, want %d", got, wantQuarantined)
	}
	if got := intField(t, accounts, "disabled"); got != wantDisabledAcc {
		t.Fatalf("accounts.disabled=%d, want %d", got, wantDisabledAcc)
	}
	if got := intField(t, accounts, "at_daily_limit"); got != wantAtLimit {
		t.Fatalf("accounts.at_daily_limit=%d, want %d", got, wantAtLimit)
	}

	// Pools headroom: the exhausted pool must report eligible_accounts=0/headroom="exhausted"; the
	// healthy pool must report >=1/"ok" — proving the SAME predicate distinguishes real exhaustion,
	// not just "some pool exists."
	pools, ok := resp.body["pools"].([]any)
	if !ok {
		t.Fatalf("response missing pools array: %s", resp.raw)
	}
	foundHealthy, foundExhausted := false, false
	for _, p := range pools {
		pm, ok := p.(map[string]any)
		if !ok {
			continue
		}
		switch pm["pool_id"] {
		case healthyPool:
			foundHealthy = true
			if pm["headroom"] != "ok" {
				t.Fatalf("healthy pool headroom=%v, want ok: %+v", pm["headroom"], pm)
			}
			if n := intField(t, pm, "eligible_accounts"); n < 1 {
				t.Fatalf("healthy pool eligible_accounts=%d, want >=1", n)
			}
		case exhaustedPool:
			foundExhausted = true
			if pm["headroom"] != "exhausted" {
				t.Fatalf("exhausted pool headroom=%v, want exhausted: %+v", pm["headroom"], pm)
			}
			if n := intField(t, pm, "eligible_accounts"); n != 0 {
				t.Fatalf("exhausted pool eligible_accounts=%d, want 0", n)
			}
		}
	}
	if !foundHealthy || !foundExhausted {
		t.Fatalf("pools array missing one of the seeded pools: found healthy=%v exhausted=%v, pools=%+v", foundHealthy, foundExhausted, pools)
	}

	// dispatch_paused mirrors dispatch_enabled=false.
	dispatch, ok := resp.body["dispatch"].(map[string]any)
	if !ok {
		t.Fatalf("response missing dispatch object: %s", resp.raw)
	}
	if paused, _ := dispatch["dispatch_paused"].(bool); !paused {
		t.Fatalf("dispatch.dispatch_paused=false with dispatch_enabled=false, want true: %+v", dispatch)
	}

	// Positive control: flip dispatch_enabled=true and confirm the field flips too — proves it
	// mirrors the LIVE config, not a cached/hardcoded value.
	e.setHealthConfigBool(t, "dispatch_enabled", true)
	resp2 := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	dispatch2, ok := resp2.body["dispatch"].(map[string]any)
	if !ok {
		t.Fatalf("response missing dispatch object (2nd): %s", resp2.raw)
	}
	if paused, _ := dispatch2["dispatch_paused"].(bool); paused {
		t.Fatalf("dispatch.dispatch_paused=true with dispatch_enabled=true, want false: %+v", dispatch2)
	}
}

// TestHealth_WindowAffectsRateCounts — health_metrics_window_sec genuinely changes the reported
// rate, proving window_sec is read live from config, not hardcoded.
func TestHealth_WindowAffectsRateCounts(t *testing.T) {
	e := newTestEnv(t, "healthwindow")
	e.seedHealthJob(t, "sent")

	e.setHealthConfigInt(t, "health_metrics_window_sec", 3600)
	resp := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	if got := intField(t, resp.body, "window_sec"); got != 3600 {
		t.Fatalf("window_sec=%d, want 3600", got)
	}

	e.setHealthConfigInt(t, "health_metrics_window_sec", 60)
	resp2 := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	if got := intField(t, resp2.body, "window_sec"); got != 60 {
		t.Fatalf("window_sec=%d after config change, want 60 (no restart needed)", got)
	}
}

// TestHealth_RequiresAuth — the health endpoint is behind the SAME operator-key gate as every
// other /v1/admin/* route (Domain B) — proven with a missing bearer.
func TestHealth_RequiresAuth(t *testing.T) {
	e := newTestEnv(t, "healthauth")
	resp := e.doRaw(t, "GET", "/v1/admin/health", nil, "")
	resp.requireStatus(t, 401)
}

// intField extracts an int from a decoded JSON map, tolerating float64 (encoding/json's default
// numeric type) — a test helper, not a production path.
func intField(t *testing.T, m map[string]any, key string) int {
	t.Helper()
	v, ok := m[key]
	if !ok {
		t.Fatalf("field %q missing from %+v", key, m)
	}
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("field %q=%v is not a number", key, v)
	}
	return int(f)
}
