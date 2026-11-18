package adminapi

import (
	"net/http"
	"strconv"
	"strings"
)

// handleAccountGet: GET /v1/admin/accounts/{id} (§2.3). No secret, ever.
func (s *Server) handleAccountGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isUUID(id) {
		s.fail(w, r, newErr(codeNotFound, "account_id_malformed"))
		return
	}
	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	a, err := getAccount(ctx, s.db, id)
	if err == errAccountNotFound {
		s.fail(w, r, newErr(codeNotFound, "account_not_found:"+id))
		return
	}
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "account_get_error: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// handleAccountList: GET /v1/admin/accounts (§2.3). Filters ?pool_id=, ?paused=, ?quarantined=,
// ?disabled=, ?user_id=. Operator sees ALL accounts. Cursor pagination on (created_at, id). Every
// item is the §2.1 object — no secret.
func (s *Server) handleAccountList(w http.ResponseWriter, r *http.Request) {
	limit := parseLimit(r)
	cur, cerr := parseCursor(r, cursorScopeAccounts)
	if cerr != nil {
		s.fail(w, r, cerr)
		return
	}

	var conds []string
	var args []any
	add := func(cond string, val any) {
		args = append(args, val)
		conds = append(conds, strings.Replace(cond, "?", "$"+strconv.Itoa(len(args)), 1))
	}
	q := r.URL.Query()
	if v := q.Get("pool_id"); v != "" {
		if !isUUID(v) {
			s.fail(w, r, newErr(codeValidationFailed, "pool_id_filter_malformed").
				withDetails(fieldDetail{Field: "pool_id", Issue: issueMalformed}))
			return
		}
		add("a.pool_id = ?", v)
	}
	if v := q.Get("user_id"); v != "" {
		if !isUUID(v) {
			s.fail(w, r, newErr(codeValidationFailed, "user_id_filter_malformed").
				withDetails(fieldDetail{Field: "user_id", Issue: issueMalformed}))
			return
		}
		add("a.user_id = ?", v)
	}
	for _, bf := range []struct{ q, col string }{{"paused", "a.paused"}, {"quarantined", "a.quarantined"}, {"disabled", "a.disabled"}} {
		if v := q.Get(bf.q); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				s.fail(w, r, newErr(codeValidationFailed, bf.q+"_filter_malformed").
					withDetails(fieldDetail{Field: bf.q, Issue: issueMalformed}))
				return
			}
			add(bf.col+" = ?", b)
		}
	}
	if cur != nil {
		args = append(args, cur.CreatedAt)
		p1 := "$" + strconv.Itoa(len(args))
		args = append(args, cur.ID)
		p2 := "$" + strconv.Itoa(len(args))
		conds = append(conds, "(a.created_at, a.id) > ("+p1+"::timestamptz, "+p2+"::uuid)")
	}

	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, limit+1)
	limitParam := "$" + strconv.Itoa(len(args))
	query := "SELECT " + accountSelectCols + " FROM send_managed.accounts a" + where +
		" ORDER BY a.created_at ASC, a.id ASC LIMIT " + limitParam

	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "account_list_query_error: "+err.Error()))
		return
	}
	defer rows.Close()

	items := []accountResource{}
	for rows.Next() {
		a, serr := scanAccount(rows)
		if serr != nil {
			s.fail(w, r, newErr(codeInternalError, "account_list_scan_error: "+serr.Error()))
			return
		}
		items = append(items, *a)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, newErr(codeInternalError, "account_list_rows_error: "+err.Error()))
		return
	}

	// The cursor is built from the LAST RETURNED item (items[limit-1]) — never from the extra (limit+1)th
	// row, which only tells us there IS another page — and from its sort key at FULL precision
	// (cursorTS), not from the second-truncated created_at the resource serializes. A cursor carrying
	// "…:05Z" for a row created at …:05.347 compares as EARLIER than that row, so the keyset predicate
	// `(created_at, id) > (cursor)` re-admits it and every row of that second already served: duplicates on
	// the next page, and — when a second holds more than `limit` rows — the same page forever. The
	// keyset's own listing tests (listing_cursor_test.go) pin both halves.
	var nextCursor *string
	if len(items) > limit {
		last := items[limit-1]
		items = items[:limit]
		c := encodeCursor(cursorScopeAccounts, last.cursorTS, last.ID)
		nextCursor = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nextCursor})
}
