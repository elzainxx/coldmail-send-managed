package platformapi

// campaign_report.go — Phase 9 Chunk 4 (B8, §2 of phase-9-contract-and-doc-completions.md): the
// campaign-scoped GET /v1/report?campaign_id= response. Derives ENTIRELY from send_managed.jobs state
// counts + existing per-job fields — no new state, no schema change. report remains a read/poll
// of existing data (§2 ruling).
//
// The three-way distinction a UI must render without reading internal state-machine docs (§2.3):
// waiting_for_capacity (still pending, no eligible account right now — NEVER shown as failed),
// failed (terminal — pool exhaustion OR the attempt cap; refundable, platform's policy), bounced
// (terminal, recipient-permanent — billable). All three, plus plain pending, are surfaced as aggregate
// COUNTS in totals — the per-item state stays exactly the send_managed.job_state enum value (queued/
// assigned/sending/sent/failed/bounced/cancelled); waiting_for_capacity is an AGGREGATE distinction, not
// a new per-job state (§2.3).
//
// Since send_managed_0027/0028: `cancelled` is a seventh, TERMINAL state (POST /v1/cancel) and totals
// gain `cancelled`; and every item carries `reason` + `error_class` (item_reason.go) saying WHY it is in
// its state — which is how a caller tells a pool_exhausted failure from an attempts_exhausted one.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/coldmail-send-managed/control/internal/pagecursor"
)

// campaignTotals mirrors §2.2's "totals" object.
type campaignTotals struct {
	Total              int `json:"total"`
	Sent               int `json:"sent"`
	Bounced            int `json:"bounced"`
	Failed             int `json:"failed"`
	InFlight           int `json:"in_flight"`
	Pending            int `json:"pending"`
	WaitingForCapacity int `json:"waiting_for_capacity"`
	// Cancelled (additive, send_managed_0027/0028): items POST /v1/cancel stopped. TERMINAL: it is part of
	// progress.terminal and therefore of is_complete, so a cancelled campaign reads as complete once its
	// in-flight items have finished — and it is never charged (nothing was sent).
	Cancelled int `json:"cancelled"`
}

// campaignItem mirrors one entry of §2.2's "items.results" array — the SAME clean external
// per-job shape the single-job report already returns (§6.3), never SMTP/error/credential detail.
type campaignItem struct {
	JobItemID         string  `json:"job_item_id"`
	State             string  `json:"state"`
	ProviderMessageID *string `json:"provider_message_id"`
	CompletedAt       *string `json:"completed_at"`
	AttemptCount      int     `json:"attempt_count"`
	// Reason / ErrorClass (additive, M2-G1): why the item is in this state, and the class of its last
	// failed attempt. Both are ALWAYS present as keys, null when there is nothing to say — the same
	// presence rule as provider_message_id/completed_at on this shape. Vocabulary and derivation:
	// item_reason.go. Never SMTP text, an account, or a provider internal.
	Reason     *string `json:"reason"`
	ErrorClass *string `json:"error_class"`
}

