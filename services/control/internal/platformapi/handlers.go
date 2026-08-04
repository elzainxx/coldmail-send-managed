package platformapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// defaultQuoteUnitCredits is the fallback used ONLY if unit_price_credits is unreadable (should
// never happen post-send_managed_0021 — the key is seeded with this exact value). Not itself a tuning
// dial: the real, live-retunable value is read fresh from send_managed.config on every request via
// readUnitPriceCredits (Phase 9.5 S10, §2.4) — config-not-code, no redeploy needed for a price
// change to take effect.
const defaultQuoteUnitCredits = 1

// defaultQuoteTier is the tier `quote` reports (Phase 9 §1.1's additive `tier` field). `quote`'s
// request carries only recipient_count — no job_item_id/user_id/pool context — so it cannot
// resolve which of the two V1 pool-kind tiers (§2.1: shared/dedicated) a campaign will actually
// route to. This is honest rather than a gap: both tiers charge the IDENTICAL unit_price_credits
// in V1 (dedicated is isolation, not a price differentiator), so the reported tier never affects
// the quoted cost either way. "shared" is reported because it is the base-campaign default
// (Phase 4's default_shared_pool_id routing) that every campaign is eligible for regardless of
// whether it also has a dedicated pool.
const defaultQuoteTier = "shared"

// describeTiers is the SAME per-tier pricing shape quote's defaultQuoteTier draws from (Phase 9
// §2.1/§2.2) — the single source of truth describe and quote share so they can never diverge.
// unit is populated live per-request from readUnitPriceCredits; both tiers price IDENTICALLY in
// V1 (dedicated is isolation, not a premium — stated honestly rather than inventing one).
func describeTiers(unit int) []map[string]any {
	return []map[string]any{
		{"id": "shared", "unit_price_credits": unit, "description": "Global shared SMTP pool."},
		{"id": "dedicated", "unit_price_credits": unit, "description": "User-dedicated pool with shared fallback."},
	}
}

// uuidRe validates a canonical hyphenated UUID (job_item_id / job_id inputs).
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// --- POST /v1/quote — recipient count → cost (trivial pass-through, §6.1) ---

type quoteReq struct {
	RecipientCount *int `json:"recipient_count"`
}

func (s *Server) handleQuote(w http.ResponseWriter, r *http.Request) {
	var req quoteReq
	if e := decodeStrict(r.Body, &req); e != nil {
		s.fail(w, r, e)
		return
	}
	// ABSENT and PRESENT-BUT-NEGATIVE are reported DISTINCTLY (issueRequired vs issueOutOfRange),
	// matching the run/report user_id checks below and the whole details[] vocabulary contract: a UI
	// building generic per-field messaging off details[].issue must be able to tell "you forgot this"
	// from "the number you sent is not allowed". These collapsed into one issueRequired until
	// 2026-07-28, so `{"recipient_count": -5}` told the caller a field it had plainly supplied was
	// missing (found walking Stage 7 of the local test guide). out_of_range, not malformed: a negative
	// integer decodes fine and is the right TYPE — it is the VALUE that is outside the allowed domain
	// (recipient_count >= 0), which is exactly adminapi's daily_limit/smtp_port precedent.
	if req.RecipientCount == nil {
		s.fail(w, r, newErr(codeValidationFailed, "recipient_count_required").
			withDetails(fieldDetail{Field: "recipient_count", Issue: issueRequired}))
		return
	}
	if *req.RecipientCount < 0 {
		s.fail(w, r, newErr(codeValidationFailed, "recipient_count_negative").
			withDetails(fieldDetail{Field: "recipient_count", Issue: issueOutOfRange}))
		return
	}
	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	unit := s.readUnitPriceCredits(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"recipient_count": *req.RecipientCount,
		"cost":            *req.RecipientCount * unit,
		"currency":        "credits",
		// Additive (Phase 9 §1.1): unit_price_credits/tier let the platform's preview/confirm-cost
		// step show the caller WHY the number is what it is. quote has no tenant/pool context in
		// its request (recipient_count only), and both V1 tiers price identically (§2.1) — tier is
		// therefore reported as the default base-campaign tier ("shared", Phase 4's
		// default_shared_pool_id routing), which never affects cost since the two tiers charge the
		// same unit_price_credits. A dedicated-pool campaign is quoted identically either way.
		"unit_price_credits": unit,
		"tier":               defaultQuoteTier,
	})
}

// --- POST /v1/run — accept a job, return immediately (async kickoff, §6.2) ---

