package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
)

type accountCreateReq struct {
	PoolID       string  `json:"pool_id"`
	SMTPHost     string  `json:"smtp_host"`
	SMTPPort     *int    `json:"smtp_port"`
	SMTPUsername string  `json:"smtp_username"`
	SMTPSecret   string  `json:"smtp_secret"`
	FromAddress  string  `json:"from_address"`
	FromName     *string `json:"from_name"`
	DailyLimit   *int    `json:"daily_limit"`
}

// warning is a non-blocking advisory returned alongside a 2xx (§3.2 pool_disabled).
type warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// handleAccountCreate: POST /v1/admin/accounts (§2.2). Validate → resolve daily_limit default →
// derive user_id from pool → app-side UUID → encrypt secret (AAD = id ‖ key_id) → single INSERT
// + one account_events 'created' row in one tx → zero the plaintext. The secret is write-only;
// the 201 body carries has_credential:true and NO credential material.
func (s *Server) handleAccountCreate(w http.ResponseWriter, r *http.Request) {
	var req accountCreateReq
	present, derr := decodeStrict(r, &req)
	if derr != nil {
		s.fail(w, r, derr)
		return
	}

	// --- required fields ---
	var details []fieldDetail
	if req.PoolID == "" {
		details = append(details, fieldDetail{Field: "pool_id", Issue: issueRequired})
	} else if !isUUID(req.PoolID) {
		details = append(details, fieldDetail{Field: "pool_id", Issue: issueMalformed})
	}
	if !hasField(present, "smtp_secret") || req.SMTPSecret == "" {
		details = append(details, fieldDetail{Field: "smtp_secret", Issue: issueMalformed})
	}
	port := 587
	if req.SMTPPort != nil {
		port = *req.SMTPPort
	}
	details = append(details, collect(
		validateSMTPHost(req.SMTPHost),
		validateSMTPPort(port),
		validateSMTPUsername(req.SMTPUsername),
		validateFromAddress(req.FromAddress),
		validateFromName(req.FromName),
	)...)
	if req.DailyLimit != nil {
		details = append(details, collect(validateDailyLimit(*req.DailyLimit))...)
	}
	if len(details) > 0 {
		s.fail(w, r, newErr(codeValidationFailed, "account_create_validation").withDetails(details...))
		return
	}

	ctx, cancel := dbCtx(r.Context())
	defer cancel()

	// --- resolve daily_limit: request value, else config daily_limit_default ---
	dailyLimit, lerr := s.resolveDailyLimit(ctx, req.DailyLimit)
	if lerr != nil {
		s.fail(w, r, lerr)
		return
	}

	// --- derive user_id from pool tenancy (§2.2.1) ---
	userID, shared, err := deriveUserID(ctx, s.db, req.PoolID)
	if err == errPoolNotFound {
		s.fail(w, r, newErr(codeNotFound, "pool_not_found:"+req.PoolID))
		return
	}
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "derive_user_id_error: "+err.Error()))
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

	// --- per-pool uniqueness pre-check (§2.7): (pool_id, host, username) unique within pool ---
	dup, derr2 := s.duplicateInPool(ctx, req.PoolID, req.SMTPHost, req.SMTPUsername, "")
	if derr2 != nil {
		s.fail(w, r, derr2)
		return
	}
	if dup {
		s.fail(w, r, newErr(codeConflict, "duplicate_account_in_pool").
			withDetails(fieldDetail{Field: "smtp_username", Issue: issueConflict}))
		return
	}

	// --- app-side UUID (OD-4) so account.id exists before encryption ---
	accIDStr, accIDBytes, err := newUUIDv4()
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "uuid_generate_error: "+err.Error()))
		return
	}

	// --- encrypt the secret (AAD = account.id ‖ credential_key_id) then zero the plaintext ---
	secret := []byte(req.SMTPSecret)
	ciphertext, nonce, err := s.enc.Encrypt(accIDBytes, secret)
	zero(secret) // plaintext discarded immediately after encryption (§2.6 step 6)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "encrypt_error: "+err.Error()))
		return
	}
	keyID := s.enc.KeyID()

	// --- single INSERT + one 'created' audit row, one transaction ---
	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "tx_begin_error: "+err.Error()))
		return
	}
	defer tx.Rollback(context.Background())

	_, err = tx.Exec(ctx,
		`INSERT INTO send_managed.accounts
		   (id, user_id, pool_id, smtp_host, smtp_port, smtp_username,
		    credential_ciphertext, credential_key_id, credential_nonce,
		    from_address, from_name, daily_limit)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		accIDStr, userID, req.PoolID, req.SMTPHost, port, req.SMTPUsername,
		ciphertext, keyID, nonce, req.FromAddress, req.FromName, dailyLimit)
	if err != nil {
		s.fail(w, r, s.translateAccountWrite(err))
		return
	}
	if err := writeAuditEvent(ctx, tx, accIDStr, userID, "created", nil, ptrSnapshot(false, nil, false, false), nil, requestIDFrom(r.Context())); err != nil {
		s.fail(w, r, newErr(codeInternalError, "audit_write_error: "+err.Error()))
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.fail(w, r, s.translateAccountWrite(err))
		return
	}

	acc, err := getAccount(ctx, s.db, accIDStr)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "account_reread_error: "+err.Error()))
		return
	}

	// Non-blocking warning if the target pool is disabled (§3.2 allow + warn).
	resp := accountCreateResp{accountResource: acc}
	if warn := s.poolDisabledWarning(ctx, req.PoolID); warn != nil {
		resp.Warnings = []warning{*warn}
	}
	s.logOK(requestIDFrom(r.Context()), r, "account_created id="+accIDStr)
	writeJSON(w, http.StatusCreated, resp)
}

// accountCreateResp embeds the account object and adds an optional warnings array (§3.2).
type accountCreateResp struct {
	*accountResource
	Warnings []warning `json:"warnings,omitempty"`
}

// MarshalJSON flattens the embedded accountResource with the optional warnings array.
func (c accountCreateResp) MarshalJSON() ([]byte, error) {
	base, err := json.Marshal(c.accountResource)
	if err != nil {
		return nil, err
	}
	if len(c.Warnings) == 0 {
		return base, nil
	}
	// Splice "warnings" into the object.
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}
	wj, err := json.Marshal(c.Warnings)
	if err != nil {
		return nil, err
	}
	m["warnings"] = wj
	return json.Marshal(m)
}

// resolveDailyLimit returns the request value if present, else the config daily_limit_default.
func (s *Server) resolveDailyLimit(ctx context.Context, reqVal *int) (int, *apiError) {
	if reqVal != nil {
		return *reqVal, nil
	}
	row, err := getConfig(ctx, s.db, "daily_limit_default")
	if err != nil {
		return 0, newErr(codeInternalError, "daily_limit_default_lookup_error")
	}
	n, ok := asInt(row.Value)
	if !ok || n <= 0 {
		return 0, newErr(codeInternalError, "daily_limit_default_not_positive_int")
	}
	return int(n), nil
}

// duplicateInPool reports whether (pool_id, host, username) already exists, optionally excluding
// a given account id (for the PATCH move case). App-level pre-check for the clean 409 (§2.7).
func (s *Server) duplicateInPool(ctx context.Context, poolID, host, username, excludeID string) (bool, *apiError) {
	var exists bool
	var err error
	if excludeID == "" {
		err = s.db.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM send_managed.accounts WHERE pool_id=$1 AND smtp_host=$2 AND smtp_username=$3)",
			poolID, host, username).Scan(&exists)
	} else {
		err = s.db.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM send_managed.accounts WHERE pool_id=$1 AND smtp_host=$2 AND smtp_username=$3 AND id<>$4)",
			poolID, host, username, excludeID).Scan(&exists)
	}
	if err != nil {
		return false, newErr(codeInternalError, "duplicate_check_error: "+err.Error())
	}
	return exists, nil
}

// poolDisabledWarning returns a pool_disabled warning if the pool is disabled, else nil.
func (s *Server) poolDisabledWarning(ctx context.Context, poolID string) *warning {
	var enabled bool
	if err := s.db.QueryRow(ctx, "SELECT enabled FROM send_managed.pools WHERE id=$1", poolID).Scan(&enabled); err != nil {
		return nil
	}
	if !enabled {
		return &warning{Code: "pool_disabled", Message: "Assigned to a disabled pool; it will not route until enabled."}
	}
	return nil
}

// translateAccountWrite maps a raw account INSERT/UPDATE DB error to a clean envelope — the
// per-pool UNIQUE (23505) → 409 duplicate_account_in_pool; the daily_sent CHECK (23514) → 409
// (§3.1); FK → validation. A raw SQLSTATE never reaches the client.
func (s *Server) translateAccountWrite(err error) *apiError {
	switch pgErrCode(err) {
	case sqlstateUniqueViolation:
		if pgConstraint(err) == "accounts_pool_host_user_unique" {
			return newErr(codeConflict, "duplicate_account_in_pool").
				withDetails(fieldDetail{Field: "smtp_username", Issue: issueConflict})
		}
		return newErr(codeConflict, "unique_violation:"+pgConstraint(err))
	case sqlstateCheckViolation:
		if pgConstraint(err) == "accounts_daily_sent_check" {
			return newErr(codeConflict, "daily_limit_below_sent_count").
				withDetails(fieldDetail{Field: "daily_limit", Issue: issueConflict})
		}
		return newErr(codeUnprocessable, "check_violation:"+pgConstraint(err))
	case sqlstateForeignKeyViolation:
		return newErr(codeValidationFailed, "fk_violation:"+pgConstraint(err)).
			withDetails(fieldDetail{Field: "pool_id", Issue: issueNotFound})
	default:
		return newErr(codeInternalError, "account_write_error: "+err.Error())
	}
}

// ptrSnapshot builds a *flagsSnapshot for the audit to/from columns.
func ptrSnapshot(paused bool, reason *string, quarantined, disabled bool) *flagsSnapshot {
	return &flagsSnapshot{Paused: paused, PauseReason: reason, Quarantined: quarantined, Disabled: disabled}
}

// zero overwrites a byte slice (best-effort secret hygiene: the plaintext lives in process
// memory only and is cleared right after encryption, §2.6 step 6).
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
