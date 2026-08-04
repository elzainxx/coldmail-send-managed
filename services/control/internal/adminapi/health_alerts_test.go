package adminapi

// health_alerts_test.go — Phase 9.5 Chunk 4 (B4, §4), O5. Real Postgres, real HTTP against the
// actual production Server (httptest) — no mocks (CLAUDE.md Rule 3). Proves each of the three §4.1
// conditions genuinely fires and surfaces in active_conditions[], that the §4.3 webhook fires
// exactly once per rising edge (dedup holds across repeated polls), that it stays silent when
// alert_webhook_url is unset, and that a failing webhook endpoint never blocks the health response.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// findCondition returns the first active_conditions[] entry matching name ("" ok=false if none).
func findCondition(t *testing.T, resp map[string]any, name string) (map[string]any, bool) {
	t.Helper()
	arr, ok := resp["active_conditions"].([]any)
	if !ok {
		t.Fatalf("response missing active_conditions array: %+v", resp)
	}
	for _, c := range arr {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["condition"] == name {
			return cm, true
		}
	}
	return nil, false
}

// TestAlerts_PoolExhaustedFires (O5, §4.1 row 1) — an eligible pool with zero eligible accounts AND
// >=1 queued job waiting on it must appear as a pool_exhausted condition; a healthy pool must not.
func TestAlerts_PoolExhaustedFires(t *testing.T) {
	e := newTestEnv(t, "alertpool")

	exhaustedPool := e.seedHealthPool(t, "alert-pool-exhausted")
	e.seedHealthAccount(t, exhaustedPool, true, false, false, 0, 50) // only account, paused -> 0 eligible
	e.seedHealthJob(t, "queued")                                     // system user's queued job waits on this pool (shared)

	healthyPool := e.seedHealthPool(t, "alert-pool-healthy")
	e.seedHealthAccount(t, healthyPool, false, false, false, 0, 100)

	resp := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)

	cond, found := findCondition(t, resp.body, "pool_exhausted")
	if !found {
		t.Fatalf("pool_exhausted did not fire for a truly exhausted pool with a waiting job: %+v", resp.body)
	}
	if cond["detail"].(map[string]any)["pool_id"] != exhaustedPool {
		t.Fatalf("pool_exhausted fired for the wrong pool: %+v", cond)
	}
	if waiting := intField(t, cond["detail"].(map[string]any), "waiting_jobs"); waiting < 1 {
		t.Fatalf("pool_exhausted waiting_jobs=%d, want >=1", waiting)
	}
	if cond["since"] == "" || cond["since"] == nil {
		t.Fatalf("pool_exhausted missing since: %+v", cond)
	}

	// Anti-vacuity: the healthy pool must NOT also report exhausted (proves the query really
	// distinguishes real exhaustion, not "any pool exists").
	arr := resp.body["active_conditions"].([]any)
	for _, c := range arr {
		cm := c.(map[string]any)
		if cm["condition"] == "pool_exhausted" && cm["detail"].(map[string]any)["pool_id"] == healthyPool {
			t.Fatalf("pool_exhausted incorrectly fired for the healthy pool: %+v", cm)
		}
	}
}

// TestAlerts_PoolExhaustedRequiresWaitingJob — a pool with zero eligible accounts but NO queued job
// waiting on it must NOT fire (§4.1: "while >=1 job is waiting on it"). A brand-new empty pool must
// not alarm an operator who has routed nothing to it.
func TestAlerts_PoolExhaustedRequiresWaitingJob(t *testing.T) {
	e := newTestEnv(t, "alertpoolnowait")
	pool := e.seedHealthPool(t, "alert-pool-unused")
	e.seedHealthAccount(t, pool, true, false, false, 0, 50) // paused -> 0 eligible, but nothing queued

	resp := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	if _, found := findCondition(t, resp.body, "pool_exhausted"); found {
		t.Fatalf("pool_exhausted fired for an exhausted-but-unused pool (no waiting job): %+v", resp.body)
	}
}

