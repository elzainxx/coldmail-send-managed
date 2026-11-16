package adminapi

import (
	"net/http"
	"strconv"
	"strings"
)

// handlePoolList: GET /v1/admin/pools (§5.3). Operator sees ALL pools. Filters: ?kind=,
// ?owner_user_id=, ?enabled=, ?user_id=. Cursor pagination on (created_at, id).
func (s *Server) handlePoolList(w http.ResponseWriter, r *http.Request) {
	limit := parseLimit(r)
	cur, cerr := parseCursor(r)
	if cerr != nil {
		s.fail(w, r, cerr)
		return
	}

	// Build a filtered keyset query. All predicates are parameterized (never string-concatenated
	// values). The keyset predicate (created_at,id) < / > cursor gives stable pagination.
	var conds []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, strings.Replace(cond, "?", "$"+strconv.Itoa(len(args)), 1))
	}
	if v := r.URL.Query().Get("kind"); v != "" {
		add("p.kind = ?", v)
	}
	if v := r.URL.Query().Get("owner_user_id"); v != "" {
		if !isUUID(v) {
			s.fail(w, r, newErr(codeValidationFailed, "owner_user_id_filter_malformed").
				withDetails(fieldDetail{Field: "owner_user_id", Issue: issueMalformed}))
			return
		}
		add("p.owner_user_id = ?", v)
	}
	if v := r.URL.Query().Get("user_id"); v != "" {
		if !isUUID(v) {
			s.fail(w, r, newErr(codeValidationFailed, "user_id_filter_malformed").
				withDetails(fieldDetail{Field: "user_id", Issue: issueMalformed}))
			return
		}
		add("p.user_id = ?", v)
	}
	if v := r.URL.Query().Get("enabled"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			s.fail(w, r, newErr(codeValidationFailed, "enabled_filter_malformed").
				withDetails(fieldDetail{Field: "enabled", Issue: issueMalformed}))
			return
		}
		add("p.enabled = ?", b)
	}
	if cur != nil {
		// keyset: rows AFTER the cursor in ascending (created_at, id).
		args = append(args, cur.CreatedAt)
		p1 := "$" + strconv.Itoa(len(args))
		args = append(args, cur.ID)
		p2 := "$" + strconv.Itoa(len(args))
		conds = append(conds, "(p.created_at, p.id) > ("+p1+"::timestamptz, "+p2+"::uuid)")
	}

	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit+1) // fetch one extra to detect next page
	limitParam := "$" + strconv.Itoa(len(args))
	q := "SELECT " + poolSelectCols + " FROM send_managed.pools p" + where +
		" ORDER BY p.created_at ASC, p.id ASC LIMIT " + limitParam

	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "pool_list_query_error: "+err.Error()))
		return
	}
	defer rows.Close()

	items := []poolResource{}
	for rows.Next() {
		p, serr := scanPool(rows)
		if serr != nil {
			s.fail(w, r, newErr(codeInternalError, "pool_list_scan_error: "+serr.Error()))
			return
		}
		items = append(items, *p)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, newErr(codeInternalError, "pool_list_rows_error: "+err.Error()))
		return
	}

	// Cursor from the LAST RETURNED item at FULL sort-key precision — see handleAccountList.
	var nextCursor *string
	if len(items) > limit {
		last := items[limit-1]
		items = items[:limit]
		c := encodeCursor(last.cursorTS, last.ID)
		nextCursor = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nextCursor})
}

type poolUpdateReq struct {
	Name            *string `json:"name"`
	RoutingStrategy *string `json:"routing_strategy"`
	FallbackPoolID  *string `json:"fallback_pool_id"`
	Enabled         *bool   `json:"enabled"`
	// kind and owner_user_id are IMMUTABLE post-create (OD-12). They are DECLARED here (rather
	// than left as unknown fields) precisely so decodeStrict does NOT blanket-400 them: the
	// handler detects their presence and returns the specific 422 forbidden_value below, which
	// is the informative rejection the spec wants. Values are ignored (never written).
	Kind        *string `json:"kind"`
	OwnerUserID *string `json:"owner_user_id"`
}

