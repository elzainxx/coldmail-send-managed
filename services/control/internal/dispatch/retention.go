// retention.go — Phase 7 §3: the debug-log retention sweep + derive-on-demand per-day summaries.
//
// THE LOCKED INTENT. send_logs raw content (recipient + subject + body + headers + smtp_response) is
// retention-bounded: after debug_log_retention_days it is DROPPED. Job OUTCOMES (job_events + final
// jobs states) are PERMANENT and are never rolled up, mutated, or deleted. This file's sweep deletes
// ONLY send_managed.send_logs rows — that carve-out is STRUCTURAL, not policy: the sweep issues exactly one
// DML statement, a DELETE against send_logs, and no statement here touches jobs or job_events at all.
//
// P7-1 chose SCHEMA-NEUTRAL (§3.2): there is NO send_managed.send_log_summaries table (no send_managed_0020).
// "Per-day summaries" are DERIVED ON DEMAND by aggregating the permanent job_events, bucketed by
// rollup_granularity — because every per-day OUTCOME aggregate already lives there permanently, so
// the summary source survives the raw drop by construction. The accepted V1 cost (§3.2): signal that
// lives ONLY in raw send_logs (recipient-domain distribution, smtp_response text) is not retained
// past the drop. P7-12 records the future scaling levers (job_events partitioning, then the rejected
// pre-computed table) — neither is built here.
//
// MECHANISM (§3.1, P7-7): a single-Control periodic housekeeping loop on retention_sweep_interval_ms
// (default 1h) — the same discipline as the dispatch and reclaim loops, NOT pg_cron (which would
// split ownership of send_managed.* DML across two actors and need an extension the shared instance does
// not guarantee). It is the FOURTH loop in the one singleton Control.
//
// GRANTS (ADR-0008 trap, CONFIRMED not assumed): send_managed_0009 makes send_logs write-once by REVOKING
// UPDATE and installing a BEFORE UPDATE reject trigger — but DELETE was deliberately preserved for
// exactly this sweep, and send_managed_0012 grants `SELECT, INSERT, DELETE ON send_managed.send_logs TO
// send_managed_app` with a permissive RLS policy (USING/WITH CHECK true). So the sweep needs NO new
// grant and NO additive migration; Phase 7 stays schema-neutral. The write-once trigger is BEFORE
// UPDATE only, so it does not fire on DELETE. TestRetention_SweepCanActuallyDelete proves the
// privilege holds for the real app role rather than trusting this comment.
//
// HALT-SAFE (§2/§3.1): the sweep shares the Chunk-3 DBHealth gate with the dispatch and reclaim
// loops, so a DB outage pauses it too — expired rows are simply deleted on a later tick. Retention is
// a CEILING on raw-content lifetime, not a real-time deadline, so a delayed sweep is correct.
package dispatch

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// maxSweepBatchesPerTick bounds how many batches one tick will delete before yielding to the next
// tick, so a huge accumulated backlog cannot monopolize a connection or starve the dispatch/reclaim
// loops for DB throughput (§3.1's "never competes ... for DB throughput"). A SAFETY BOUND in the
// Docs/history/open-decisions.md Phase-4-closeout class, not an operator dial: the operator-facing knobs are
// the seeded retention_delete_batch_size (rows per batch) and retention_sweep_interval_ms (cadence);
// this only caps one tick's work so the remainder rolls to the next tick, which is always correct
// because retention is a ceiling, not a deadline. With the defaults that is 1000 × 64 = 64k rows/tick.
const maxSweepBatchesPerTick = 64

// sweepTickTimeout bounds one whole sweep tick's DB work (same class of safety bound as the 30s
// timeouts on the reclaim detectors).
const sweepTickTimeout = 60 * time.Second

// RetentionTick runs one retention sweep cycle (§3.1). It re-reads config fresh (Phase 2 OD-14 — a
// change to retention_delete_batch_size / retention_sweep_interval_ms takes effect on the very next
// tick, no redeploy), then DELETEs expired send_logs rows in bounded batches until a batch comes back
// short (drained) or the per-tick batch cap is hit. Returns the Config it read (so the caller times
// the next wait) and the number of raw rows dropped this tick.
//
// The DELETE is exactly §3.1's statement, served by the Phase 1 send_logs_ttl_expires_at_idx. Postgres
// does not accept LIMIT on DELETE, so the spec's `DELETE ... LIMIT :batch` is expressed the standard
// way: a bounded sub-SELECT of ids (ctid-free, primary-key based) feeding the DELETE. FOR UPDATE
// SKIP LOCKED is deliberately NOT used — send_logs is append-only debug content with no concurrent
// writer that locks rows, so plain bounded batches are correct and cheaper.
func (d *Dispatcher) RetentionTick(ctx context.Context) (Config, int, error) {
	cfg, err := LoadConfig(ctx, d.db)
	if err != nil {
		return Config{}, 0, err
	}
	deleted, _, err := d.sweepExpiredSendLogs(ctx, cfg)
	return cfg, deleted, err
}