// handleCampaignReport serves GET /v1/report?campaign_id=<id> (B8, §2.1-§2.4). Scoped to the
// calling tenant exactly like the single-job report (OD-25): a campaign_id that exists but belongs
// to another tenant, or does not exist at all, matches zero rows and returns a clean EMPTY
// campaign report (total:0) rather than 404 — a campaign_id is not a single resource identity the
// way job_id/job_item_id are (the platform mints it, the module never allocates one), so "zero
// jobs under this campaign_id for this tenant" is the correct, honest answer whether the id is
// foreign, mistyped, or simply not-yet-run — never a cross-tenant existence signal either way,
// since the WHERE clause scopes to user_id in every branch.
func (s *Server) handleCampaignReport(w http.ResponseWriter, r *http.Request, campaignID, userID string) {
	if !uuidRe.MatchString(campaignID) {
		s.fail(w, r, newErr(codeValidationFailed, "campaign_id_malformed").
			withDetails(fieldDetail{Field: "campaign_id", Issue: issueMalformed}))
		return
	}
	cur, cerr := parseReportCursor(r, userID, campaignID)
	if cerr != nil {
		s.fail(w, r, cerr)
		return
	}
	pageSize := parseReportPageSize(r)

	ctx, cancel := dbCtx(r.Context())
	defer cancel()

	totals, err := s.campaignTotals(ctx, campaignID, userID)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "campaign_report_totals_failed"))
		return
	}
	dispatchPaused, err := s.readDispatchPaused(ctx)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "campaign_report_dispatch_state_failed"))
		return
	}
	// The two LIVE facts a queued item's reason can hang on. WaitingForCapacity is derived from the totals
	// already computed above: the predicate is a property of the TENANT (no eligible account in any pool it
	// can reach), not of the row, so if this campaign has any queued item and the tenant has no capacity,
	// totals.waiting_for_capacity counts every queued item and is > 0 — and if the tenant has capacity it is 0.
	// Reading it back here means "waiting" cannot mean two different things inside one response.
	items, nextCursor, err := s.campaignItems(ctx, campaignID, userID, cur, pageSize, reasonContext{
		DispatchPaused:     dispatchPaused,
		WaitingForCapacity: totals.WaitingForCapacity > 0,
	})
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "campaign_report_items_failed"))
		return
	}

	// cancelled is TERMINAL (send_managed_0027): an item the platform stopped is as settled as one that
	// was sent, so it counts toward progress and is_complete.
	terminal := totals.Sent + totals.Bounced + totals.Failed + totals.Cancelled
	fractionComplete := 0.0
	if totals.Total > 0 {
		fractionComplete = float64(terminal) / float64(totals.Total)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"campaign_id":     campaignID,
		"dispatch_paused": dispatchPaused,
		"totals":          totals,
		"progress": map[string]any{
			"terminal":          terminal,
			"fraction_complete": fractionComplete,
			"is_complete":       totals.Total > 0 && terminal == totals.Total,
		},
		"items": map[string]any{
			"results":     items,
			"next_cursor": nextCursor,
			"page_size":   pageSize,
		},
	})
}

// campaignTotals computes §2.2's totals object in ONE query, tenant-scoped. waiting_for_capacity
// reuses the IDENTICAL eligibility predicate internal/adminapi's health endpoint already uses for
// its own global waiting_for_capacity count (mirrored here rather than imported — platformapi does
// not depend on adminapi, and adminapi does not depend on internal/dispatch; each surface keeps its
// own minimal read-only copy of the §2.1 eligibility predicate, the established package-isolation
// precedent, OD-7) so "waiting" can never mean two different things across the two admin/platform
// surfaces.
func (s *Server) campaignTotals(ctx context.Context, campaignID, userID string) (campaignTotals, error) {
	tz, err := s.readConfigStringLocal(ctx, "daily_reset_timezone", "UTC")
	if err != nil {
		return campaignTotals{}, err
	}
	throttleSec, err := s.readConfigIntLocal(ctx, "throttle_pause_seconds", 900)
	if err != nil {
		return campaignTotals{}, err
	}
	hour, err := s.readConfigIntLocal(ctx, "daily_reset_hour", 0)
	if err != nil {
		return campaignTotals{}, err
	}

	const q = `
WITH scoped AS (
    SELECT j.id, j.state, j.user_id
    FROM send_managed.jobs j
    WHERE j.campaign_id = $1 AND j.user_id = $2
),
user_pools AS (
    SELECT p.id AS pool_id
    FROM send_managed.pools p
    WHERE p.enabled = TRUE
      AND (p.owner_user_id = $2::uuid OR p.kind = 'shared')
)
SELECT
    COUNT(*) AS total,
    COUNT(*) FILTER (WHERE state = 'sent') AS sent,
    COUNT(*) FILTER (WHERE state = 'bounced') AS bounced,
    COUNT(*) FILTER (WHERE state = 'failed') AS failed,
    COUNT(*) FILTER (WHERE state = 'cancelled') AS cancelled,
    COUNT(*) FILTER (WHERE state IN ('assigned', 'sending')) AS in_flight,
    COUNT(*) FILTER (WHERE state = 'queued') AS pending_total,
    COUNT(*) FILTER (WHERE state = 'queued' AND NOT EXISTS (
        SELECT 1 FROM user_pools up
        WHERE EXISTS (` + campaignEligibleAccountsSubquery + `)
    )) AS waiting_for_capacity
FROM scoped`

	var totals campaignTotals
	var pendingTotal int
	if err := s.db.QueryRow(ctx, q, campaignID, userID, tz, throttleSec, hour).Scan(
		&totals.Total, &totals.Sent, &totals.Bounced, &totals.Failed, &totals.Cancelled, &totals.InFlight,
		&pendingTotal, &totals.WaitingForCapacity); err != nil {
		return campaignTotals{}, err
	}
	// pending = queued jobs that are NOT currently waiting_for_capacity (i.e. dispatchable now).
	totals.Pending = pendingTotal - totals.WaitingForCapacity
	return totals, nil
}