type runReq struct {
	JobItemID string `json:"job_item_id"`
	UserID    string `json:"user_id"` // tenant selector (OD-25 option b) — the owning tenant
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req runReq
	if e := decodeStrict(r.Body, &req); e != nil {
		s.fail(w, r, e)
		return
	}
	if !uuidRe.MatchString(req.JobItemID) {
		s.fail(w, r, newErr(codeValidationFailed, "job_item_id_required").
			withDetails(fieldDetail{Field: "job_item_id", Issue: issueRequired}))
		return
	}
	// Tenant selector is REQUIRED and must be a UUID (OD-25 option b — module-enforced tenancy).
	// Empty vs malformed are reported distinctly (issueRequired vs issueMalformed), matching the
	// job_item_id/job_id/campaign_id checks elsewhere in this file — a UI building generic
	// per-field messaging off details[].issue must see one consistent vocabulary across every
	// UUID field on this surface (R2 finding).
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

	// Create one send_managed.jobs row (state=queued, genesis event via the actor='system' GUC), reading
	// the frozen recipient/subject/body/reply_to from public.job_items (SELECT-only, Phase 1),
	// SCOPED to the calling tenant: the job_item must belong to req.UserID (else 404-style
	// not-found — no cross-tenant existence disclosure). The job_item_id UNIQUE constraint makes a
	// duplicate run idempotent (§6.2).
	jobID, existing, e := s.createJob(ctx, req.JobItemID, req.UserID)
	if e != nil {
		s.fail(w, r, e)
		return
	}
	if existing {
		// Duplicate run for the same job_item_id: return the existing job + its current state
		// (treated as "already accepted", never a raw 23505). 200 with the existing job.
		state, _ := s.jobStateByItem(ctx, req.JobItemID, req.UserID)
		writeJSON(w, http.StatusOK, map[string]any{"job_id": jobID, "state": state})
		return
	}

	// Async kickoff: nudge the dispatcher WITHOUT blocking on the send (§6.2). run only INSERTs.
	if s.kick != nil {
		s.kick()
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job_id": jobID, "state": "queued"})
}

// --- GET /v1/report — reflect the job's state (per-tenant, clean state only, §6.3) ---

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	jobID := r.URL.Query().Get("job_id")
	jobItemID := r.URL.Query().Get("job_item_id")
	campaignID := r.URL.Query().Get("campaign_id")
	userID := r.URL.Query().Get("user_id")
	if jobID == "" && jobItemID == "" && campaignID == "" {
		s.fail(w, r, newErr(codeValidationFailed, "job_id_or_job_item_id_or_campaign_id_required").
			withDetails(fieldDetail{Field: "job_id", Issue: issueRequired}))
		return
	}
	// Tenant selector is REQUIRED and must be a UUID (OD-25 option b — module-enforced tenancy).
	// Empty vs malformed reported distinctly, matching job_id/job_item_id below (R2 finding).
	if userID == "" {
		s.fail(w, r, newErr(codeValidationFailed, "user_id_required").
			withDetails(fieldDetail{Field: "user_id", Issue: issueRequired}))
		return
	}
	if !uuidRe.MatchString(userID) {
		s.fail(w, r, newErr(codeValidationFailed, "user_id_malformed").
			withDetails(fieldDetail{Field: "user_id", Issue: issueMalformed}))
		return
	}
	// campaign_id (B8, §2.1): the aggregate campaign-scoped response, a distinct mode from the
	// single-job lookup below. Mutually exclusive in practice (a caller polling a campaign has no
	// single job_id/job_item_id yet); if somehow both are present, campaign_id wins since it is the
	// more specific ask a UI makes when it already knows it wants the campaign aggregate.
	if campaignID != "" {
		s.handleCampaignReport(w, r, campaignID, userID)
		return
	}
	if jobID != "" && !uuidRe.MatchString(jobID) {
		s.fail(w, r, newErr(codeValidationFailed, "job_id_malformed").
			withDetails(fieldDetail{Field: "job_id", Issue: issueMalformed}))
		return
	}
	if jobItemID != "" && !uuidRe.MatchString(jobItemID) {
		s.fail(w, r, newErr(codeValidationFailed, "job_item_id_malformed").
			withDetails(fieldDetail{Field: "job_item_id", Issue: issueMalformed}))
		return
	}

	ctx, cancel := dbCtx(r.Context())
	defer cancel()

	// SCOPED to the calling tenant (userID). A job that exists but belongs to another tenant
	// matches 0 rows and returns the SAME not_found as a non-existent job — no existence
	// disclosure across tenants (404-style, never 403 — OD-25).
	rep, ok, err := s.readReport(ctx, jobID, jobItemID, userID)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "report_query_failed"))
		return
	}
	if !ok {
		// Name the selector the CALLER actually sent. This branch is reached by either selector, and
		// hardcoding "job_id" pointed a job_item_id lookup at a field the caller never supplied —
		// misleading on the one code path (a polling integration keyed by job_item_id) where the
		// operator has no job_id to go check. The validation branches above already draw this
		// distinction, and store.go's run/quote not-founds already name job_item_id.
		// NO EXISTENCE DISCLOSURE (OD-25): this echoes back the caller's OWN choice of selector, not
		// anything about the row. Both selectors still return the SAME code, message, and status as a
		// wholly nonexistent job, so a cross-tenant probe learns nothing.
		field := "job_id"
		if jobID == "" {
			field = "job_item_id"
		}
		s.fail(w, r, newErr(codeNotFound, "job_not_found").
			withDetails(fieldDetail{Field: field, Issue: issueNotFound}))
		return
	}
	// Clean external state ONLY (§6.3): job_id, state, provider_message_id (when sent),
	// completed_at, attempt_count. NEVER the SMTP text, error_reason, credential, or internal reason.
	out := map[string]any{
		"job_id":        rep.jobID,
		"state":         rep.state,
		"attempt_count": rep.attemptCount,
	}
	if rep.providerMessageID != nil {
		out["provider_message_id"] = *rep.providerMessageID
	}
	if rep.completedAt != nil {
		out["completed_at"] = rep.completedAt.UTC()
	}
	writeJSON(w, http.StatusOK, out)
}

