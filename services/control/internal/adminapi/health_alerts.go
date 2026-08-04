package adminapi

// health_alerts.go — Phase 9.5 B4 (§4): the three critical alert conditions surfaced in the B3
// health endpoint's active_conditions[] block, plus the optional outbound webhook. Additive,
// read-mostly (the webhook POST is the only outbound effect anywhere in this file — no job/account
// state is ever written here): buildActiveConditions issues a few small read-only queries beyond
// the already-computed B3 snapshot (§4.1's "fires when" column needs data — waiting jobs per
// exhausted pool, recent enforce-mode quarantines, currently-stale workers — that healthSnapshot does
// not carry verbatim); it never claims, locks, or mutates send_managed.jobs/accounts/workers.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// activeConditionJSON is one entry in the §4.2 active_conditions[] block.
type activeConditionJSON struct {
	Condition string         `json:"condition"`
	Severity  string         `json:"severity"`
	Detail    map[string]any `json:"detail"`
	Since     string         `json:"since"`
}

// severityCritical is the only severity §4.1's three conditions use today (all three are the
// "operator must never miss" tier the goal statement names) — a single constant rather than a
// per-condition table, since there is nothing yet to vary it by.
const severityCritical = "critical"

// buildActiveConditions evaluates the §4.1 conditions. It takes the ALREADY-COMPUTED B3 snapshot so
// it never re-derives queue/worker/account counts, but each condition's DETAIL (which pool, how many
// waiting jobs, which account, which worker, since when) needs data the snapshot does not carry
// per-row, so this issues its own small read-only queries scoped to exactly what §4.2's jsonc
// example shows. windowSec (from snap) bounds the account_quarantined lookup to the SAME
// health_metrics_window_sec the rest of the endpoint uses (§6) — a condition that fired long before
// the window but has not yet cleared is still caught by the two live-state conditions
// (pool_exhausted from the live snapshot, worker_death from the worker's CURRENT status='stale'); only
// account_quarantined has no persistent "still true" flag to re-derive from (a quarantine is a past
// event, not a current row state), so it is inherently window-scoped — an enforce quarantine seen
// once inside the window surfaces once, ages out, and does not recur unless a new one lands.
func buildActiveConditions(ctx context.Context, db *pgxpool.Pool, snap healthSnapshot) ([]activeConditionJSON, error) {
	conditions := []activeConditionJSON{}

	pe, err := queryPoolExhaustedConditions(ctx, db, snap)
	if err != nil {
		return nil, err
	}
	conditions = append(conditions, pe...)

	aq, err := queryAccountQuarantinedConditions(ctx, db, snap.WindowSec)
	if err != nil {
		return nil, err
	}
	conditions = append(conditions, aq...)

	bd, err := queryWorkerDeathConditions(ctx, db)
	if err != nil {
		return nil, err
	}
	conditions = append(conditions, bd...)

	return conditions, nil
}

// queryPoolExhaustedConditions implements §4.1 row 1: an eligible pool (snap.Pools already computed
// this — headroom=="exhausted") fires ONLY while >=1 job is waiting on it (a newly-created, never-
// used pool with zero accounts must not alarm an operator who hasn't routed anything to it yet).
// "waiting on it" is scoped to jobs whose owning user's resolved chain includes this pool (the SAME
// visibility rule queryWaitingForCapacity/queryPoolHeadroom already use) — reusing that exact
// predicate per-pool rather than a second, potentially-drifting definition of "waiting".
func queryPoolExhaustedConditions(ctx context.Context, db *pgxpool.Pool, snap healthSnapshot) ([]activeConditionJSON, error) {
	var exhausted []poolHeadroom
	for _, p := range snap.Pools {
		if p.Headroom == "exhausted" {
			exhausted = append(exhausted, p)
		}
	}
	if len(exhausted) == 0 {
		return nil, nil
	}

	var out []activeConditionJSON
	for _, p := range exhausted {
		var waiting int
		var since *time.Time
		const q = `
WITH pool_users AS (
    SELECT qu.user_id
    FROM (SELECT DISTINCT user_id FROM send_managed.jobs WHERE state = 'queued') qu
    JOIN send_managed.pools pl ON pl.id = $1
    WHERE pl.enabled = TRUE AND (pl.owner_user_id = qu.user_id OR pl.kind = 'shared')
)
SELECT COUNT(*), MIN(j.created_at)
  FROM send_managed.jobs j
  JOIN pool_users pu ON pu.user_id = j.user_id
 WHERE j.state = 'queued'`
		if err := db.QueryRow(ctx, q, p.PoolID).Scan(&waiting, &since); err != nil {
			return nil, err
		}
		if waiting == 0 {
			continue // exhausted but nothing is actually waiting on it — §4.1 does not fire
		}
		sinceStr := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		if since != nil {
			sinceStr = since.UTC().Format("2006-01-02T15:04:05Z")
		}
		out = append(out, activeConditionJSON{
			Condition: "pool_exhausted",
			Severity:  severityCritical,
			Detail:    map[string]any{"pool_id": p.PoolID, "waiting_jobs": waiting},
			Since:     sinceStr,
		})
	}
	return out, nil
}

