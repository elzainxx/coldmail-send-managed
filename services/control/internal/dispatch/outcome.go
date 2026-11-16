package dispatch

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OutcomeHandler applies a worker `report` to the DB (§5.3–§5.5). It is installed as the stream
// server's ReportHandler. It NEVER invents an outcome (Rule 5) — it only acts on what the worker
// reported — and it NEVER lets the credential reach send_logs/job_events/jobs (§5.5).
//
// CONFIG FRESHNESS (Phase 9.5 fix, discovered while wiring the B2 shadow-mode/kill-switch keys):
// Handle loads Config FRESH on every call (LoadConfig(ctx, h.db)), exactly like Tick/ReclaimTick,
// and threads it explicitly through every apply/effect function below — none of them read a cached
// field anymore. Before this fix the handler captured ONE Config snapshot at construction and
// reused it for the Control's entire lifetime, so an operator write to quarantine_threshold,
// throttle_pause_seconds, the backoff ladder, or (newly) autoquarantine_mode/autopause_enabled
// would silently NOT take effect on this path until a restart — contradicting §0's "a config
// change takes effect on the very next attempt, no redeploy" contract, and specifically §2.1's "no
// restart" promotion requirement for the shadow ladder. The constructor still ACCEPTS a cfg
// parameter (kept for the many existing call sites and as the harmless pre-first-report value);
// nothing after this point reads it.
type OutcomeHandler struct {
	db *pgxpool.Pool

	// health is the OPTIONAL Phase 7 §2 outage gate (P7-3), shared with the loops when wired by
	// main.go. The handler never gates ITS OWN work on it — a report must always be attempted, since
	// a successful commit is exactly what ends an outage — it only REPORTS what it observed, so a DB
	// failure seen here pauses the loops too, and a success here resumes them promptly instead of
	// waiting out the backoff. nil in tests that don't wire it (Note/Ready are nil-safe).
	health *DBHealth
}

// NewOutcomeHandler wires the handler. The cfg parameter is accepted for call-site compatibility
// but is NOT stored or read — Handle loads a fresh Config on every call (see the freshness note
// above), so there is no stale in-memory copy to keep in sync.
func NewOutcomeHandler(db *pgxpool.Pool, cfg Config) *OutcomeHandler {
	_ = cfg
	return &OutcomeHandler{db: db}
}

// WithHealth wires the shared outage gate (§2). Returns the handler for chaining at the call site.
func (h *OutcomeHandler) WithHealth(hh *DBHealth) *OutcomeHandler {
	h.health = hh
	return h
}

// noteHealth feeds an observed DB result to the shared gate when one is wired (nil-safe).
func (h *OutcomeHandler) noteHealth(err error) {
	if h.health != nil {
		h.health.Note(err)
	}
}

