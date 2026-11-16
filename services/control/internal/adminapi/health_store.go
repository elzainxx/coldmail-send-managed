package adminapi

// health_store.go — Phase 9.5 B3 (§3): the read-only queries backing GET /v1/admin/health. Every
// field derives from COUNT/GROUP BY over existing tables (send_managed.jobs, send_managed.boxes,
// send_managed.accounts, send_managed.job_events, send_managed.account_events) plus the live dispatch_enabled/
// autoquarantine_mode config values — NO new persisted state, NO schema change (§3.1). Read-only:
// no claim, no lock, no mutation anywhere in this file.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// healthSnapshot is the fully-resolved §3.2 response shape (JSON tags on healthResponse, the wire
// type in health_handler.go — this struct is the internal, pre-serialization data).
type healthSnapshot struct {
	GeneratedAt time.Time
	WindowSec   int

	DispatchPaused  bool
	DispatchRung    string // fix-plan item 12: the EFFECTIVE rung (both dials composed), not a raw config value
	TicksPerMin     float64
	SendsLastWindow int
	SendRatePerMin  float64

	Queue queueCounts

	Workers workerCounts

	Accounts accountCounts

	Pools []poolHeadroom

	ErrorsLastWindow map[string]int

	Shadow shadowSummary
}

type queueCounts struct {
	Queued             int
	Assigned           int
	Sending            int
	Sent               int
	Failed             int
	Bounced            int
	Cancelled          int
	WaitingForCapacity int
}

type workerCounts struct {
	Active                      int
	Stale                       int
	Disabled                    int
	OldestActiveHeartbeatAgeSec *int
}

type accountCounts struct {
	Healthy      int
	Paused       int
	Quarantined  int
	Disabled     int
	AtDailyLimit int
}

type poolHeadroom struct {
	PoolID           string
	Kind             string
	EligibleAccounts int
	Headroom         string // "ok" | "exhausted"
}

type shadowSummary struct {
	AutoquarantineMode   string
	WouldQuarantineCount int
	Recent               []shadowDecision
}

type shadowDecision struct {
	AccountID string
	Score     int
	Threshold int
	At        time.Time
}

// buildHealthSnapshot runs every read for the §3.2 response. windowSec is health_metrics_window_sec
// (already loaded by the caller); rung/autoquarantineMode are the live config values the caller
// already read (single source of truth — this function does not re-read config).
//
// THE PARAMETER IS THE RESOLVED RUNG, NOT dispatch_enabled (fix-plan item 12). It was the boolean
// until send_managed_0025. Taking the resolved rung means this function cannot compute
// `dispatch_paused` from half the inputs — which is the bug the ladder would otherwise have introduced
// here, since a `stopped` rung with dispatch_enabled=true would have reported "not paused" while
// nothing dispatched. The composition happens once, in resolveRung, at the caller.
func buildHealthSnapshot(ctx context.Context, db *pgxpool.Pool, windowSec int, rung string, autoquarantineMode string) (healthSnapshot, error) {
	snap := healthSnapshot{
		GeneratedAt:    time.Now().UTC(),
		WindowSec:      windowSec,
		DispatchPaused: rung != rungServing,
		DispatchRung:   rung,
	}

	q, err := queryQueueCounts(ctx, db)
	if err != nil {
		return snap, err
	}
	snap.Queue = q

	sends, err := queryTerminalCountInWindow(ctx, db, "sent", windowSec)
	if err != nil {
		return snap, err
	}
	snap.SendsLastWindow = sends
	snap.SendRatePerMin = perMinute(sends, windowSec)

	ticks, err := queryDispatchTicksInWindow(ctx, db, windowSec)
	if err != nil {
		return snap, err
	}
	snap.TicksPerMin = perMinute(ticks, windowSec)

	b, err := queryWorkerCounts(ctx, db)
	if err != nil {
		return snap, err
	}
	snap.Workers = b

	a, err := queryAccountCounts(ctx, db)
	if err != nil {
		return snap, err
	}
	snap.Accounts = a

	pools, err := queryPoolHeadroom(ctx, db)
	if err != nil {
		return snap, err
	}
	snap.Pools = pools

	errs, err := queryErrorsInWindow(ctx, db, windowSec)
	if err != nil {
		return snap, err
	}
	snap.ErrorsLastWindow = errs

	shadow, err := queryShadowSummary(ctx, db, windowSec, autoquarantineMode)
	if err != nil {
		return snap, err
	}
	snap.Shadow = shadow

	return snap, nil
}