// campaignEligibleAccountsSubquery is the §2.1 eligibility predicate (P5-13 expired-throttle
// admission + daily_reset_hour boundary offset), byte-identical in substance to
// internal/adminapi/health_store.go's eligibleAccountsSubqueryUsingCTEPoolID — the two surfaces
// each hold their own minimal copy (package isolation, OD-7's precedent) rather than sharing Go
// code across internal/adminapi and internal/platformapi. Binds $3=tz, $4=throttleSec, $5=hour
// (this query's parameter numbering, distinct from health_store.go's).
const campaignEligibleAccountsSubquery = `
    SELECT 1
    FROM send_managed.accounts a
    WHERE a.pool_id = up.pool_id
      AND a.quarantined = FALSE
      AND a.disabled = FALSE
      AND (
            (a.daily_reset_day < ((NOW() - INTERVAL '1 hour' * $5) AT TIME ZONE $3)::date
                AND (a.paused = FALSE
                     OR a.pause_reason = 'daily_limit'
                     OR (a.pause_reason = 'throttle' AND a.throttle_paused_at IS NOT NULL
                         AND NOW() >= a.throttle_paused_at + INTERVAL '1 second' * $4)))
            OR
            (a.daily_reset_day = ((NOW() - INTERVAL '1 hour' * $5) AT TIME ZONE $3)::date
                AND (a.paused = FALSE
                     OR (a.pause_reason = 'throttle' AND a.throttle_paused_at IS NOT NULL
                         AND NOW() >= a.throttle_paused_at + INTERVAL '1 second' * $4))
                AND a.daily_sent_count < a.daily_limit)
          )`

// reasonContext carries the two LIVE facts a queued item's `reason` can depend on (item_reason.go): they
// are properties of the module and the tenant at this moment, not of any one row, so they are computed once
// per response and handed to every item.
type reasonContext struct {
	DispatchPaused     bool
	WaitingForCapacity bool
}

// campaignItemKey is the keyset position of one returned row: the sort key pair the cursor encodes.
type campaignItemKey struct {
	createdAt time.Time
	id        string
}

// tenantHasEligibleCapacity reports whether ANY pool this tenant can reach (its own enabled dedicated
// pools plus the enabled shared ones) has an eligible account right now — the SAME predicate, byte for
// byte, as the `waiting_for_capacity` count in campaignTotals (it reuses campaignEligibleAccountsSubquery
// and the same three config reads), so a queued item can never be called "waiting" by one route and
// "pending" by the other. The single-job report uses it for a queued item's `reason`; the campaign report
// does not need it (it reads the answer back from its own totals).
//
// $1 is bound to the tenant AND referenced only through a no-op cast: campaignEligibleAccountsSubquery
// hard-codes its parameters as $3/$4/$5 to match campaignTotals' ($1 campaign, $2 tenant), and Postgres
// refuses a statement whose parameter list has a hole ("could not determine data type of parameter $1").
func (s *Server) tenantHasEligibleCapacity(ctx context.Context, userID string) (bool, error) {
	tz, err := s.readConfigStringLocal(ctx, "daily_reset_timezone", "UTC")
	if err != nil {
		return false, err
	}
	throttleSec, err := s.readConfigIntLocal(ctx, "throttle_pause_seconds", 900)
	if err != nil {
		return false, err
	}
	hour, err := s.readConfigIntLocal(ctx, "daily_reset_hour", 0)
	if err != nil {
		return false, err
	}
	const q = `
WITH user_pools AS (
    SELECT p.id AS pool_id
    FROM send_managed.pools p
    WHERE p.enabled = TRUE
      AND (p.owner_user_id = $2::uuid OR p.kind = 'shared')
)
SELECT ($1::text IS NOT NULL) AND EXISTS (
    SELECT 1 FROM user_pools up
    WHERE EXISTS (` + campaignEligibleAccountsSubquery + `)
)`
	var ok bool
	if err := s.db.QueryRow(ctx, q, userID, userID, tz, throttleSec, hour).Scan(&ok); err != nil {
		return false, err
	}
	return ok, nil
}

