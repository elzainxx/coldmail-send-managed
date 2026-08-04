package adminapi

import (
	"context"
	"net/http"
)

// Pools CRUD + lifecycle (§5). user_id is DERIVED from kind (shared → SystemUserID; dedicated →
// owner_user_id), never a request field. Owner rules, fallback-cycle prevention, and the
// ON DELETE RESTRICT contract are enforced with clean envelopes — a raw 23503/23514 never
// reaches the client. Pool admin actions are logged (structured app log); the account_events
// table is FK'd to accounts and does not audit pools (a pool-audit table is a future item, §10).

type poolCreateReq struct {
	Kind            string  `json:"kind"`
	OwnerUserID     *string `json:"owner_user_id"`
	Name            string  `json:"name"`
	RoutingStrategy *string `json:"routing_strategy"`
	FallbackPoolID  *string `json:"fallback_pool_id"`
	Enabled         *bool   `json:"enabled"`
}

func validRoutingStrategy(s string) bool {
	return s == "random" || s == "round_robin" || s == "one_by_one"
}

// handlePoolCreate: POST /v1/admin/pools (§5.2). Owner rules enforced before the DB CHECK.
func (s *Server) handlePoolCreate(w http.ResponseWriter, r *http.Request) {
	var req poolCreateReq
	if derr := s.decodeInto(r, &req); derr != nil {
		s.fail(w, r, derr)
		return
	}

	// name required.
	if req.Name == "" {
		s.fail(w, r, newErr(codeValidationFailed, "pool_name_required").
			withDetails(fieldDetail{Field: "name", Issue: issueRequired}))
		return
	}
	// routing_strategy default round_robin; validate if present.
	routing := "round_robin"
	if req.RoutingStrategy != nil {
		if !validRoutingStrategy(*req.RoutingStrategy) {
			s.fail(w, r, newErr(codeValidationFailed, "pool_routing_strategy_invalid").
				withDetails(fieldDetail{Field: "routing_strategy", Issue: issueForbiddenValue}))
			return
		}
		routing = *req.RoutingStrategy
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	// Owner rules + derive user_id (§5.2 matrix).
	var userID string
	var ownerUserID *string
	ctx, cancel := dbCtx(r.Context())
	defer cancel()

	switch req.Kind {
	case "shared":
		if req.OwnerUserID != nil {
			s.fail(w, r, newErr(codeUnprocessable, "shared_pool_forbids_owner").
				withDetails(fieldDetail{Field: "owner_user_id", Issue: issueForbiddenValue}))
			return
		}
		// §0.1 pre-flight: the reserved system user must be provisioned in public.users.
		ok, err := systemUserProvisioned(ctx, s.db)
		if err != nil {
			s.fail(w, r, newErr(codeInternalError, "system_user_preflight_error: "+err.Error()))
			return
		}
		if !ok {
			s.fail(w, r, newErr(codeConflict, "system_user_not_provisioned"))
			return
		}
		userID = SystemUserID
		ownerUserID = nil
	case "dedicated":
		if req.OwnerUserID == nil {
			s.fail(w, r, newErr(codeUnprocessable, "dedicated_pool_requires_owner").
				withDetails(fieldDetail{Field: "owner_user_id", Issue: issueRequired}))
			return
		}
		if !isUUID(*req.OwnerUserID) {
			s.fail(w, r, newErr(codeValidationFailed, "owner_user_id_malformed").
				withDetails(fieldDetail{Field: "owner_user_id", Issue: issueMalformed}))
			return
		}
		exists, err := userExists(ctx, s.db, *req.OwnerUserID)
		if err != nil {
			s.fail(w, r, newErr(codeInternalError, "owner_lookup_error: "+err.Error()))
			return
		}
		if !exists {
			s.fail(w, r, newErr(codeValidationFailed, "owner_user_not_found").
				withDetails(fieldDetail{Field: "owner_user_id", Issue: issueNotFound}))
			return
		}
		userID = *req.OwnerUserID
		ownerUserID = req.OwnerUserID
	case "":
		// OMITTED is a different operator mistake from a BAD VALUE, and the two want different
		// fixes ("add the field" vs "pick a legal value"). This handler already draws that line for
		// owner_user_id (issueRequired vs issueMalformed above), and platformapi draws it for
		// job_item_id/user_id; `kind` collapsed both into forbidden_value, which reads as "the value
		// you sent is not allowed" when nothing was sent at all. Found walking the Stage 4 admin
		// surface in Docs/coldmail-send-managed-local-test-guide.md.
		s.fail(w, r, newErr(codeValidationFailed, "pool_kind_required").
			withDetails(fieldDetail{Field: "kind", Issue: issueRequired}))
		return
	default:
		s.fail(w, r, newErr(codeValidationFailed, "pool_kind_invalid").
			withDetails(fieldDetail{Field: "kind", Issue: issueForbiddenValue}))
		return
	}

	// fallback_pool_id: must exist + pass the cycle check (§5.5). On create, editing="" (a
	// brand-new pool cannot be pointed at yet), so the walk only validates existence + a
	// pre-existing loop among the targets.
	if req.FallbackPoolID != nil {
		if !isUUID(*req.FallbackPoolID) {
			s.fail(w, r, newErr(codeValidationFailed, "fallback_pool_id_malformed").
				withDetails(fieldDetail{Field: "fallback_pool_id", Issue: issueMalformed}))
			return
		}
		if verr := s.checkFallback(ctx, "", *req.FallbackPoolID); verr != nil {
			s.fail(w, r, verr)
			return
		}
	}

	p, err := insertPool(ctx, s.db, userID, ownerUserID, req.Kind, req.Name, routing, req.FallbackPoolID, enabled)
	if err != nil {
		s.fail(w, r, s.translatePoolWrite(err))
		return
	}
	s.logOK(requestIDFrom(r.Context()), r, "pool_created id="+p.ID+" kind="+p.Kind)
	writeJSON(w, http.StatusCreated, p)
}

// handlePoolGet: GET /v1/admin/pools/{id}.
func (s *Server) handlePoolGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isUUID(id) {
		s.fail(w, r, newErr(codeNotFound, "pool_id_malformed"))
		return
	}
	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	p, err := getPool(ctx, s.db, id)
	if err == errPoolNotFound {
		s.fail(w, r, newErr(codeNotFound, "pool_not_found:"+id))
		return
	}
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "pool_get_error: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handlePoolDelete: DELETE /v1/admin/pools/{id} (§5.5). ON DELETE RESTRICT via accounts_pool_fk
// blocks a pool that still has accounts → clean 409 (a pre-check produces it before the DB; the
// RESTRICT is the race backstop, translated never surfaced raw).
func (s *Server) handlePoolDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isUUID(id) {
		s.fail(w, r, newErr(codeNotFound, "pool_id_malformed"))
		return
	}
	ctx, cancel := dbCtx(r.Context())
	defer cancel()

	p, err := getPool(ctx, s.db, id)
	if err == errPoolNotFound {
		s.fail(w, r, newErr(codeNotFound, "pool_not_found:"+id))
		return
	}
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "pool_get_error: "+err.Error()))
		return
	}
	// Pre-check: refuse if it still has accounts, with a clean 409 (the exact §5.5 message).
	if p.AccountCount > 0 {
		s.fail(w, r, newErr(codeConflict, "pool_has_accounts").
			withMessage("Pool still has accounts; reassign or remove them first."))
		return
	}
	if _, err := deletePool(ctx, s.db, id); err != nil {
		// Race backstop: a concurrently-inserted account triggers the FK RESTRICT (23503).
		if pgErrCode(err) == sqlstateForeignKeyViolation {
			s.fail(w, r, newErr(codeConflict, "pool_has_accounts_race").
				withMessage("Pool still has accounts; reassign or remove them first."))
			return
		}
		s.fail(w, r, newErr(codeInternalError, "pool_delete_error: "+err.Error()))
		return
	}
	s.logOK(requestIDFrom(r.Context()), r, "pool_deleted id="+id)
	w.WriteHeader(http.StatusNoContent)
}