// TestAlerts_AccountQuarantinedFires (O5, §4.1 row 2) — a REAL enforce-mode quarantine
// (account_events action='quarantine', actor='system') fires; a SHADOW would-quarantine observation
// (action='updated', reason shadow_would_quarantine:...) on a DIFFERENT account must NOT — proving
// the condition distinguishes the two exactly as §4.1 specifies.
func TestAlerts_AccountQuarantinedFires(t *testing.T) {
	e := newTestEnv(t, "alertquarantine")
	pool := e.seedHealthPool(t, "alert-quarantine-pool")
	enforced := e.seedHealthAccount(t, pool, false, false, false, 0, 100)
	shadowed := e.seedHealthAccount(t, pool, false, false, false, 0, 100)

	if _, err := e.admin.Exec(bgCtx(),
		`INSERT INTO send_managed.account_events (account_id, user_id, actor, action, reason)
		 VALUES ($1, $2, 'system', 'quarantine', 'score_below_threshold')`,
		enforced, sysUserHealth); err != nil {
		t.Fatalf("seed enforce quarantine event: %v", err)
	}
	if _, err := e.admin.Exec(bgCtx(),
		`INSERT INTO send_managed.account_events (account_id, user_id, actor, action, from_flags, to_flags, reason)
		 VALUES ($1, $2, 'system', 'updated', '{"quarantined":false}', '{"quarantined":false}',
		         'shadow_would_quarantine: score=10 threshold=30 trigger=score')`,
		shadowed, sysUserHealth); err != nil {
		t.Fatalf("seed shadow event: %v", err)
	}

	resp := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)

	cond, found := findCondition(t, resp.body, "account_quarantined")
	if !found {
		t.Fatalf("account_quarantined did not fire for a real enforce quarantine: %+v", resp.body)
	}
	if cond["detail"].(map[string]any)["account_id"] != enforced {
		t.Fatalf("account_quarantined fired for the wrong account: %+v", cond)
	}

	arr := resp.body["active_conditions"].([]any)
	for _, c := range arr {
		cm := c.(map[string]any)
		if cm["condition"] == "account_quarantined" && cm["detail"].(map[string]any)["account_id"] == shadowed {
			t.Fatalf("account_quarantined incorrectly fired for a SHADOW observation (negative control failed): %+v", cm)
		}
	}
}

// TestAlerts_WorkerDeathFires (O5, §4.1 row 3) — a worker currently status='stale' fires worker_death; an
// active worker does not. Clearing (stale->active) removes it on the very next poll (§4.2 "conditions
// clear automatically"), proven directly against the live workers row rather than the worker process.
func TestAlerts_WorkerDeathFires(t *testing.T) {
	e := newTestEnv(t, "alertworkerdeath")
	staleWorker := e.seedHealthWorker(t, "stale", 600)
	activeWorker := e.seedHealthWorker(t, "active", 5)

	resp := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	cond, found := findCondition(t, resp.body, "worker_death")
	if !found {
		t.Fatalf("worker_death did not fire for a stale worker: %+v", resp.body)
	}
	if cond["detail"].(map[string]any)["box_id"] != staleWorker {
		t.Fatalf("worker_death fired for the wrong worker: %+v", cond)
	}

	arr := resp.body["active_conditions"].([]any)
	for _, c := range arr {
		cm := c.(map[string]any)
		if cm["condition"] == "worker_death" && cm["detail"].(map[string]any)["box_id"] == activeWorker {
			t.Fatalf("worker_death incorrectly fired for the active worker: %+v", cm)
		}
	}

	// Clear: mark the worker active again (mirrors workerlink/stream.go's touchHeartbeat) and re-poll.
	if _, err := e.admin.Exec(bgCtx(),
		`UPDATE send_managed.boxes SET status='active', last_heartbeat_at=NOW() WHERE id=$1`, staleWorker); err != nil {
		t.Fatalf("clear stale: %v", err)
	}
	resp2 := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	if _, found := findCondition(t, resp2.body, "worker_death"); found {
		t.Fatalf("worker_death still present after the worker cleared to active: %+v", resp2.body)
	}
}

// webhookSink is a tiny real HTTP server (httptest) that records every POST body it receives —
// real HTTP end to end, no mock of the Control's outbound client.
type webhookSink struct {
	mu       sync.Mutex
	posts    []map[string]any
	attempts int  // incremented on EVERY request, even a failed one — proves an attempt was really made
	fail     bool // when true, every request 500s (for the "webhook failure never blocks" test)
	srv      *httptest.Server
}

func newWebhookSink(t *testing.T) *webhookSink {
	t.Helper()
	s := &webhookSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.attempts++
		s.mu.Unlock()
		if s.fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		s.posts = append(s.posts, body)
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *webhookSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.posts)
}

func (s *webhookSink) attemptCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func (s *webhookSink) posted() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.posts))
	copy(out, s.posts)
	return out
}

// waitForCount polls until count() reaches at least n or the deadline passes.
func (s *webhookSink) waitForCount(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.count() >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("webhook sink received %d POST(s) within %s, want >= %d", s.count(), timeout, n)
}