// campaignItems returns one page of per-job detail (§2.2 "items"), cursor-paginated on
// (created_at, id) — the same keyset pattern the admin surface uses (§5.1 pagination
// conventions), tenant-scoped.
//
// THE CURSOR IS BUILT FROM THE LAST ROW RETURNED, never from the extra one (M2-G5). The query asks for
// pageSize+1 rows for exactly one purpose — to learn whether another page exists — and the next page then
// starts STRICTLY AFTER the cursor row. This function used to keep overwriting its "last row" variables on
// every Scan, so after the loop they held the (pageSize+1)th row, which is the row that was NOT returned:
// the cursor pointed past it, and it was skipped on the next page, one item lost at every page boundary
// and silently (every page looked complete, next_cursor was well-formed, the final page ended on null).
// Safe only for a campaign of at most page_size items, which is why nothing noticed. The fix keeps every
// returned row's key and takes the cursor from the pageSize-th; the extra row is dropped after it has
// served its one purpose.
func (s *Server) campaignItems(ctx context.Context, campaignID, userID string, cur *reportCursor, pageSize int, rc reasonContext) ([]campaignItem, *string, error) {
	args := []any{campaignID, userID}
	where := "WHERE j.campaign_id = $1 AND j.user_id = $2"
	if cur != nil {
		args = append(args, cur.CreatedAt, cur.ID)
		where += " AND (j.created_at, j.id) > ($3::timestamptz, $4::uuid)"
	}
	args = append(args, pageSize+1)
	limitParam := len(args)
	q := `SELECT j.job_item_id::text, j.state::text, j.provider_message_id, j.completed_at, j.attempt_count,
	             j.last_error_class::text, j.last_error_message, j.terminal_reason,
	             j.created_at, j.id::text
	      FROM send_managed.jobs j ` + where + `
	      ORDER BY j.created_at ASC, j.id ASC
	      LIMIT $` + strconv.Itoa(limitParam)

	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	items := []campaignItem{}
	keys := []campaignItemKey{}
	for rows.Next() {
		var it campaignItem
		var completedAt *time.Time
		var lastClass, lastMsg, termReason *string
		var key campaignItemKey
		if err := rows.Scan(&it.JobItemID, &it.State, &it.ProviderMessageID, &completedAt, &it.AttemptCount,
			&lastClass, &lastMsg, &termReason,
			&key.createdAt, &key.id); err != nil {
			return nil, nil, err
		}
		if completedAt != nil {
			s := completedAt.UTC().Format("2006-01-02T15:04:05Z")
			it.CompletedAt = &s
		}
		it.Reason, it.ErrorClass = itemOutcome(itemFacts{
			State:              it.State,
			TerminalReason:     termReason,
			LastErrorClass:     lastClass,
			LastErrorMessage:   lastMsg,
			DispatchPaused:     rc.DispatchPaused,
			WaitingForCapacity: rc.WaitingForCapacity,
		})
		items = append(items, it)
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var nextCursor *string
	if len(items) > pageSize {
		// has-more comes from the extra row; the cursor comes from the last row we are RETURNING.
		last := keys[pageSize-1]
		items = items[:pageSize]
		c := encodeReportCursor(userID, campaignID, last.createdAt.Format(time.RFC3339Nano), last.id)
		nextCursor = &c
	}
	return items, nextCursor, nil
}

// readDispatchPaused reports whether dispatch is paused — "dispatch_paused" (§2.2) is the
// operator-visible framing a UI renders on the campaign report.
//
// IT READS BOTH DIALS SINCE send_managed_0025 (fix-plan item 12), and that is a BUG FIX, not a
// widening. This function used to negate dispatch_enabled alone. Once dispatch_mode exists, an
// operator who pulls `refuse_new` or `stopped` while the boolean is still true has stopped dispatch
// completely — and the old reading would have reported `dispatch_paused: false` to every UI polling a
// campaign, while not one job moved. That is precisely M1's D2 defect (a config read that looked
// normal while the system ignored it), and it would have been introduced HERE by adding the ladder
// without touching this reader. Adding a dial without revisiting what reports on it is how the
// inert-control bug gets in.
//
// So the answer is "is the effective rung anything other than serving" — the same composition
// dispatch.resolveRung performs, which by construction still returns true for every case the old
// reading returned true for (dispatch_enabled=false floors at accept_and_defer, which is paused).
// No existing client's interpretation changes; the field simply stops being wrong in the new cases.
//
// A read failure still propagates (the caller 500s the report) rather than guessing: a UI rendering
// "not paused" from a failed read is the same lie in a smaller font.
func (s *Server) readDispatchPaused(ctx context.Context) (bool, error) {
	var raw []byte
	if err := s.db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key = 'dispatch_enabled'`).Scan(&raw); err != nil {
		return false, err
	}
	var wrap struct {
		V *bool `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil {
		return false, nil
	}
	if !*wrap.V {
		return true, nil // the legacy floor: dispatch_enabled=false is at least accept_and_defer
	}
	mode, ok := s.readDispatchModeRaw(ctx)
	if !ok {
		// Unreadable/absent mode with the boolean true: report not-paused, matching the pre-0025
		// answer for exactly this input. An unrecognized VALUE is different and is handled below.
		return false, nil
	}
	if _, known := rungSeverity[mode]; !known {
		// Garbage value: the Control resolves it to accept_and_defer, which IS paused. Reporting
		// not-paused here would tell a UI that work is flowing while the Control claims nothing.
		return true, nil
	}
	return mode != rungServing, nil
}

// readConfigStringLocal/readConfigIntLocal are minimal local send_managed.config readers (mirroring
// readUnitPriceCredits's own pattern) scoped to exactly the keys campaignTotals needs, falling back
// to the seeded default on any read/parse failure — report is a read-mostly endpoint and a
// transient config blip must not 500 every poll in flight.
func (s *Server) readConfigStringLocal(ctx context.Context, key, fallback string) (string, error) {
	var raw []byte
	if err := s.db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key = $1`, key).Scan(&raw); err != nil {
		return fallback, nil
	}
	var wrap struct {
		V *string `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil {
		return fallback, nil
	}
	return *wrap.V, nil
}

func (s *Server) readConfigIntLocal(ctx context.Context, key string, fallback int) (int, error) {
	var raw []byte
	if err := s.db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key = $1`, key).Scan(&raw); err != nil {
		return fallback, nil
	}
	var wrap struct {
		V *int `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil {
		return fallback, nil
	}
	return *wrap.V, nil
}

// --- cursor helpers (campaign-report-local; mirror internal/adminapi/pagination.go's shape) ---

type reportCursor struct {
	CreatedAt string
	ID        string
}

// reportCursorScope is what a campaign-report cursor is valid for: this route, this tenant, this campaign.
// A cursor minted for any other scope is refused (pagecursor.ErrWrongScope) exactly like a malformed one.
func reportCursorScope(userID, campaignID string) string {
	return "platform.report|" + userID + "|" + campaignID
}

// parseReportCursor reads ?cursor= for the campaign report (Phase 3 close-out B2). A bad, malformed or
// forged cursor — not base64url, not <timestamp>|<uuid>, a timestamp or id that would not survive the SQL
// casts, or a cursor minted for ANOTHER tenant, campaign or route — is a 400 validation_failed with details
// [cursor:malformed], decided here, BEFORE any query runs: it can never reach the database and so can never
// be a 500, and nothing of the other scope's data is read. The caller cannot tell the two refusals apart
// (the internal reason, logged against the request id, can).
func parseReportCursor(r *http.Request, userID, campaignID string) (*reportCursor, *apiError) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return nil, nil
	}
	pos, err := pagecursor.Decode(raw, reportCursorScope(userID, campaignID))
	if err != nil {
		reason := "cursor_malformed"
		if errors.Is(err, pagecursor.ErrWrongScope) {
			reason = "cursor_wrong_scope"
		}
		return nil, newErr(codeValidationFailed, reason).
			withDetails(fieldDetail{Field: "cursor", Issue: issueMalformed})
	}
	return &reportCursor{CreatedAt: pos.CreatedAt, ID: pos.ID}, nil
}

func parseReportPageSize(r *http.Request) int {
	const defaultPageSize = 100
	const maxPageSize = 200
	q := r.URL.Query().Get("page_size")
	if q == "" {
		return defaultPageSize
	}
	n, err := strconv.Atoi(q)
	if err != nil || n <= 0 {
		return defaultPageSize
	}
	if n > maxPageSize {
		return maxPageSize
	}
	return n
}

func encodeReportCursor(userID, campaignID, createdAt, id string) string {
	return pagecursor.Encode(reportCursorScope(userID, campaignID), createdAt, id)
}