// handlePoolUpdate: PATCH /v1/admin/pools/{id} (§5.4). Patchable: name, routing_strategy,
// fallback_pool_id, enabled (omit = unchanged). NOT patchable: kind, owner_user_id (OD-12
// immutable — rejected as forbidden if sent). Setting fallback_pool_id re-runs the cycle check.
func (s *Server) handlePoolUpdate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isUUID(id) {
		s.fail(w, r, newErr(codeNotFound, "pool_id_malformed"))
		return
	}
	var req poolUpdateReq
	present, derr := decodeStrict(r, &req)
	if derr != nil {
		s.fail(w, r, derr)
		return
	}
	// Reject immutable fields explicitly if the client sent them (clean 422, not silent ignore).
	if hasField(present, "kind") {
		s.fail(w, r, newErr(codeUnprocessable, "pool_kind_immutable").
			withDetails(fieldDetail{Field: "kind", Issue: issueForbiddenValue}))
		return
	}
	if hasField(present, "owner_user_id") {
		s.fail(w, r, newErr(codeUnprocessable, "pool_owner_immutable").
			withDetails(fieldDetail{Field: "owner_user_id", Issue: issueForbiddenValue}))
		return
	}

	ctx, cancel := dbCtx(r.Context())
	defer cancel()

	// Confirm existence first (404 before any write).
	if _, err := getPool(ctx, s.db, id); err == errPoolNotFound {
		s.fail(w, r, newErr(codeNotFound, "pool_not_found:"+id))
		return
	} else if err != nil {
		s.fail(w, r, newErr(codeInternalError, "pool_get_error: "+err.Error()))
		return
	}

	// Build the SET list from present, validated fields.
	var sets []string
	var args []any
	set := func(expr string, val any) {
		args = append(args, val)
		sets = append(sets, strings.Replace(expr, "?", "$"+strconv.Itoa(len(args)), 1))
	}
	if hasField(present, "name") {
		if req.Name == nil || *req.Name == "" {
			s.fail(w, r, newErr(codeValidationFailed, "pool_name_malformed").
				withDetails(fieldDetail{Field: "name", Issue: issueMalformed}))
			return
		}
		set("name = ?", *req.Name)
	}
	if hasField(present, "routing_strategy") {
		if req.RoutingStrategy == nil || !validRoutingStrategy(*req.RoutingStrategy) {
			s.fail(w, r, newErr(codeValidationFailed, "pool_routing_strategy_invalid").
				withDetails(fieldDetail{Field: "routing_strategy", Issue: issueForbiddenValue}))
			return
		}
		set("routing_strategy = ?", *req.RoutingStrategy)
	}
	if hasField(present, "enabled") {
		if req.Enabled == nil {
			s.fail(w, r, newErr(codeValidationFailed, "pool_enabled_malformed").
				withDetails(fieldDetail{Field: "enabled", Issue: issueMalformed}))
			return
		}
		set("enabled = ?", *req.Enabled)
	}
	if hasField(present, "fallback_pool_id") {
		if isJSONNull(present, "fallback_pool_id") || req.FallbackPoolID == nil {
			// Explicitly clearing the fallback is allowed.
			set("fallback_pool_id = ?", nil)
		} else {
			if !isUUID(*req.FallbackPoolID) {
				s.fail(w, r, newErr(codeValidationFailed, "fallback_pool_id_malformed").
					withDetails(fieldDetail{Field: "fallback_pool_id", Issue: issueMalformed}))
				return
			}
			// Self-loop is caught by the Phase 1 CHECK, but reject early with a clean 422.
			if *req.FallbackPoolID == id {
				s.fail(w, r, newErr(codeUnprocessable, "fallback_cycle_self").
					withDetails(fieldDetail{Field: "fallback_pool_id", Issue: issueForbiddenValue}))
				return
			}
			if verr := s.checkFallback(ctx, id, *req.FallbackPoolID); verr != nil {
				s.fail(w, r, verr)
				return
			}
			set("fallback_pool_id = ?", *req.FallbackPoolID)
		}
	}

	if len(sets) == 0 {
		// No-op PATCH: return the current resource unchanged (still 200).
		p, err := getPool(ctx, s.db, id)
		if err != nil {
			s.fail(w, r, newErr(codeInternalError, "pool_get_error: "+err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, p)
		return
	}

	sets = append(sets, "updated_at = NOW()")
	args = append(args, id)
	idParam := "$" + strconv.Itoa(len(args))
	if _, err := s.db.Exec(ctx, "UPDATE send_managed.pools SET "+strings.Join(sets, ", ")+" WHERE id="+idParam, args...); err != nil {
		s.fail(w, r, s.translatePoolWrite(err))
		return
	}
	p, err := getPool(ctx, s.db, id)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "pool_reread_error: "+err.Error()))
		return
	}
	s.logOK(requestIDFrom(r.Context()), r, "pool_updated id="+id)
	writeJSON(w, http.StatusOK, p)
}