// sweepExpiredSendLogs is the batched DELETE loop. batch <= 0 is treated as "no sweep" defensively (a
// mis-seeded zero must not turn into an unbounded DELETE); the seeded default is 1000. It also returns
// the number of DELETE statements it issued, so the T3 test can OBSERVE that a large expired set is
// drained in several bounded batches rather than one unbounded statement (a claim about batching that
// nothing measures is not a tested property).
func (d *Dispatcher) sweepExpiredSendLogs(ctx context.Context, cfg Config) (deleted, batches int, err error) {
	batch := cfg.RetentionDeleteBatchSize
	if batch <= 0 {
		log.Printf("retention: retention_delete_batch_size is %d — skipping sweep (a zero batch must never become an unbounded DELETE)", batch)
		return 0, 0, nil
	}
	ctx, cancel := context.WithTimeout(ctx, sweepTickTimeout)
	defer cancel()

	total := 0
	for i := 0; i < maxSweepBatchesPerTick; i++ {
		// ONE bounded DELETE. Only send_managed.send_logs is touched — jobs/job_events are never named by
		// this statement, which is what makes "outcomes are permanent" structural (§3.2 guarantee).
		tag, err := d.db.Exec(ctx,
			`DELETE FROM send_managed.send_logs
			  WHERE id IN (
			        SELECT id FROM send_managed.send_logs
			         WHERE ttl_expires_at < NOW()
			         ORDER BY ttl_expires_at
			         LIMIT $1)`, batch)
		if err != nil {
			return total, i, err
		}
		n := int(tag.RowsAffected())
		total += n
		if n < batch {
			return total, i + 1, nil // drained: fewer than a full batch remained
		}
	}
	// Cap hit: say so explicitly rather than letting a truncated sweep read as "fully drained".
	log.Printf("retention: per-tick batch cap reached (%d batches × %d rows = %d dropped); remaining expired rows are swept on the next tick",
		maxSweepBatchesPerTick, batch, total)
	return total, maxSweepBatchesPerTick, nil
}

// RetentionRunner drives the §3.1 housekeeping loop: the FOURTH single-Control polling loop (beside the
// dispatch loop, the reclaim loop and the in-tick pacing), on its own low-frequency
// retention_sweep_interval_ms. No Kick — retention is a periodic sweep with no event to react to, and
// a ceiling rather than a deadline.
type RetentionRunner struct {
	d *Dispatcher
}

func NewRetentionRunner(d *Dispatcher) *RetentionRunner { return &RetentionRunner{d: d} }

// Run loops until ctx is cancelled: sweep, then sleep for the freshly-read retention_sweep_interval_ms.
// It sweeps once immediately at startup (a Control that was down past a retention boundary has a backlog
// of expired raw content to drop).
func (r *RetentionRunner) Run(ctx context.Context) {
	interval := r.tick(ctx)
	for {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			interval = r.tick(ctx)
		}
	}
}

// tick runs one RetentionTick and returns the wait before the next. Same discipline as the other two
// loops: recover a panic so a sweep failure degrades to "skip this tick" rather than crashing the
// singleton Control (Rule 7), and consult the SHARED Phase 7 §2 outage gate first so a DB outage pauses
// the sweep along with dispatch and reclaim (§3.1 halt-safe).
func (r *RetentionRunner) tick(ctx context.Context) (interval time.Duration) {
	interval = defaultTickIntervalOnLoadErr
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("retention: sweep PANIC recovered: %v — skipping this tick, will retry", rec)
		}
	}()
	if ok, wait := r.d.health.Ready(ctx); !ok {
		return wait
	}
	// ADR-0011 leadership gate (same as dispatch/reclaim): the sweep DELETES rows, so only the
	// lock-holding Control runs it. Two Controls sweeping concurrently would not corrupt anything (the
	// DELETE is idempotent by predicate), but a non-leader writing send_managed.* at all violates the
	// single-owner-of-DML property Rule 7 rests on.
	if !r.d.IsLeader() {
		r.d.logNotLeader("retention")
		return notLeaderRetryInterval
	}
	cfg, deleted, err := r.d.RetentionTick(ctx)
	if err != nil {
		if r.d.health.Note(err) {
			return r.d.health.RetryIn()
		}
		log.Printf("retention: sweep error: %v", err)
		return defaultTickIntervalOnLoadErr
	}
	r.d.health.Note(nil)
	if deleted > 0 {
		log.Printf("retention: dropped %d expired send_logs row(s) (raw content only; job_events + final job states untouched)", deleted)
	}
	return time.Duration(cfg.RetentionSweepIntervalMS) * time.Millisecond
}

// --- derive-on-demand per-day summaries (§3.2, P7-1 schema-neutral) -------------------------------