// perMinute converts a count observed over windowSec into a per-minute rate. windowSec<=0 (an
// invalid config value that should never occur post-validation) safely returns 0 rather than
// dividing by zero.
func perMinute(count, windowSec int) float64 {
	if windowSec <= 0 {
		return 0
	}
	return float64(count) * 60.0 / float64(windowSec)
}

// queryQueueCounts is COUNT(*) GROUP BY state on send_managed.jobs (§3.2 "queue"), plus
// waiting_for_capacity: queued jobs whose resolved pool chain currently has ZERO eligible accounts.
// waiting_for_capacity reuses the IDENTICAL eligibility WHERE (inlined below, mirroring
// internal/dispatch's poolHasEligible/eligible_accounts predicate byte-for-byte) so this read-only
// count can never drift from the real selection logic (§3.2's own requirement) — adminapi does not
// import internal/dispatch (structural package isolation, OD-7's precedent), so the predicate is
// duplicated here as SQL, not shared as Go code across the package boundary.
func queryQueueCounts(ctx context.Context, db *pgxpool.Pool) (queueCounts, error) {
	var q queueCounts
	rows, err := db.Query(ctx, `SELECT state::text, COUNT(*) FROM send_managed.jobs GROUP BY state`)
	if err != nil {
		return q, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return q, err
		}
		switch state {
		case "queued":
			q.Queued = n
		case "assigned":
			q.Assigned = n
		case "sending":
			q.Sending = n
		case "sent":
			q.Sent = n
		case "failed":
			q.Failed = n
		case "bounced":
			q.Bounced = n
		case "cancelled":
			q.Cancelled = n
		}
	}
	if err := rows.Err(); err != nil {
		return q, err
	}

	waiting, err := queryWaitingForCapacity(ctx, db)
	if err != nil {
		return q, err
	}
	q.WaitingForCapacity = waiting
	return q, nil
}