// --- GET /v1/describe — module self-description: capabilities + tiers/pricing (Phase 9 §2, finalized) ---

// handleDescribe reports the module's honest self-description (Phase 9 §2.2): the ONE real V1
// product axis (pool_kind: shared vs dedicated, Phase 1), flat per-recipient pricing, and the
// capabilities the module actually implements — no sequences, no open/click tracking, content
// variation present as operator-tunable behavior (no per-toggle detail, Phase 8 §3). Reads the
// SAME live unit_price_credits quote (§1) multiplies by, via describeTiers — single source of
// truth, so describe and quote can never disagree (Phase 9 §2.2, E2).
func (s *Server) handleDescribe(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	unit := s.readUnitPriceCredits(ctx)
	writeJSON(w, http.StatusOK, map[string]any{
		"module":      "send_managed",
		"api_version": "v1",
		"capabilities": map[string]any{
			"single_send":           true,
			"sequences":             false,
			"reply_to_per_campaign": true,
			"open_tracking":         false,
			"click_tracking":        false,
			"content_variation":     true,
		},
		"pricing": map[string]any{
			"currency":           "credits",
			"model":              "flat_per_recipient",
			"unit_price_credits": unit,
			"tiers":              describeTiers(unit),
		},
	})
}

// readUnitPriceCredits reads unit_price_credits fresh from send_managed.config (Phase 9.5 S10, §2.4) —
// live-retunable, no redeploy. platformapi does NOT import internal/dispatch's LoadConfig
// (structural package isolation, OD-7's precedent — mirrors internal/adminapi's own local config
// readers), so this is a minimal, scoped-to-one-key local reader. Falls back to the seeded default
// on any read/parse failure rather than failing the request — quote/describe are read-mostly,
// low-stakes endpoints and a transient config-read blip must not 500 every quote in flight.
func (s *Server) readUnitPriceCredits(ctx context.Context) int {
	var raw []byte
	if err := s.db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key = 'unit_price_credits'`).Scan(&raw); err != nil {
		return defaultQuoteUnitCredits
	}
	var wrap struct {
		V *int `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil {
		return defaultQuoteUnitCredits
	}
	return *wrap.V
}

// --- helpers ---

// decodeStrict reads a JSON body with unknown fields rejected. An empty body is allowed for
// endpoints with all-optional fields (the caller validates required fields itself).
func decodeStrict(body io.Reader, dst any) *apiError {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty body; required-field checks run next
		}
		e := newErr(codeValidationFailed, "malformed_json: "+sanitizeDecodeErr(err))
		// NAME THE FIELD when the decoder knows it. A wrong-TYPE value (`{"recipient_count": "7"}`)
		// returned an EMPTY details[] until 2026-07-28, so a caller got validation_failed with nothing
		// to attach it to — the one class of rejection where the body IS well-formed JSON and exactly
		// one named field is at fault (found walking Stage 7 of the local test guide). details[] is
		// documented as optional/additive, so this is a diagnosability fix, not a contract change:
		// the code and status are unchanged.
		//
		// TWO-VOCABULARY SAFETY (Rule 5): only *json.UnmarshalTypeError is unwrapped, and only its
		// .Field is used. That name is the key that MATCHED one of OUR struct tags (encoding/json
		// matches case-insensitively), so it is bounded to this endpoint's own schema — never arbitrary
		// caller content, and never the offending VALUE. A DisallowUnknownFields rejection is
		// deliberately NOT given a detail: its field name is attacker-chosen text from the body.
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			e = e.withDetails(fieldDetail{Field: typeErr.Field, Issue: issueMalformed})
		}
		return e
	}
	return nil
}

// sanitizeDecodeErr keeps a short internal hint from a decode error WITHOUT echoing body content.
func sanitizeDecodeErr(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return msg
}

// pgCode extracts the SQLSTATE from a pg error (never surfaced to the client).
func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// isNoRows reports pgx.ErrNoRows.
func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }
