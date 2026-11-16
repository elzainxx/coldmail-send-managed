package platformapi

// cancel.go — POST /v1/cancel (M2-G3): the platform stops a campaign.
//
// THE GAP. There was no way to stop queued work. A platform-side "cancelled" job left its module-2 items
// `queued`, where the dispatcher kept claiming and SENDING them: the only levers were operator-wide
// (dispatch_mode / dispatch_enabled), which stop every tenant at once. A user who cancels a campaign must
// be able to stop THAT campaign, now, and know exactly what happened to each item.
//
// THE CONTRACT (additive; Domain A auth; same envelope and error codes as the sibling routes):
//
//	POST /v1/cancel  {"campaign_id": <uuid>, "user_id": <uuid>}
//	200  {"campaign_id": …, "cancelled": n, "in_flight": m, "already_terminal": k}
//
//   - user_id is the TENANT selector, exactly as on /v1/run and /v1/report: every statement here is
//     scoped `WHERE campaign_id = $1 AND user_id = $2`, so another tenant's campaign is untouched, and a
//     campaign id that is foreign or unknown answers the SAME empty result {0,0,0} as a campaign with
//     nothing in it — the campaign report's own rule (a campaign id is not a resource the module allocates,
//     so "no items under this id for this tenant" is the honest answer and never an existence signal).
//   - Every NOT-YET-CLAIMED item (`queued`) becomes the terminal state `cancelled` (send_managed_0027), one
//     job_events row per transition, completed_at stamped. It can never be claimed again, and because
//     job_item_id is UNIQUE it can never be re-run under the same job_item_id (a repeat /v1/run answers
//     the existing, cancelled job).
//   - Items already `assigned`/`sending` are NOT touched in state: they FINISH and report truthfully
//     (sent / bounced / their failure path) exactly once. What cancel does to them is set the
//     cancel_requested_at flag, so that if their attempt FAILS they end as `cancelled` instead of going
//     back to `queued` for a retry — a retry after a cancel is a new send, which is what cancel promised
//     not to make. The flag changes no state, writes no event and no report; the work is done by the
//     fn_jobs_cancel_guard trigger when (and only if) such an item would otherwise return to the queue.
//   - The response is the campaign's position AFTER the call, as a partition of its non-queued items:
//     `cancelled` = items in state cancelled, `in_flight` = assigned + sending (still finishing),
//     `already_terminal` = sent + failed + bounced (settled before, or while, this ran). It is therefore
//     IDEMPOTENT in the strict sense: a second call finds nothing left to cancel, changes nothing, writes
//     nothing, and returns the same numbers (apart from in-flight items that have finished in between,
//     which move from in_flight to already_terminal, or to cancelled if their attempt failed).
//   - Cancel is never gated by the dispatch rung: stopping work must work at every rung, `stopped`
//     included, or the one moment an operator has halted the module is the moment a tenant cannot stop its
//     own campaign.
//   - Cancel acts on the items that EXIST when it runs. A job_item submitted to /v1/run for the same
//     campaign afterwards is new work and is accepted as usual; the platform that cancelled the campaign is
//     the one that decides not to submit more.
//
// WHY TWO TRANSACTIONS, and in this order. The first FLAGS every non-terminal item of the campaign and
// COMMITS; the second moves the queued ones to cancelled.
//
//	1. flag      locks the campaign's non-terminal rows in id order and sets cancel_requested_at (no state
//	             change, so no job_events row and — Rule 9 — no implicit KEY SHARE on an account).
//	2. cancel    locks the campaign's QUEUED rows in id order and moves them to cancelled (the event trigger
//	             inserts the job_events row, which takes the account's KEY SHARE).
//
// They are separate because of the account→job lock order (CLAUDE.md Rule 9, ADR-0010). The outcome
// handler takes an account's FOR UPDATE and THEN the job row; step 2 takes job rows and THEN (via the event
// trigger) the account's KEY SHARE. If one transaction did both while holding the in-flight rows it had
// just locked in step 1, an outcome for one of those rows (holding the account, wanting the row) and the
// cancel (holding the row, wanting the account) would cycle. Committing step 1 before step 2 means step 2
// holds only QUEUED rows — which no outcome path writes — so the cycle has no second edge.
//
// RACE-FREEDOM against the dispatcher. The claim takes `FOR UPDATE SKIP LOCKED` on queued, un-flagged
// rows. Either the claim wins a row first (then step 1 waits for its commit, sees the row `assigned`, and
// flags it — it is in flight and will finish), or step 1 wins (then the row is flagged and the claim skips
// it, and step 2 cancels it). There is no interleaving in which an item is both cancelled and sent: the
// only writers of `sent` require state='sending', which a cancelled item never reaches. The same row-lock
// serialization covers an in-flight outcome REQUEUEING a row while cancel runs, in either order (see
// send_managed_0028's header). Both are proven by forced interleaves, not by hope
// (internal/dispatch/cancel_test.go).

import (
	"context"
	"net/http"
	"time"
)

// cancelReq is the POST /v1/cancel body.
type cancelReq struct {
	CampaignID string `json:"campaign_id"` // the campaign to stop
	UserID     string `json:"user_id"`     // the owning tenant (OD-25 option b), REQUIRED
}

// cancelResult is the 200 body.
type cancelResult struct {
	CampaignID      string `json:"campaign_id"`
	Cancelled       int    `json:"cancelled"`
	InFlight        int    `json:"in_flight"`
	AlreadyTerminal int    `json:"already_terminal"`
}