// Handle processes one report for a job produced by workerID. It is idempotent: a duplicate
// accepted report for an already-sent job is a clean no-op (§2.5, §5.3). workerID attributes
// provenance in job_events.
//
// Return value (Phase 7 §1.3, P7-4 — the persist-ack contract): Handle reports whether the outcome
// is DURABLY RESOLVED, i.e. whether the worker may DROP its buffered copy. This is true when the
// outcome committed to the DB, AND (equally) when the report is a clean idempotent no-op — the job
// is already sent, or already left 'sending' (some other path resolved it) — because in every one of
// those cases the Control owns the job's final resolution and a replay would change nothing. It is
// FALSE only when a real (transport/DB) error prevented us from determining/committing the outcome:
// then the worker must RETAIN the outcome and replay it later (until a post-commit ack), which is
// exactly the DB-outage / stream-drop recovery the contract exists for. The Control NEVER acks an
// outcome it did not persist (Rule 5: never invent an outcome; and never claim one is safely stored
// when it is not).
func (h *OutcomeHandler) Handle(workerID string, rep workerproto.Report) (resolved bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Load config FRESH for this report (see the freshness note on the OutcomeHandler struct above)
	// — mirrors Tick's own LoadConfig-at-entry pattern, so quarantine_threshold, autoquarantine_mode,
	// autopause_enabled, throttle_pause_seconds, the backoff ladder, and the score weights are always
	// this call's live values, never a boot-time snapshot. A load failure here is a DB-read failure
	// exactly like jobStatus's below — do not ack, let the worker retain and replay.
	cfg, err := LoadConfig(ctx, h.db)
	if err != nil {
		h.noteHealth(err)
		log.Printf("outcome: cannot load config for job %s report: %v — NOT acked; the worker retains and will replay", rep.JobID, err)
		return false
	}

	// Idempotency gate (§5.3): if the job already has a provider_message_id, it is already sent —
	// never send/persist again. This makes a replayed/duplicate accepted a no-op.
	already, curState, assignedWorker, curToken, err := h.jobStatus(ctx, rep.JobID)
	if err != nil {
		// A DB-read failure means we do NOT know the job's state — we can neither persist nor safely
		// declare a no-op, so DO NOT ack: the worker retains + replays (P7-4). This is the DB-outage path:
		// the job stays EXACTLY as stored (sending), the Control marks nothing, and the outcome survives
		// on the worker until a post-commit ack (§2.3). Report it to the shared gate so the loops pause too.
		h.noteHealth(err)
		log.Printf("outcome: cannot read job %s: %v — NOT acked; the worker retains and will replay (§2.3, nothing invented)", rep.JobID, err)
		return false
	}
	h.noteHealth(nil) // the DB answered: strongest possible liveness proof, ends any halt promptly
	if already {
		return true // already sent; clean no-op — the Control owns it, the worker may drop the buffered outcome
	}
	// Only a job that is in-flight (sending) can transition on a report. A report for a job in any
	// other state (e.g. already requeued to queued, or unknown) is a clean no-op the worker may drop —
	// the Control has already resolved this job by another path (§2.5).
	if curState != "sending" {
		return true
	}

	// STALE-REPLAY ATTRIBUTION GUARD (Phase 7 §1.3). Outcome buffering deliberately lets an outcome
	// outlive its own attempt: the buffer TTL (box_outcome_buffer_ttl_sec, 900s) is much longer than a
	// job lease (job_max_processing_time_sec, 120s), so a job whose lease expired can be reclaimed,
	// requeued, and RE-DISPATCHED — possibly to a DIFFERENT worker — while worker B1 still holds and replays
	// its old outcome. When B1's replay finally lands, the job is legitimately 'sending' again, so the
	// `state='sending'` guard on every apply path matches, and B1's stale outcome would be applied to
	// B2's live attempt: the account B2 is using takes a penalty for a failure it never had, and B2's
	// genuine outcome is later discarded as an errNoop "already handled". That is the Control attributing
	// an outcome to an attempt that did not produce it — the same family as inventing one (Rule 5), and
	// the one hazard the replay mechanism itself introduces.
	//
	// So: apply a report only when it belongs to the worker that owns the CURRENT in-flight attempt. This
	// is deliberately conservative — it rejects only a PROVABLE mismatch (both ids known and different).
	// A NULL assigned_box_id (claimed but never pushed) is not a mismatch and is left to the apply
	// paths' existing guards, so this can never reject a legitimate outcome.
	//
	// resolved=true (the worker may DROP it): the stale copy is worthless — the job has moved on to a new
	// attempt whose outcome the Control will record — and refusing to ack would make B1 replay a
	// permanently-inapplicable outcome until its TTL for no benefit.
	if assignedWorker != nil && workerID != "" && *assignedWorker != workerID {
		log.Printf("outcome: report for job %s from worker %s does NOT match the current in-flight worker %s — stale replay from a superseded attempt; dropped without applying (never attribute an outcome to an attempt that did not produce it)", rep.JobID, workerID, *assignedWorker)
		return true
	}

	// PER-ATTEMPT IDENTITY GUARD (Phase 8 Chunk 1 — P7-OD-6, RULED-ADOPT 2026-07-26). The worker guard
	// above can only decide the DIFFERENT-worker case. When a reclaimed job is re-dispatched to the SAME
	// worker, the reporting worker IS assigned_box_id, the job IS legitimately 'sending' again, and a replay
	// of the SUPERSEDED attempt's outcome was — until this guard — indistinguishable from the live
	// attempt's report. Its consequences were real in both directions: a stale ACCEPTED would mark the
	// job sent (and write a send_log) while attempt 2 is still mid-flight, so attempt 2's genuine
	// delivery becomes a silent DUPLICATE the Control never records; a stale FAILURE would requeue the job
	// out from under the live attempt and PENALIZE the account attempt 2 is using for a failure it never
	// had — and the requeued job is then dispatched again, which is a second real send. This is DISTINCT
	// from the accepted crash-after-250 window (a worker that dies between the SMTP 250 and its report):
	// that one is unavoidable, this one is fixable, which is why it is fixed rather than accepted.
	//
	// The identity is send_managed.jobs.attempt_token (send_managed_0020): minted by the Control, incremented in the
	// one guarded assigned→sending UPDATE (dispatch.go markSending), echoed on push_job, returned
	// verbatim in the report. NOT attempt_count — Phase 5 made that saturate at max_attempts while
	// requeue is unbounded until pool exhaustion, so a long-lived job reuses the value and it cannot
	// identify an attempt.
	//
	// Conservative in exactly the same way as the worker guard: it rejects ONLY a PROVABLE mismatch (both
	// tokens non-zero and different). A zero/absent token on either side means "unknown" — a pre-0020
	// worker, or an attempt that was in flight across the migration — and falls through to the existing
	// guards, so this can never reject a legitimate outcome.
	//
	// A rejected report mutates NOTHING: we return before every apply path, so no job row is written, no
	// account is scored, no send_logs row is inserted, and no job_events row is produced. resolved=true
	// (the worker may DROP it) for the same reason as the different-worker guard: the stale copy is
	// permanently inapplicable — the job has moved to an attempt whose outcome the Control will record —
	// so refusing to ack would only make the worker replay it until its TTL for no benefit.
	// DEFENSE IN DEPTH — this pre-check is a read, so there is a window between it and the apply tx in
	// which a reclaim + re-dispatch could bump the token (exactly the same TOCTOU shape as the
	// already-sent and state gates above). Every apply path therefore ALSO carries
	// `($n = 0 OR attempt_token = 0 OR attempt_token = $n)` inside its own guarded UPDATE, so the
	// decision is re-made atomically against the row it is about to write; a token that moved in the
	// window makes the UPDATE match 0 rows → errNoop → a clean acked no-op that mutates nothing. This
	// check exists so the rejection is LOGGED and so nothing (not even a send_logs row) is attempted for
	// an outcome we already know is stale.
	if rep.AttemptToken != 0 && curToken != 0 && rep.AttemptToken != curToken {
		log.Printf("outcome: report for job %s from worker %s carries attempt token %d but the current in-flight attempt is %d — stale replay from a superseded attempt; dropped without applying (no state change, no account scoring)",
			rep.JobID, workerID, rep.AttemptToken, curToken)
		return true
	}

	// Phase 5 (§1/§2): the Control classifies the raw report into exactly one error_class, then
	// applies the per-class job + account policy in one transaction. Success has no class. Each apply
	// path returns nil on a durable commit (or a clean errNoop race) and a non-nil error only on a
	// genuine failure — mapped to resolved=false so the worker retains + replays.
	if rep.Outcome == workerproto.OutcomeAccepted {
		return h.persisted(h.applyAccepted(ctx, workerID, rep, cfg))
	}
	class := classifyReport(rep)
	switch class {
	case classBouncePermanent:
		return h.persisted(h.applyBounced(ctx, workerID, rep, cfg)) // terminal bounced, NO account penalty (§2.1, §3.3 carve-out)
	default: // auth_failure | throttle | transport_4xx | timeout | unknown → requeue + per-class account effect
		return h.persisted(h.applyClassifiedFailure(ctx, workerID, rep, class, cfg))
	}
}