// queryAccountQuarantinedConditions implements §4.1 row 2: a REAL enforce-mode quarantine only —
// action='quarantine', actor='system' — never a shadow observation (those are action='updated'
// with a shadow_would_quarantine: reason marker and are structurally a different action value, so
// they can never match this query). Scoped to the health window: a quarantine event is a point-in-
// time fact, not a re-derivable current condition, so "active" here means "happened within the
// window", consistent with the rest of the endpoint's windowed counters (errors_last_window, shadow
// would_quarantine_count).
func queryAccountQuarantinedConditions(ctx context.Context, db *pgxpool.Pool, windowSec int) ([]activeConditionJSON, error) {
	rows, err := db.Query(ctx,
		`SELECT account_id::text, created_at FROM send_managed.account_events
		  WHERE actor = 'system' AND action = 'quarantine'
		    AND created_at >= NOW() - INTERVAL '1 second' * $1
		  ORDER BY created_at DESC`, windowSec)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []activeConditionJSON
	for rows.Next() {
		var acctID *string
		var at time.Time
		if err := rows.Scan(&acctID, &at); err != nil {
			return nil, err
		}
		id := ""
		if acctID != nil {
			id = *acctID
		}
		out = append(out, activeConditionJSON{
			Condition: "account_quarantined",
			Severity:  severityCritical,
			Detail:    map[string]any{"account_id": id},
			Since:     at.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return out, rows.Err()
}

// queryWorkerDeathConditions implements §4.1 row 3: every worker CURRENTLY status='stale' (the live
// Phase-6 signal, not a windowed event) — a worker that reconnects (stale->active on its next
// heartbeat, workerlink/stream.go's touchHeartbeat) drops out of this query on the very next poll,
// giving the §4.2 "conditions clear automatically" property for free from the live row state.
func queryWorkerDeathConditions(ctx context.Context, db *pgxpool.Pool) ([]activeConditionJSON, error) {
	rows, err := db.Query(ctx,
		`SELECT id::text, last_heartbeat_at FROM send_managed.boxes WHERE status = 'stale' ORDER BY last_heartbeat_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []activeConditionJSON
	for rows.Next() {
		var workerID string
		var lastHB time.Time
		if err := rows.Scan(&workerID, &lastHB); err != nil {
			return nil, err
		}
		out = append(out, activeConditionJSON{
			Condition: "worker_death",
			Severity:  severityCritical,
			Detail: map[string]any{
				"box_id":            workerID,
				"last_heartbeat_at": lastHB.UTC().Format("2006-01-02T15:04:05Z"),
			},
			Since: lastHB.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}
	return out, rows.Err()
}

// --- §4.3 the optional outbound webhook ------------------------------------------------------

// webhookHTTPClient is bounded so a slow/unreachable operator endpoint can never hang the caller
// goroutine that fires it — fire-and-forget per §4.3, never blocking dispatch (dispatch itself never
// calls into this file; this bounds the health-request path's own background goroutine instead).
var webhookHTTPClient = &http.Client{Timeout: 5 * time.Second}

const webhookMaxAttempts = 3 // bounded retries per §4.3 ("retried a bounded number of times, then dropped")

// webhookPayload is the §4.3 wire shape — two-vocabulary clean (CLAUDE.md Rule 5 extends explicitly
// to this webhook): condition code, subject id, and timestamps only. No credential, no SQL, no
// internal hostname, no send content ever reaches this struct.
type webhookPayload struct {
	Condition string         `json:"condition"`
	Severity  string         `json:"severity"`
	Subject   webhookSubject `json:"subject"`
	Detail    map[string]any `json:"detail"`
	FiredAt   string         `json:"fired_at"`
	Module    string         `json:"module"`
}

type webhookSubject struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// alertDedup tracks which (condition, subject_id) pairs have already fired a webhook POST on their
// CURRENT rising edge, so a condition that stays true across many health-endpoint polls fires
// exactly once (§4.3 "dedup by (condition, subject_id) until it clears") rather than once per poll.
// Process-local and unexported: the Control is a singleton (Rule 7), so one in-memory set is the
// correct scope — no second Control can be firing webhooks concurrently, and a restart's "fires again
// once" is the documented, acceptable edge (state is not persisted; §0 adds no new persisted state).
type alertDedup struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newAlertDedup() *alertDedup { return &alertDedup{seen: map[string]bool{}} }

// rising reports whether key is a NEW rising edge (not currently marked seen) and marks it seen.
// clearStale removes keys for conditions that dropped out of the current set, so a future re-fire
// for the same subject is treated as a fresh rising edge, not a repeat of the old one.
func (a *alertDedup) rising(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen[key] {
		return false
	}
	a.seen[key] = true
	return true
}

func (a *alertDedup) clearStale(currentKeys map[string]bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k := range a.seen {
		if !currentKeys[k] {
			delete(a.seen, k)
		}
	}
}

// defaultAlertDedup is the one process-wide tracker the health handler consults (singleton Control,
// §5 — exactly one process ever calls fireWebhooks in production).
var defaultAlertDedup = newAlertDedup()

// dedupKey identifies a condition's subject for the rising-edge tracker. subject_id is whichever
// detail field names the condition's subject (pool_id / account_id / box_id, per §4.1); a condition
// this file adds later without one of those three keys degrades to the empty string, which still
// dedups correctly (one key per condition type in that case) rather than failing to build a key.
func dedupKey(c activeConditionJSON) string {
	for _, k := range []string{"pool_id", "account_id", "box_id"} {
		if v, ok := c.Detail[k]; ok {
			return c.Condition + ":" + fmt.Sprint(v)
		}
	}
	return c.Condition
}

// fireWebhooks POSTs one payload per NEW rising edge among conditions to webhookURL (§4.3). A blank
// webhookURL is the documented minimal default: this returns immediately, no HTTP attempted, no
// dedup state touched (§4.3 "unset = no webhook"). Never returns an error to the caller — a webhook
// failure is logged and dropped internally by postWebhookWithRetry, and MUST NOT fail or slow down
// the /v1/admin/health response it rides along with (each delivery attempt runs in its own
// goroutine so this function itself returns immediately).
func fireWebhooks(webhookURL string, conditions []activeConditionJSON) {
	currentKeys := make(map[string]bool, len(conditions))
	for _, c := range conditions {
		currentKeys[dedupKey(c)] = true
	}
	defaultAlertDedup.clearStale(currentKeys)

	if webhookURL == "" {
		return
	}
	for _, c := range conditions {
		key := dedupKey(c)
		if !defaultAlertDedup.rising(key) {
			continue // already fired for this subject's current occurrence — dedup holds
		}
		go postWebhookWithRetry(webhookURL, c)
	}
}

// postWebhookWithRetry POSTs the §4.3 payload, retrying a bounded number of times on failure, then
// logging and dropping (never mutates job/account state). The internal failure reason (a network
// error, a non-2xx status) never leaves this process (two-vocabulary rule: this IS the outbound
// egress boundary, CLAUDE.md Rule 5).
func postWebhookWithRetry(webhookURL string, c activeConditionJSON) {
	subjectType, subjectID := webhookSubjectFrom(c)
	payload := webhookPayload{
		Condition: c.Condition,
		Severity:  c.Severity,
		Subject:   webhookSubject{Type: subjectType, ID: subjectID},
		Detail:    c.Detail,
		FiredAt:   time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Module:    "send_managed",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		log.Printf("alert webhook: marshal failed for condition %q: %v — dropped", c.Condition, err)
		return
	}
	var lastErr error
	for attempt := 1; attempt <= webhookMaxAttempts; attempt++ {
		req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewReader(body))
		if err != nil {
			lastErr = err
			break // a malformed URL will not succeed on retry
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := webhookHTTPClient.Do(req)
		if err != nil {
			lastErr = err
		} else {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return // delivered
			}
			lastErr = fmt.Errorf("webhook endpoint returned status %d", resp.StatusCode)
		}
		if attempt < webhookMaxAttempts {
			time.Sleep(time.Duration(attempt) * time.Second)
		}
	}
	log.Printf("alert webhook: condition %q delivery failed after %d attempt(s): %v — dropped (never blocks dispatch)",
		c.Condition, webhookMaxAttempts, lastErr)
}

// webhookSubjectFrom maps a condition's detail to the §4.3 {type, id} subject shape.
func webhookSubjectFrom(c activeConditionJSON) (subjectType, subjectID string) {
	switch c.Condition {
	case "pool_exhausted":
		return "pool", fmt.Sprint(c.Detail["pool_id"])
	case "account_quarantined":
		return "account", fmt.Sprint(c.Detail["account_id"])
	case "worker_death":
		return "box", fmt.Sprint(c.Detail["box_id"])
	default:
		return "unknown", ""
	}
}