func (s *Server) handleCancel(w http.ResponseWriter, r *http.Request) {
	var req cancelReq
	if e := decodeStrict(r.Body, &req); e != nil {
		s.fail(w, r, e)
		return
	}
	// Required vs malformed are reported distinctly (issueRequired / issueMalformed), the same vocabulary
	// /v1/run and /v1/report use for the same two fields.
	if req.CampaignID == "" {
		s.fail(w, r, newErr(codeValidationFailed, "campaign_id_required").
			withDetails(fieldDetail{Field: "campaign_id", Issue: issueRequired}))
		return
	}
	if !uuidRe.MatchString(req.CampaignID) {
		s.fail(w, r, newErr(codeValidationFailed, "campaign_id_malformed").
			withDetails(fieldDetail{Field: "campaign_id", Issue: issueMalformed}))
		return
	}
	if req.UserID == "" {
		s.fail(w, r, newErr(codeValidationFailed, "user_id_required").
			withDetails(fieldDetail{Field: "user_id", Issue: issueRequired}))
		return
	}
	if !uuidRe.MatchString(req.UserID) {
		s.fail(w, r, newErr(codeValidationFailed, "user_id_malformed").
			withDetails(fieldDetail{Field: "user_id", Issue: issueMalformed}))
		return
	}

	ctx, cancel := dbCtx(r.Context())
	defer cancel()

	res, err := s.cancelCampaign(ctx, req.CampaignID, req.UserID)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "cancel_failed"))
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// cancelFlagSQL is step 1: lock the campaign's non-terminal rows in id order and flag them. The locking
// sub-select (ORDER BY id FOR UPDATE) makes every multi-row writer of this table that is shaped the same
// way — a second cancel, dispatch.failExhaustedJobs — take its row locks in the same order, so two of
// them can wait on each other but never cycle. The state IN (...) filter is re-evaluated on the LOCKED row
// version, so a row that finished while we waited for its lock is simply not flagged.
const cancelFlagSQL = `
UPDATE send_managed.jobs
   SET cancel_requested_at = NOW(),
       updated_at = NOW()
 WHERE id IN (SELECT id FROM send_managed.jobs
               WHERE campaign_id = $1 AND user_id = $2
                 AND state IN ('queued', 'assigned', 'sending')
                 AND cancel_requested_at IS NULL
               ORDER BY id FOR UPDATE)`

// cancelQueuedSQL is step 2: move the campaign's queued rows to the terminal `cancelled`. The actor GUC
// ('system', set by the caller in the same transaction) is REQUIRED by the job_events trigger for any
// state change; terminal_reason records why the row is terminal; completed_at is REQUIRED on a terminal
// row by jobs_completed_at_valid_check.
const cancelQueuedSQL = `
UPDATE send_managed.jobs
   SET state = 'cancelled',
       completed_at = NOW(),
       terminal_reason = 'cancelled',
       updated_at = NOW()
 WHERE id IN (SELECT id FROM send_managed.jobs
               WHERE campaign_id = $1 AND user_id = $2
                 AND state = 'queued'
               ORDER BY id FOR UPDATE)`

// cancelPositionSQL reads the campaign's position after the two steps: one aggregate, one snapshot, so the
// three numbers partition a single consistent moment.
const cancelPositionSQL = `
SELECT COUNT(*) FILTER (WHERE state = 'cancelled'),
       COUNT(*) FILTER (WHERE state IN ('assigned', 'sending')),
       COUNT(*) FILTER (WHERE state IN ('sent', 'failed', 'bounced'))
  FROM send_managed.jobs
 WHERE campaign_id = $1 AND user_id = $2`

// cancelAttempts bounds the retry of a transaction the database aborted for a lock cycle or a
// serialization failure. Two multi-row writers waiting on each other is rare and the loser is simply
// retried — the operation is idempotent, so a retry is always safe.
const cancelAttempts = 3

// cancelCampaign runs the two steps and reads the resulting position. See the file header for why they
// are two transactions and in this order.
func (s *Server) cancelCampaign(ctx context.Context, campaignID, userID string) (cancelResult, error) {
	var err error
	for attempt := 0; attempt < cancelAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return cancelResult{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 50 * time.Millisecond):
			}
		}
		if err = s.cancelStep(ctx, cancelFlagSQL, campaignID, userID); err != nil {
			if isRetryableTxError(err) {
				continue
			}
			return cancelResult{}, err
		}
		if err = s.cancelStep(ctx, cancelQueuedSQL, campaignID, userID); err != nil {
			if isRetryableTxError(err) {
				continue
			}
			return cancelResult{}, err
		}
		err = nil
		break
	}
	if err != nil {
		return cancelResult{}, err
	}

	res := cancelResult{CampaignID: campaignID}
	if err := s.db.QueryRow(ctx, cancelPositionSQL, campaignID, userID).
		Scan(&res.Cancelled, &res.InFlight, &res.AlreadyTerminal); err != nil {
		return cancelResult{}, err
	}
	return res, nil
}

// cancelStep runs ONE of the two statements in its own transaction, with the actor GUC set (the job_events
// trigger refuses a state change without it; step 1 changes no state, so it is inert there, and setting it
// uniformly keeps the two steps one code path).
func (s *Server) cancelStep(ctx context.Context, sql, campaignID, userID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT set_config('send_managed.actor', 'system', true)"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, sql, campaignID, userID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// isRetryableTxError reports a deadlock (40P01) or serialization failure (40001): the two SQLSTATEs for
// which "run the whole transaction again" is the documented answer.
func isRetryableTxError(err error) bool {
	switch pgCode(err) {
	case "40P01", "40001":
		return true
	}
	return false
}