// waitForAttempts polls until attemptCount() reaches at least n or the deadline passes — used where
// the sink is configured to fail every request, so a delivered post is never recorded but an
// attempt still is.
func (s *webhookSink) waitForAttempts(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.attemptCount() >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("webhook sink received %d attempt(s) within %s, want >= %d", s.attemptCount(), timeout, n)
}

// TestAlerts_WebhookFiresOncePerRisingEdge (O5, §4.3) — with alert_webhook_url set, a worker_death
// condition produces EXACTLY ONE webhook POST even across several repeated health polls (dedup by
// (condition, subject_id) holds); the payload is two-vocabulary clean.
func TestAlerts_WebhookFiresOncePerRisingEdge(t *testing.T) {
	e := newTestEnv(t, "webhookrising")
	sink := newWebhookSink(t)

	e.do(t, "PUT", "/v1/admin/config/alert_webhook_url", map[string]any{"value": sink.srv.URL}).requireStatus(t, 200)

	workerID := e.seedHealthWorker(t, "stale", 600)

	// Poll the health endpoint several times — the condition stays true throughout, but dedup must
	// hold: exactly one webhook POST for this worker, not one per poll.
	for i := 0; i < 4; i++ {
		e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	}
	sink.waitForCount(t, 1, 3*time.Second)
	time.Sleep(200 * time.Millisecond) // give any (incorrect) extra fires a chance to land
	if got := sink.count(); got != 1 {
		t.Fatalf("webhook fired %d time(s) across 4 polls of an unchanged condition, want exactly 1 (dedup)", got)
	}

	posts := sink.posted()
	p := posts[0]
	if p["condition"] != "worker_death" {
		t.Fatalf("webhook payload condition=%v, want worker_death: %+v", p["condition"], p)
	}
	subj, ok := p["subject"].(map[string]any)
	if !ok || subj["type"] != "box" || subj["id"] != workerID {
		t.Fatalf("webhook payload subject wrong: %+v", p)
	}
	if p["fired_at"] == "" || p["fired_at"] == nil {
		t.Fatalf("webhook payload missing fired_at: %+v", p)
	}
	if p["module"] != "send_managed" {
		t.Fatalf("webhook payload module=%v, want send_managed: %+v", p["module"], p)
	}
	// Two-vocabulary: only the documented clean fields, never a credential/SQL/hostname/content key.
	for k := range p {
		switch k {
		case "condition", "severity", "subject", "detail", "fired_at", "module":
		default:
			t.Fatalf("webhook payload carries an undocumented field %q (two-vocabulary risk): %+v", k, p)
		}
	}
}

// TestAlerts_WebhookSilentWhenUnset (O5, §4.3) — the documented minimal default: with
// alert_webhook_url at its seeded "" default, a real condition still surfaces in the endpoint but
// NO webhook POST is ever attempted.
func TestAlerts_WebhookSilentWhenUnset(t *testing.T) {
	e := newTestEnv(t, "webhookunset")
	sink := newWebhookSink(t)
	// Deliberately do NOT set alert_webhook_url — leave it at the seeded "" default.

	e.seedHealthWorker(t, "stale", 600)
	resp := e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	if _, found := findCondition(t, resp.body, "worker_death"); !found {
		t.Fatalf("worker_death should still surface in the endpoint with no webhook configured: %+v", resp.body)
	}

	time.Sleep(300 * time.Millisecond) // give a wrongly-fired webhook a chance to land
	if got := sink.count(); got != 0 {
		t.Fatalf("webhook sink received %d POST(s) with alert_webhook_url unset, want 0", got)
	}
}

// TestAlerts_WebhookFailureNeverBlocksHealthResponse (O5, §4.3) — a webhook endpoint that always
// fails must not slow down or fail the /v1/admin/health response it rides along with.
func TestAlerts_WebhookFailureNeverBlocksHealthResponse(t *testing.T) {
	e := newTestEnv(t, "webhookfail")
	sink := newWebhookSink(t)
	sink.fail = true

	e.do(t, "PUT", "/v1/admin/config/alert_webhook_url", map[string]any{"value": sink.srv.URL}).requireStatus(t, 200)
	e.seedHealthWorker(t, "stale", 600)

	start := time.Now()
	e.do(t, "GET", "/v1/admin/health", nil).requireStatus(t, 200)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("health response took %s with a failing webhook configured — it must not block", elapsed)
	}

	// Anti-vacuity: the sink DID receive attempts (proves the webhook was really tried, not just
	// skipped) — it just never affected the response above.
	sink.waitForAttempts(t, 1, 5*time.Second)
}
