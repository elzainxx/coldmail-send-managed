package adminapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

type accountUpdateReq struct {
	PoolID       *string `json:"pool_id"`
	SMTPHost     *string `json:"smtp_host"`
	SMTPPort     *int    `json:"smtp_port"`
	SMTPUsername *string `json:"smtp_username"`
	SMTPSecret   *string `json:"smtp_secret"`
	FromAddress  *string `json:"from_address"`
	FromName     *string `json:"from_name"`
	DailyLimit   *int    `json:"daily_limit"`
}

// handleAccountUpdate: PATCH /v1/admin/accounts/{id} (§2.4). omit = unchanged, enforced
// STRUCTURALLY for the secret: when smtp_secret is ABSENT, the three credential columns are NOT
// in the SET list at all (byte-identical, not re-encrypted to the same value). Not patchable
// here: paused/quarantined/disabled/pause_reason (§7 only), account_status_score (Phase 5),
// user_id (derived from pool). A successful PATCH writes one 'updated' audit row.
func (s *Server) handleAccountUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isUUID(id) {
		s.fail(w, r, newErr(codeNotFound, "account_id_malformed"))
		return
	}
	var req accountUpdateReq
	present, derr := decodeStrict(r, &req)
	if derr != nil {
		s.fail(w, r, derr)
		return
	}
	// Fields that are NOT patchable through this endpoint — paused / quarantined / disabled /
	// pause_reason (move only through the audited §7 lifecycle), account_status_score (Phase 5),
	// user_id / id (tenancy derived from the pool, not directly settable) — are deliberately
	// ABSENT from accountUpdateReq. decodeStrict's DisallowUnknownFields therefore rejects any of
	// them with a clean 400 validation_failed BEFORE this point (no silent ignore, no partial
	// write). That is the structural guard; there is intentionally no per-field 422 branch here,
	// since the field can never decode into the request struct in the first place.

	ctx, cancel := dbCtx(r.Context())
	defer cancel()

	// Load current row (404 before any write) + its user_id and daily_sent_count for checks.
	cur, err := getAccount(ctx, s.db, id)
	if err == errAccountNotFound {
		s.fail(w, r, newErr(codeNotFound, "account_not_found:"+id))
		return
	}
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "account_get_error: "+err.Error()))
		return
	}

	// Validate present scalar fields.
	var details []fieldDetail
	if hasField(present, "smtp_host") {
		if req.SMTPHost == nil {
			details = append(details, fieldDetail{Field: "smtp_host", Issue: issueMalformed})
		} else {
			details = append(details, collect(validateSMTPHost(*req.SMTPHost))...)
		}
	}
	if hasField(present, "smtp_port") {
		if req.SMTPPort == nil {
			// present-but-null (or non-integer) — reject like every sibling field, never skip
			// (a skip would let the SET-list builder below dereference a nil pointer).
			details = append(details, fieldDetail{Field: "smtp_port", Issue: issueMalformed})
		} else {
			details = append(details, collect(validateSMTPPort(*req.SMTPPort))...)
		}
	}
	if hasField(present, "smtp_username") {
		if req.SMTPUsername == nil {
			details = append(details, fieldDetail{Field: "smtp_username", Issue: issueMalformed})
		} else {
			details = append(details, collect(validateSMTPUsername(*req.SMTPUsername))...)
		}
	}
	if hasField(present, "from_address") {
		if req.FromAddress == nil {
			details = append(details, fieldDetail{Field: "from_address", Issue: issueMalformed})
		} else {
			details = append(details, collect(validateFromAddress(*req.FromAddress))...)
		}
	}
	if hasField(present, "from_name") {
		details = append(details, collect(validateFromName(req.FromName))...)
	}
	if hasField(present, "daily_limit") {
		if req.DailyLimit == nil {
			details = append(details, fieldDetail{Field: "daily_limit", Issue: issueMalformed})
		} else {
			details = append(details, collect(validateDailyLimit(*req.DailyLimit))...)
		}
	}
	// smtp_secret present-but-null-or-empty → malformed (§2.4). Absent = unchanged (handled below).
	if hasField(present, "smtp_secret") {
		if isJSONNull(present, "smtp_secret") || req.SMTPSecret == nil || *req.SMTPSecret == "" {
			details = append(details, fieldDetail{Field: "smtp_secret", Issue: issueMalformed})
		}
	}
	if len(details) > 0 {
		s.fail(w, r, newErr(codeValidationFailed, "account_update_validation").withDetails(details...))
		return
	}

	// daily_limit below current counter → clean 409 pre-check (§3.1).
	if hasField(present, "daily_limit") && req.DailyLimit != nil && *req.DailyLimit < cur.DailySentCount {
		s.fail(w, r, newErr(codeConflict, "daily_limit_below_sent_count").
			withDetails(fieldDetail{Field: "daily_limit", Issue: issueConflict}))
		return
	}

	// Determine effective pool + re-derive user_id if pool_id changes (§2.4/§2.2.1).
	newUserID := cur.UserID
	effectivePool := cur.PoolID
	if hasField(present, "pool_id") {
		if req.PoolID == nil || !isUUID(*req.PoolID) {
			s.fail(w, r, newErr(codeValidationFailed, "pool_id_malformed").
				withDetails(fieldDetail{Field: "pool_id", Issue: issueMalformed}))
			return
		}
		uid, shared, derr := deriveUserID(ctx, s.db, *req.PoolID)
		if derr == errPoolNotFound {
			s.fail(w, r, newErr(codeNotFound, "pool_not_found:"+*req.PoolID))
			return
		}
		if derr != nil {
			s.fail(w, r, newErr(codeInternalError, "derive_user_id_error: "+derr.Error()))
			return
		}
		if shared {
			ok, perr := systemUserProvisioned(ctx, s.db)
			if perr != nil {
				s.fail(w, r, newErr(codeInternalError, "system_user_preflight_error: "+perr.Error()))
				return
			}
			if !ok {
				s.fail(w, r, newErr(codeConflict, "system_user_not_provisioned"))
				return
			}
		}
		newUserID = uid
		effectivePool = *req.PoolID
	}

	// Per-pool uniqueness pre-check on the RESULTING (pool, host, username) (§2.7 move case).
	effHost := cur.SMTPHost
	if hasField(present, "smtp_host") && req.SMTPHost != nil {
		effHost = *req.SMTPHost
	}
	effUser := cur.SMTPUsername
	if hasField(present, "smtp_username") && req.SMTPUsername != nil {
		effUser = *req.SMTPUsername
	}
	if hasField(present, "pool_id") || hasField(present, "smtp_host") || hasField(present, "smtp_username") {
		dup, uerr := s.duplicateInPool(ctx, effectivePool, effHost, effUser, id)
		if uerr != nil {
			s.fail(w, r, uerr)
			return
		}
		if dup {
			s.fail(w, r, newErr(codeConflict, "duplicate_account_in_pool").
				withDetails(fieldDetail{Field: "smtp_username", Issue: issueConflict}))
			return
		}
	}

	// Build the SET list. The credential columns are added ONLY when smtp_secret is present —
	// this is the structural omit=unchanged guarantee (§2.4).
	var sets []string
	var args []any
	set := func(expr string, val any) {
		args = append(args, val)
		sets = append(sets, strings.Replace(expr, "?", "$"+strconv.Itoa(len(args)), 1))
	}
	if hasField(present, "pool_id") {
		set("pool_id = ?", effectivePool)
		set("user_id = ?", newUserID) // re-derived tenancy
	}
	if hasField(present, "smtp_host") {
		set("smtp_host = ?", *req.SMTPHost)
	}
	if hasField(present, "smtp_port") && req.SMTPPort != nil {
		set("smtp_port = ?", *req.SMTPPort)
	}
	if hasField(present, "smtp_username") {
		set("smtp_username = ?", *req.SMTPUsername)
	}
	if hasField(present, "from_address") {
		set("from_address = ?", *req.FromAddress)
	}
	if hasField(present, "from_name") {
		set("from_name = ?", req.FromName) // may be null (clears display name)
	}
	if hasField(present, "daily_limit") {
		set("daily_limit = ?", *req.DailyLimit)
	}

	// Secret re-encryption ONLY if present (fresh nonce, AAD = this account's id ‖ current key).
	if hasField(present, "smtp_secret") {
		accBytes, berr := uuidToBytes(id)
		if berr != nil {
			s.fail(w, r, newErr(codeInternalError, "account_id_parse_error"))
			return
		}
		secret := []byte(*req.SMTPSecret)
		ct, nonce, eerr := s.enc.Encrypt(accBytes, secret)
		zero(secret)
		if eerr != nil {
			s.fail(w, r, newErr(codeInternalError, "encrypt_error: "+eerr.Error()))
			return
		}
		set("credential_ciphertext = ?", ct)
		set("credential_nonce = ?", nonce)
		set("credential_key_id = ?", s.enc.KeyID())
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "tx_begin_error: "+err.Error()))
		return
	}
	defer tx.Rollback(context.Background())

	if len(sets) > 0 {
		sets = append(sets, "updated_at = NOW()")
		args = append(args, id)
		idParam := "$" + strconv.Itoa(len(args))
		if _, err := tx.Exec(ctx, "UPDATE send_managed.accounts SET "+strings.Join(sets, ", ")+" WHERE id="+idParam, args...); err != nil {
			s.fail(w, r, s.translateAccountWrite(err))
			return
		}
	}
	// One 'updated' audit row (flags unchanged by a generic PATCH — snapshot before == after).
	fromSnap := ptrSnapshot(cur.Status.Paused, cur.Status.PauseReason, cur.Status.Quarantined, cur.Status.Disabled)
	if err := writeAuditEvent(ctx, tx, id, newUserID, "updated", fromSnap, fromSnap, nil, requestIDFrom(r.Context())); err != nil {
		s.fail(w, r, newErr(codeInternalError, "audit_write_error: "+err.Error()))
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.fail(w, r, s.translateAccountWrite(err))
		return
	}

	acc, err := getAccount(ctx, s.db, id)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "account_reread_error: "+err.Error()))
		return
	}
	s.logOK(requestIDFrom(r.Context()), r, "account_updated id="+id)
	writeJSON(w, http.StatusOK, acc)
}