// persisted maps an apply-path result to the persist-ack decision (P7-4): a nil error OR a clean
// errNoop race (the row already left 'sending' — the Control owns it) means the worker may DROP the
// buffered outcome (ack); any other error means RETAIN + replay (no ack).
//
// It also feeds the shared outage gate (§2): an apply that failed on transport pauses the loops, and
// one that committed proves the DB is back. A resolved outcome is never invented here — `persisted`
// only ever reports what the transaction actually did.
func (h *OutcomeHandler) persisted(err error) bool {
	ok := err == nil || errors.Is(err, errNoop)
	if ok {
		h.noteHealth(nil)
	} else {
		h.noteHealth(err)
	}
	return ok
}

// applyAccepted: one tx — set provider_message_id + state='sent' + completed_at + clear lease,
// write the send_logs row, and (trigger) the event. provider_message_id is written at the SAME
// commit that marks sent, so there is no window where a job is 'sent' with a null id (§5.3). A
// report claiming accepted with NO id is impossible per §4.3 (the worker would send
// error/accepted_without_id); guard anyway.
//
// daily_sent_count is NOT incremented here (Phase 4 change, §7.1/§7.3): the atomic account-consume
// (eligibility.go) increments it BEFORE the send is attempted, in the same statement that admits
// the row — that is the only ordering that prevents two concurrent workers from both passing an
// eligibility check for an account's last unit of headroom. Incrementing again on accept would
// double-count. The accepted tradeoff (§7.3): a consumed-but-never-delivered slot (send fails/
// crashes after consume) still costs headroom; refunding it is deferred to Phase 6 (P4-15).
func (h *OutcomeHandler) applyAccepted(ctx context.Context, workerID string, rep workerproto.Report, cfg Config) error {
	if rep.ProviderMessageID == "" {
		// Defensive: never mark sent without an idempotency key (§4.3 anomaly). Treat as a
		// classified failure — accepted_without_id classifies as 'unknown' (§1.1 row 8), a
		// conservative requeue with NO account penalty, which is exactly right for this anomaly.
		log.Printf("outcome: job %s accepted with empty provider_message_id — requeued as unknown (§4.3 anomaly)", rep.JobID)
		anomaly := workerproto.Report{JobID: rep.JobID, AttemptToken: rep.AttemptToken, Outcome: workerproto.OutcomeError, ErrorReason: workerproto.ReasonAcceptedWithoutID, SMTPResponse: rep.SMTPResponse}
		return h.applyClassifiedFailure(ctx, workerID, anomaly, classifyReport(anomaly), cfg)
	}
	err := h.inActorTx(ctx, "box", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE send_managed.jobs
			   SET state='sent',
			       provider_message_id=$2,
			       completed_at=NOW(),
			       lease_expires_at=NULL,
			       updated_at=NOW()
			 WHERE id=$1 AND state='sending' AND provider_message_id IS NULL
			   AND ($3 = 0 OR attempt_token = 0 OR attempt_token = $3)`,
			rep.JobID, rep.ProviderMessageID, rep.AttemptToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Raced to sent/requeued by another path; treat as already handled (no double send).
			return errNoop
		}
		// daily_sent_count is NOT touched here (see the doc comment above) — the atomic consume
		// already incremented it. On success (§3.3) the account's consecutive_failures resets to 0,
		// last_success_at=NOW(), and the status score is recomputed on the one account — the
		// symmetric counterpart to the failure recompute in applyClassifiedFailure. Read the
		// assigned account first so the recompute targets exactly the account that sent.
		var acctID *string
		if err := tx.QueryRow(ctx,
			`SELECT assigned_account_id FROM send_managed.jobs WHERE id=$1`, rep.JobID).Scan(&acctID); err != nil {
			return err
		}
		if acctID != nil {
			if _, err := h.recomputeScoreOnSuccess(ctx, tx, *acctID, cfg); err != nil {
				return err
			}
		}
		return h.writeSendLog(ctx, tx, rep.JobID, rep.SMTPResponse, cfg)
	})
	if err != nil && !errors.Is(err, errNoop) {
		log.Printf("outcome: apply accepted for job %s failed: %v", rep.JobID, err)
	}
	return err
}

// applyBounced: sending → bounced (terminal, §2.1). Sets completed_at, clears the lease,
// last_error_class='bounce_permanent'. NO account effect: a bounce is a RECIPIENT fault, so it
// does NOT set last_failure_at, does NOT touch consecutive_failures, and does NOT recompute the
// score (the LOCKED "bounce never penalizes" carve-out, §3.3 — asserted byte-for-byte by T4).
// Also writes a send_logs row for the attempt.
func (h *OutcomeHandler) applyBounced(ctx context.Context, workerID string, rep workerproto.Report, cfg Config) error {
	err := h.inActorTx(ctx, "box", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE send_managed.jobs
			   SET state='bounced',
			       completed_at=NOW(),
			       lease_expires_at=NULL,
			       last_error_class='bounce_permanent',
			       last_error_message=$2,
			       updated_at=NOW()
			 WHERE id=$1 AND state='sending'
			   AND ($3 = 0 OR attempt_token = 0 OR attempt_token = $3)`,
			rep.JobID, truncate(rep.SMTPResponse, 2000), rep.AttemptToken)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNoop
		}
		return h.writeSendLog(ctx, tx, rep.JobID, rep.SMTPResponse, cfg)
	})
	if err != nil && !errors.Is(err, errNoop) {
		log.Printf("outcome: apply bounced for job %s failed: %v", rep.JobID, err)
	}
	return err
}