// checkFallback runs the app-layer cycle check and translates the result. editing is "" on
// create, or the pool id on update.
func (s *Server) checkFallback(ctx context.Context, editing, target string) *apiError {
	cyc, err := fallbackCycleExists(ctx, s.db, editing, target)
	if err == errPoolNotFound {
		return newErr(codeValidationFailed, "fallback_pool_not_found").
			withDetails(fieldDetail{Field: "fallback_pool_id", Issue: issueNotFound})
	}
	if err != nil {
		return newErr(codeInternalError, "fallback_cycle_check_error: "+err.Error())
	}
	if cyc {
		return newErr(codeUnprocessable, "fallback_cycle").
			withDetails(fieldDetail{Field: "fallback_pool_id", Issue: issueForbiddenValue})
	}
	return nil
}

// translatePoolWrite maps a raw pool INSERT/UPDATE DB error to a clean envelope. The Phase 1
// CHECKs (pools_kind_ownership_check, pools_fallback_not_self_check) are backstops — the API
// rejects first, but a 23514 that slips through is translated to 422, never surfaced raw.
func (s *Server) translatePoolWrite(err error) *apiError {
	switch pgErrCode(err) {
	case sqlstateCheckViolation:
		return newErr(codeUnprocessable, "pool_check_violation:"+pgConstraint(err)).
			withDetails(fieldDetail{Field: "fallback_pool_id", Issue: issueForbiddenValue})
	case sqlstateForeignKeyViolation:
		return newErr(codeValidationFailed, "pool_fk_violation:"+pgConstraint(err)).
			withDetails(fieldDetail{Field: "fallback_pool_id", Issue: issueNotFound})
	default:
		return newErr(codeInternalError, "pool_write_error: "+err.Error())
	}
}

// decodeInto is a thin wrapper for handlers that don't need the field-presence map.
func (s *Server) decodeInto(r *http.Request, dst any) *apiError {
	_, derr := decodeStrict(r, dst)
	return derr
}