// queryWaitingForCapacity counts QUEUED jobs whose owning user has NO pool with an eligible
// account right now. This mirrors the Phase 4/5 pre-claim gate's own "is this user's chain
// exhausted" test at read-only granularity: for each distinct user_id with queued work, resolve
// their visible pools (dedicated owned + any shared — the same visibility rule
// selectAccountForJob/resolvePoolChain use) and check eligibility across them.
func queryWaitingForCapacity(ctx context.Context, db *pgxpool.Pool) (int, error) {
	tz, err := readConfigString(ctx, db, "daily_reset_timezone")
	if err != nil {
		return 0, err
	}
	throttleSec, err := readConfigInt(ctx, db, "throttle_pause_seconds")
	if err != nil {
		return 0, err
	}
	hour, err := readConfigInt(ctx, db, "daily_reset_hour")
	if err != nil {
		return 0, err
	}
	const q = `
WITH queued_users AS (
    SELECT DISTINCT user_id FROM send_managed.jobs WHERE state = 'queued'
),
user_pools AS (
    SELECT qu.user_id, p.id AS pool_id
    FROM queued_users qu
    JOIN send_managed.pools p
      ON p.enabled = TRUE
     AND (p.owner_user_id = qu.user_id OR p.kind = 'shared')
)
SELECT COUNT(*) FROM (
    SELECT j.id
    FROM send_managed.jobs j
    WHERE j.state = 'queued'
      AND NOT EXISTS (
          SELECT 1 FROM user_pools up
          WHERE up.user_id = j.user_id
            AND EXISTS (` + eligibleAccountsSubqueryUsingCTEPoolID + `)
      )
) waiting`
	var n int
	if err := db.QueryRow(ctx, q, tz, throttleSec, hour).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// eligibleAccountsSubqueryUsingCTEPoolID is the §2.1 eligibility predicate (including the P5-13
// expired-throttle admission and the daily_reset_hour boundary offset), referencing up.pool_id
// (the enclosing CTE's column) since it is nested inside queryWaitingForCapacity's own query
// rather than invoked as a standalone statement. $1/$2/$3 bind to that OUTER query's parameters
// ($1=daily_reset_timezone, $2=throttle_pause_seconds, $3=daily_reset_hour) — see the call site
// above (db.QueryRow(ctx, q, tz, throttleSec, hour)).
const eligibleAccountsSubqueryUsingCTEPoolID = `
    SELECT 1
    FROM send_managed.accounts a
    WHERE a.pool_id = up.pool_id
      AND a.quarantined = FALSE
      AND a.disabled = FALSE
      AND (
            (a.daily_reset_day < ((NOW() - INTERVAL '1 hour' * $3) AT TIME ZONE $1)::date
                AND (a.paused = FALSE
                     OR a.pause_reason = 'daily_limit'
                     OR (a.pause_reason = 'throttle' AND a.throttle_paused_at IS NOT NULL
                         AND NOW() >= a.throttle_paused_at + INTERVAL '1 second' * $2)))
            OR
            (a.daily_reset_day = ((NOW() - INTERVAL '1 hour' * $3) AT TIME ZONE $1)::date
                AND (a.paused = FALSE
                     OR (a.pause_reason = 'throttle' AND a.throttle_paused_at IS NOT NULL
                         AND NOW() >= a.throttle_paused_at + INTERVAL '1 second' * $2))
                AND a.daily_sent_count < a.daily_limit)
          )`

// queryTerminalCountInWindow counts jobs.completed_at within the trailing windowSec that reached
// the given terminal state (used for sends_last_window: state='sent').
func queryTerminalCountInWindow(ctx context.Context, db *pgxpool.Pool, state string, windowSec int) (int, error) {
	var n int
	err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM send_managed.jobs
		 WHERE state = $1 AND completed_at >= NOW() - INTERVAL '1 second' * $2`,
		state, windowSec).Scan(&n)
	return n, err
}

// queryDispatchTicksInWindow approximates the observed dispatch cadence from job_events: every
// dispatcher-actor 'assigned' transition in the window is one claim, and ticks_per_min is reported
// as an OBSERVED THROUGHPUT proxy (distinct claim timestamps rounded to the second, a reasonable
// approximation with no dedicated tick-counter table — Phase 9.5 adds no new persisted state,
// §3.1). A tick that claimed nothing produces no event and is invisible here; this is a documented
// approximation, not an exact tick counter.
func queryDispatchTicksInWindow(ctx context.Context, db *pgxpool.Pool, windowSec int) (int, error) {
	var n int
	err := db.QueryRow(ctx,
		`SELECT COUNT(DISTINCT date_trunc('second', created_at))
		   FROM send_managed.job_events
		  WHERE actor = 'dispatcher' AND to_state = 'assigned'
		    AND created_at >= NOW() - INTERVAL '1 second' * $1`,
		windowSec).Scan(&n)
	return n, err
}

// queryWorkerCounts is COUNT(*) GROUP BY status on send_managed.boxes, plus the oldest ACTIVE worker's
// heartbeat age (a liveness signal: a large value on an "active" worker means it is close to the
// stale threshold without having crossed it yet).
func queryWorkerCounts(ctx context.Context, db *pgxpool.Pool) (workerCounts, error) {
	var b workerCounts
	rows, err := db.Query(ctx, `SELECT status::text, COUNT(*) FROM send_managed.boxes GROUP BY status`)
	if err != nil {
		return b, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return b, err
		}
		switch status {
		case "active":
			b.Active = n
		case "stale":
			b.Stale = n
		case "disabled":
			b.Disabled = n
		}
	}
	if err := rows.Err(); err != nil {
		return b, err
	}
	var ageSec *int
	if err := db.QueryRow(ctx,
		`SELECT EXTRACT(EPOCH FROM (NOW() - MIN(last_heartbeat_at)))::int
		   FROM send_managed.boxes WHERE status = 'active'`).Scan(&ageSec); err != nil {
		return b, err
	}
	b.OldestActiveHeartbeatAgeSec = ageSec
	return b, nil
}

// queryAccountCounts is a per-account-status summary (§3.2 "accounts"): healthy (none of the
// blocking flags AND under limit) / paused / quarantined / disabled / at_daily_limit. These
// buckets OVERLAP by design with at_daily_limit (a paused-for-daily_limit account counts in BOTH
// "paused" and "at_daily_limit" — the spec's example values are consistent with this: 14 paused,
// 9 at_daily_limit, not disjoint totals summing to the account count).
func queryAccountCounts(ctx context.Context, db *pgxpool.Pool) (accountCounts, error) {
	tz, err := readConfigString(ctx, db, "daily_reset_timezone")
	if err != nil {
		return accountCounts{}, err
	}
	hour, err := readConfigInt(ctx, db, "daily_reset_hour")
	if err != nil {
		return accountCounts{}, err
	}
	var a accountCounts
	err = db.QueryRow(ctx,
		`SELECT
		   COUNT(*) FILTER (WHERE paused = FALSE AND quarantined = FALSE AND disabled = FALSE
		                       AND daily_sent_count < daily_limit),
		   COUNT(*) FILTER (WHERE paused = TRUE),
		   COUNT(*) FILTER (WHERE quarantined = TRUE),
		   COUNT(*) FILTER (WHERE disabled = TRUE),
		   COUNT(*) FILTER (WHERE daily_reset_day = ((NOW() - INTERVAL '1 hour' * $2) AT TIME ZONE $1)::date
		                       AND daily_sent_count >= daily_limit)
		 FROM send_managed.accounts`, tz, hour).
		Scan(&a.Healthy, &a.Paused, &a.Quarantined, &a.Disabled, &a.AtDailyLimit)
	return a, err
}

// queryPoolHeadroom returns, per pool, the count of currently-eligible accounts and a clean
// ok/exhausted headroom label (§3.2 "pools"). Uses the SAME predicate as queryWaitingForCapacity's
// inner EXISTS, applied per-pool as a COUNT instead of an EXISTS, so "eligible_accounts" and
// "waiting_for_capacity" can never disagree about what "eligible" means.
func queryPoolHeadroom(ctx context.Context, db *pgxpool.Pool) ([]poolHeadroom, error) {
	tz, err := readConfigString(ctx, db, "daily_reset_timezone")
	if err != nil {
		return nil, err
	}
	throttleSec, err := readConfigInt(ctx, db, "throttle_pause_seconds")
	if err != nil {
		return nil, err
	}
	hour, err := readConfigInt(ctx, db, "daily_reset_hour")
	if err != nil {
		return nil, err
	}
	const q = `
SELECT p.id::text, p.kind::text,
       COUNT(a.id) FILTER (WHERE
            a.quarantined = FALSE AND a.disabled = FALSE AND (
              (a.daily_reset_day < ((NOW() - INTERVAL '1 hour' * $3) AT TIME ZONE $1)::date
                  AND (a.paused = FALSE
                       OR a.pause_reason = 'daily_limit'
                       OR (a.pause_reason = 'throttle' AND a.throttle_paused_at IS NOT NULL
                           AND NOW() >= a.throttle_paused_at + INTERVAL '1 second' * $2)))
              OR
              (a.daily_reset_day = ((NOW() - INTERVAL '1 hour' * $3) AT TIME ZONE $1)::date
                  AND (a.paused = FALSE
                       OR (a.pause_reason = 'throttle' AND a.throttle_paused_at IS NOT NULL
                           AND NOW() >= a.throttle_paused_at + INTERVAL '1 second' * $2))
                  AND a.daily_sent_count < a.daily_limit)
            )
       ) AS eligible
FROM send_managed.pools p
LEFT JOIN send_managed.accounts a ON a.pool_id = p.id
WHERE p.enabled = TRUE
GROUP BY p.id, p.kind
ORDER BY p.created_at`
	rows, err := db.Query(ctx, q, tz, throttleSec, hour)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []poolHeadroom
	for rows.Next() {
		var ph poolHeadroom
		if err := rows.Scan(&ph.PoolID, &ph.Kind, &ph.EligibleAccounts); err != nil {
			return nil, err
		}
		if ph.EligibleAccounts > 0 {
			ph.Headroom = "ok"
		} else {
			ph.Headroom = "exhausted"
		}
		out = append(out, ph)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []poolHeadroom{} // §3.2: an empty array, never a null field
	}
	return out, nil
}

// queryErrorsInWindow is COUNT job_events GROUP BY error_class within the trailing window (§3.2
// "errors_last_window"). Zero-valued classes are OMITTED (the handler fills the fixed six-class
// vocabulary with 0 defaults so the response shape is stable even when a class never fired).
//
// The `to_state`/`from_state` filter is NOT redundant with `error_class IS NOT NULL`, for two
// distinct reasons — this counter is an OPERATOR-FACING incident-sizing number (the runbook's
// "Sends are failing" triage reads it first), so over-counting is a real operational harm:
//
//	(1) HISTORY. Before send_managed_0022, send_managed_0008's triggers copied the job row's STICKY
//	    last_error_class onto EVERY event, so one requeue left four tagged rows — the requeue plus
//	    the following queued->assigned, assigned->sending, and sending->SENT. Observed live at
//	    Stage 9 of Docs/install-and-deploy.md#part-7--developing-and-testing-locally: errors_last_window read
//	    {"unknown": 4} for a single failure. job_events is append-only (by REVOKE and by trigger),
//	    so those rows were deliberately NOT rewritten; filtering here makes the metric correct
//	    IMMEDIATELY on deploy instead of only after health_metrics_window_sec rolls them out.
//	(2) DEFENSE IN DEPTH, permanently. The predicate makes the count structurally correct
//	    regardless of what any present or future writer stamps into the column: a success can never
//	    be counted as an error, because `sending -> sent` is excluded by shape.
//
// The transition shape mirrors send_managed.fn_job_event_is_error_transition (send_managed_0022) — terminal
// 'failed'/'bounced', plus a requeue (a non-genesis transition back to 'queued'). Kept in both
// places on purpose: the trigger governs what is WRITTEN, this governs what is COUNTED, and the
// counter must stay right over rows written by an older trigger.
func queryErrorsInWindow(ctx context.Context, db *pgxpool.Pool, windowSec int) (map[string]int, error) {
	rows, err := db.Query(ctx,
		`SELECT error_class::text, COUNT(*)
		   FROM send_managed.job_events
		  WHERE error_class IS NOT NULL
		    AND (to_state IN ('failed','bounced')
		         OR (to_state = 'queued' AND from_state IS NOT NULL))
		    AND created_at >= NOW() - INTERVAL '1 second' * $1
		  GROUP BY error_class`, windowSec)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var class string
		var n int
		if err := rows.Scan(&class, &n); err != nil {
			return nil, err
		}
		out[class] = n
	}
	return out, rows.Err()
}

// queryShadowSummary counts shadow would-quarantine decisions in the trailing window (§2.1's own
// marker: actor='system', action='updated', reason LIKE 'shadow_would_quarantine:%') and returns
// the most recent few, parsed back into (account_id, score, threshold, at) for the §3.2
// "shadow.recent" array.
func queryShadowSummary(ctx context.Context, db *pgxpool.Pool, windowSec int, mode string) (shadowSummary, error) {
	s := shadowSummary{AutoquarantineMode: mode}
	var count int
	if err := db.QueryRow(ctx,
		`SELECT COUNT(*) FROM send_managed.account_events
		  WHERE actor = 'system' AND action = 'updated'
		    AND reason LIKE 'shadow_would_quarantine:%'
		    AND created_at >= NOW() - INTERVAL '1 second' * $1`, windowSec).Scan(&count); err != nil {
		return s, err
	}
	s.WouldQuarantineCount = count

	const recentLimit = 10
	rows, err := db.Query(ctx,
		`SELECT account_id::text, reason, created_at FROM send_managed.account_events
		  WHERE actor = 'system' AND action = 'updated'
		    AND reason LIKE 'shadow_would_quarantine:%'
		    AND created_at >= NOW() - INTERVAL '1 second' * $1
		  ORDER BY created_at DESC LIMIT $2`, windowSec, recentLimit)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	var recent []shadowDecision
	for rows.Next() {
		var acctID *string
		var reason string
		var at time.Time
		if err := rows.Scan(&acctID, &reason, &at); err != nil {
			return s, err
		}
		score, threshold := parseShadowReason(reason)
		dec := shadowDecision{Score: score, Threshold: threshold, At: at}
		if acctID != nil {
			dec.AccountID = *acctID
		}
		recent = append(recent, dec)
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	if recent == nil {
		recent = []shadowDecision{}
	}
	s.Recent = recent
	return s, nil
}

// parseShadowReason extracts score/threshold from the §2.1 structured marker
// ("shadow_would_quarantine: score=<X> threshold=<T> trigger=<...>", written verbatim by
// account_effect.go's shadowWouldQuarantineReason). A malformed/unparseable reason yields
// (0, 0) rather than an error — this is a best-effort display field for the health endpoint, never
// a decision input, so a parse miss must not fail the whole request.
func parseShadowReason(reason string) (score, threshold int) {
	_, _ = fmt.Sscanf(reason, "shadow_would_quarantine: score=%d threshold=%d", &score, &threshold)
	return score, threshold
}

// readConfigString/readConfigInt are tiny local readers for the two config keys this file needs
// beyond what the caller already loaded (daily_reset_timezone, throttle_pause_seconds) — adminapi
// does not import internal/dispatch's LoadConfig (structural package isolation), so these mirror
// its unwrap-the-{"v":...}-envelope logic minimally, scoped to exactly the two keys used here.
func readConfigString(ctx context.Context, db *pgxpool.Pool, key string) (string, error) {
	row, err := getConfig(ctx, db, key)
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(row.Value, &s); err != nil {
		return "", err
	}
	return s, nil
}

func readConfigInt(ctx context.Context, db *pgxpool.Pool, key string) (int, error) {
	row, err := getConfig(ctx, db, key)
	if err != nil {
		return 0, err
	}
	var n int
	if err := json.Unmarshal(row.Value, &n); err != nil {
		return 0, err
	}
	return n, nil
}

// readConfigBool mirrors readConfigString/readConfigInt for the boolean keys this file/handler
// needs (dispatch_enabled).
func readConfigBool(ctx context.Context, db *pgxpool.Pool, key string) (bool, error) {
	row, err := getConfig(ctx, db, key)
	if err != nil {
		return false, err
	}
	var b bool
	if err := json.Unmarshal(row.Value, &b); err != nil {
		return false, err
	}
	return b, nil
}