// applyClassifiedFailure applies the §2.1 per-class policy for a penalizing failure
// (auth_failure | throttle | transport_4xx | timeout | unknown), in ONE transaction:
//  1. requeue the job: sending → queued (the one locked failure transition, §2.2), lease cleared,
//     last_error_class = the real class, saturating attempt_count (§6.2 — NEVER exceeds
//     max_attempts, so jobs_attempt_count_check is honored, not weakened), next_attempt_at = the
//     per-class backoff (§6.4);
//  2. apply the account effect to the assigned_account_id account (quarantine / pause / none) +
//     recompute the score, writing account_events(actor='system') on a flag change (§2, §4);
//  3. write the send_logs row for the attempt.
//
// All three commit together, so a failed account effect rolls back the job requeue too (the
// account state and the job state never disagree).
func (h *OutcomeHandler) applyClassifiedFailure(ctx context.Context, workerID string, rep workerproto.Report, class string, cfg Config) error {
	detail := rep.ErrorReason
	if rep.SMTPResponse != "" {
		detail = rep.ErrorReason + ": " + rep.SMTPResponse
	}
	err := h.inActorTx(ctx, "box", func(tx pgx.Tx) error {
		// The account this attempt used (needed for the account effect + score recompute). Read it
		// BEFORE the requeue clears assignment fields; assigned_account_id is not cleared on requeue
		// (only the worker is), but reading first keeps the account effect independent of that ordering.
		var acctID *string
		if err := tx.QueryRow(ctx,
			`SELECT assigned_account_id FROM send_managed.jobs
			  WHERE id=$1 AND state='sending'
			    AND ($2 = 0 OR attempt_token = 0 OR attempt_token = $2)`,
			rep.JobID, rep.AttemptToken).Scan(&acctID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errNoop // not sending (raced/duplicate) — nothing to apply
			}
			return err
		}

		// (0) LOCK ORDER (deadlock avoidance): acquire the account row's FOR UPDATE lock BEFORE the
		// job requeue below. The job state UPDATE fires the job_events trigger, whose INSERT carries
		// account_id and so takes a FOR KEY SHARE lock on the account row. If two concurrent outcomes
		// for the SAME account each took that shared FK lock first and THEN tried to upgrade to the
		// FOR UPDATE in applyAccountEffect, they would deadlock (each waiting for the other to release
		// KEY SHARE). Taking the exclusive account lock up front gives a single consistent order
		// (account → job) so the two transactions serialize cleanly instead of cycling. This is also
		// the lock applyAccountEffect's readAccountEffectState needs; acquiring it here (a no-op to
		// re-request the same row lock later) makes the ordering explicit at the tx entry point.
		if acctID != nil {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM send_managed.accounts WHERE id=$1 FOR UPDATE`, *acctID); err != nil {
				return err
			}
		}

		// (1) Requeue with the saturating attempt_count and the per-class backoff. The CASE bounds
		// attempt_count at max_attempts (§6.2): once at the cap it STAYS there, so the locked
		// jobs_attempt_count_check (attempt_count <= max_attempts) can never abort — no 23514 ever,
		// for any job lifetime. next_attempt_at uses the post-increment attempt as the backoff index.
		//
		// THE ATTEMPT CAP (send_managed_0028, M2-G2) is decided in this SAME statement. This failure is a
		// real failed SMTP attempt, so it bumps failed_attempt_count / stamps first_failed_at (attempt_count
		// cannot do this job: it saturates at max_attempts and is also bumped by dispatcher-side requeues
		// that never reached SMTP), and if it is the job_max_failed_attempts-th — or the job's first failure
		// is at least job_failed_max_age_sec old — the row goes to the terminal `failed` instead of back to
		// `queued`, with completed_at and terminal_reason='attempts_exhausted'. See attempts_cap.go for the
		// predicate and why it is one expression. The state guard (`state='sending'` + the attempt-token
		// guard) is the one this path always had, so a duplicate or stale report still matches no row.
		//
		// RETURNING the resulting state, not the one we asked for: the cancel guard trigger
		// (send_managed_0028) may turn a `queued` into `cancelled` for a job whose campaign was cancelled while
		// this attempt was in flight, and the log line below must say what actually happened.
		exhausted := attemptsExhaustedSQL("TRUE", 6, 7)
		nextSecs := h.backoffSecondsForClass(class, saturatingAttempt(ctx, tx, rep.JobID), cfg)
		var resultState string
		err := tx.QueryRow(ctx,
			`UPDATE send_managed.jobs
			   SET state = CASE WHEN `+exhausted+` THEN 'failed'::send_managed.job_state ELSE 'queued'::send_managed.job_state END,
			       completed_at = CASE WHEN `+exhausted+` THEN NOW() ELSE NULL END,
			       terminal_reason = CASE WHEN `+exhausted+` THEN 'attempts_exhausted' ELSE NULL END,
			       lease_expires_at=NULL,
			       last_error_class=$3,
			       last_error_message=$2,
			       attempt_count = LEAST(attempt_count + 1, max_attempts),
			       failed_attempt_count = failed_attempt_count + 1,
			       first_failed_at = COALESCE(first_failed_at, NOW()),
			       next_attempt_at = NOW() + make_interval(secs => $4),
			       updated_at=NOW()
			 WHERE id=$1 AND state='sending'
			   AND ($5 = 0 OR attempt_token = 0 OR attempt_token = $5)
			 RETURNING state::text`,
			rep.JobID, truncate(detail, 2000), class, nextSecs, rep.AttemptToken,
			cfg.JobMaxFailedAttempts, cfg.JobFailedMaxAgeSec).Scan(&resultState)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errNoop
			}
			return err
		}
		switch resultState {
		case "failed":
			log.Printf("outcome: job %s reached the attempt cap (job_max_failed_attempts=%d, job_failed_max_age_sec=%d) on a %s failure — moved to failed/attempts_exhausted; the message never reached the world, so it is not chargeable",
				rep.JobID, cfg.JobMaxFailedAttempts, cfg.JobFailedMaxAgeSec, class)
		case "cancelled":
			log.Printf("outcome: job %s failed (%s) while its campaign was cancelled — not requeued, moved to cancelled", rep.JobID, class)
		}

		// (2) Account effect + score recompute on the one account this attempt used.
		if acctID != nil {
			if err := h.applyAccountEffect(ctx, tx, *acctID, class, rep, cfg); err != nil {
				return err
			}
		}

		// (3) Log the attempt.
		return h.writeSendLog(ctx, tx, rep.JobID, rep.SMTPResponse, cfg)
	})
	if err != nil && !errors.Is(err, errNoop) {
		log.Printf("outcome: apply %s for job %s failed: %v", class, rep.JobID, err)
	}
	return err
}

// backoffSecondsForClass returns next_attempt_at's delay (seconds) for a requeued class (§6.4):
//   - transport_4xx / timeout / unknown → the exponential ladder indexed by the saturating attempt;
//   - throttle                          → throttle_pause_seconds (align with the account's window);
//   - auth_failure                      → 0 (immediately eligible; the account is quarantined so
//     routing simply won't pick it — the JOB should try a different account on the very next tick).
func (h *OutcomeHandler) backoffSecondsForClass(class string, attemptCount int, cfg Config) int {
	switch class {
	case classAuthFailure:
		return 0
	case classThrottle:
		return cfg.ThrottlePauseSeconds
	default: // transport_4xx | timeout | unknown
		return cfg.Backoff.backoffForAttempt(attemptCount)
	}
}

// saturatingAttempt reads the job's current attempt_count, returning the value AFTER a saturating
// increment (min(attempt_count+1, max_attempts)) — the same value the UPDATE will store — so the
// backoff ladder is indexed by the attempt this requeue represents. Read-only; the UPDATE does the
// actual write atomically.
func saturatingAttempt(ctx context.Context, tx pgx.Tx, jobID string) int {
	var attempt, max int
	if err := tx.QueryRow(ctx,
		`SELECT attempt_count, max_attempts FROM send_managed.jobs WHERE id=$1`, jobID).Scan(&attempt, &max); err != nil {
		return 1 // conservative: treat as first retry if unreadable (backoff still bounded)
	}
	if attempt+1 > max {
		return max
	}
	return attempt + 1
}

// writeSendLog inserts one send_logs row from the job's denormalized content + the report's
// smtp_response (§7). headers are the §8 set the Control built (re-derived here from the job +
// the account it used, so send_logs.headers matches the bytes sent). ttl_expires_at is
// created_at + debug_log_retention_days. The credential is NEVER written here (§5.5).
func (h *OutcomeHandler) writeSendLog(ctx context.Context, tx pgx.Tx, jobID, smtpResponse string, cfg Config) error {
	// Re-derive the sent headers from the job + its assigned account so send_logs.headers ==
	// the guaranteed §8 set. Message-ID is deterministic from job_id (§8.1 / P8-5), so it matches
	// what the worker was handed. Date is send-time; send_logs records the attempt time — acceptable
	// (the header Date lives in the message; send_logs is a debug record, Phase 1 #28).
	var recipient, subject, body string
	var replyTo *string
	var fromAddr string
	var fromName *string
	err := tx.QueryRow(ctx,
		`SELECT j.recipient_email, j.rendered_subject, j.rendered_body, j.reply_to,
		        a.from_address, a.from_name
		 FROM send_managed.jobs j
		 LEFT JOIN send_managed.accounts a ON a.id = j.assigned_account_id
		 WHERE j.id = $1`, jobID).Scan(&recipient, &subject, &body, &replyTo, &fromAddr, &fromName)
	if err != nil {
		return err
	}
	fn := ""
	if fromName != nil {
		fn = *fromName
	}
	rt := ""
	if replyTo != nil {
		rt = *replyTo
	}
	headers := buildHeaders(fn, fromAddr, recipient, subject, rt, messageIDFor(jobID, fromAddr), time.Now().UTC())

	_, err = tx.Exec(ctx,
		`INSERT INTO send_managed.send_logs
		   (job_id, user_id, recipient_email, subject, body, headers, smtp_response, ttl_expires_at)
		 SELECT j.id, j.user_id, j.recipient_email, j.rendered_subject, j.rendered_body,
		        $2::jsonb, $3, NOW() + make_interval(days => $4)
		 FROM send_managed.jobs j WHERE j.id = $1`,
		jobID, mustJSON(headers), scrubSmtpResponse(smtpResponse), cfg.DebugLogRetentionDays)
	return err
}

// jobStatus reads the FOUR facts Handle gates on: whether the job is already sent (idempotency), its
// current state, which worker the current in-flight attempt belongs to (attribution — see the
// stale-replay guard in Handle), and the current attempt's token (per-attempt identity, Phase 8
// Chunk 1 / P7-OD-6). assignedWorker is nil when the column is NULL (claimed but never pushed, or
// assignment cleared by a requeue). attemptToken is 0 for a job that has never been dispatched (or
// one whose in-flight attempt predates send_managed_0020) — Handle treats 0 as "unknown", never a mismatch.
// A missing job → (false, "", nil, 0, err).
func (h *OutcomeHandler) jobStatus(ctx context.Context, jobID string) (alreadySent bool, state string, assignedWorker *string, attemptToken int64, err error) {
	var pmid *string
	err = h.db.QueryRow(ctx,
		`SELECT provider_message_id, state::text, assigned_box_id::text, attempt_token FROM send_managed.jobs WHERE id=$1`,
		jobID).Scan(&pmid, &state, &assignedWorker, &attemptToken)
	if err != nil {
		return false, "", nil, 0, err
	}
	return pmid != nil, state, assignedWorker, attemptToken, nil
}

func (h *OutcomeHandler) inActorTx(ctx context.Context, actor string, fn func(tx pgx.Tx) error) error {
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('send_managed.actor', $1, true)", actor); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// errNoop signals an idempotent no-op (row already handled) so the tx commits cleanly without
// treating a zero-row update as an error.
var errNoop = errors.New("noop: row already in target state")