// DaySummary is one bucket of the derived summary: outcome counts for a period, computed from the
// PERMANENT job_events ledger. It carries no raw content (there is none left to carry after a sweep)
// and no credential-adjacent field — only counts, which is exactly what the locked "per-day summary"
// reports.
//
// Bucket is WALL-CLOCK TIME IN daily_reset_timezone, not UTC — see SummarizeByPeriod (P7-OD-7). A
// caller rendering it must not re-interpret it as UTC.
type DaySummary struct {
	Bucket  time.Time      // start of the bucket in daily_reset_timezone (day/hour/… per rollup_granularity)
	ByState map[string]int // to_state → count (sent/bounced/failed/queued/assigned/sending)
	ByClass map[string]int // error_class → count ("" excluded; only classified transitions)
	Total   int            // total transitions in the bucket
}

// SummarizeByPeriod derives per-period outcome summaries for one user over [from, to) by aggregating
// send_managed.job_events — the permanent ledger P7-1 relies on, NOT send_logs (whose raw content the sweep
// drops). This is the whole of the "rollup": there is no summary table to write, so a swept day still
// summarizes correctly because its outcomes were never in send_logs to begin with.
//
// The bucket width comes from the seeded rollup_granularity (config-not-code) via cfg, mapped to a
// date_trunc field. An unrecognized value is REJECTED rather than silently defaulted — a mis-seeded
// granularity that quietly became "day" would make a reporting caller believe it got hourly data.
// Served by the Phase 1 job_events (user_id, created_at) index.
//
// BUCKET BOUNDARY (P7-OD-7, RULED): a bucket is truncated in daily_reset_timezone, the module's ONE
// authoritative definition of "a day" — the same boundary the daily-limit reset, account scoring and
// pool exhaustion already use (eligibility.go, account_score.go, exhaustion.go all do
// `(NOW() AT TIME ZONE $n)::date`). created_at is TIMESTAMPTZ, and date_trunc on a TIMESTAMPTZ would
// otherwise silently truncate in the SESSION timezone — an implicit environment value, which Rule 1
// does not permit to govern behavior. Worse, a reported day whose edges did not line up with the
// capacity-reset edges would show an operator PARTIAL days of capacity: a "day" with 40 of a 100/day
// limit consumed, because the other 60 landed in the report's neighbouring bucket. Consistency with
// the existing definition beats introducing a second one, so this follows daily_reset_timezone rather
// than hardcoding UTC or seeding a new key.
//
// Mechanically: `created_at AT TIME ZONE $tz` converts the TIMESTAMPTZ to local wall-clock TIMESTAMP
// in that zone, and date_trunc on a plain TIMESTAMP is zone-independent — so the boundary is pinned
// by the parameter, never inherited from the session. The returned Bucket is therefore wall-clock in
// daily_reset_timezone (documented on DaySummary).
func (d *Dispatcher) SummarizeByPeriod(ctx context.Context, cfg Config, userID string, from, to time.Time) ([]DaySummary, error) {
	field, err := truncFieldFor(cfg.RollupGranularity)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// date_trunc's field is validated against a fixed allow-list above and interpolated as an
	// identifier-free literal — never caller-controlled text (no injection surface). The timezone is a
	// BOUND parameter ($4), not interpolated.
	rows, err := d.db.Query(ctx, fmt.Sprintf(`
		SELECT date_trunc('%s', created_at AT TIME ZONE $4) AS bucket,
		       to_state::text,
		       COALESCE(error_class::text, '') AS class,
		       COUNT(*)
		  FROM send_managed.job_events
		 WHERE user_id = $1 AND created_at >= $2 AND created_at < $3
		 GROUP BY 1, 2, 3
		 ORDER BY 1`, field), userID, from, to, cfg.DailyResetTimezone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DaySummary
	byBucket := map[time.Time]int{} // bucket → index into out
	for rows.Next() {
		var bucket time.Time
		var state, class string
		var n int
		if err := rows.Scan(&bucket, &state, &class, &n); err != nil {
			return nil, err
		}
		idx, ok := byBucket[bucket]
		if !ok {
			out = append(out, DaySummary{Bucket: bucket, ByState: map[string]int{}, ByClass: map[string]int{}})
			idx = len(out) - 1
			byBucket[bucket] = idx
		}
		out[idx].ByState[state] += n
		if class != "" {
			out[idx].ByClass[class] += n
		}
		out[idx].Total += n
	}
	return out, rows.Err()
}

// truncFieldFor maps the seeded rollup_granularity to a date_trunc field, rejecting anything outside
// the allow-list (flag, don't silently fix — and it keeps the interpolation above provably safe).
func truncFieldFor(granularity string) (string, error) {
	switch granularity {
	case "hour", "day", "week", "month":
		return granularity, nil
	default:
		return "", fmt.Errorf("rollup_granularity %q is not one of hour|day|week|month", granularity)
	}
}

// countExpiredSendLogs is a small read used by the tests (and useful in a boot log) to see how much
// expired raw content is currently pending. Read-only.
func (d *Dispatcher) countExpiredSendLogs(ctx context.Context) (int, error) {
	var n int
	err := d.db.QueryRow(ctx, `SELECT count(*) FROM send_managed.send_logs WHERE ttl_expires_at < NOW()`).Scan(&n)
	if err != nil && err != pgx.ErrNoRows {
		return 0, err
	}
	return n, nil
}
