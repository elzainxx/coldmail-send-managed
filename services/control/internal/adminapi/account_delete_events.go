package adminapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

// handleAccountDelete: DELETE /v1/admin/accounts/{id} (§2.5). Hard delete. No FK referencing
// send_managed.accounts.id uses RESTRICT (jobs/job_events/account_events are ON DELETE SET NULL), so
// nothing blocks it. Writes one 'deleted' audit row BEFORE the row removal in the SAME tx; the
// FK ON DELETE SET NULL then nulls that row's account_id as part of the DELETE — so the audit
// survives. Returns 204. (Disable is the everyday "take out of service" verb — §2.5.)
func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isUUID(id) {
		s.fail(w, r, newErr(codeNotFound, "account_id_malformed"))
		return
	}
	ctx, cancel := dbCtx(r.Context())
	defer cancel()

	cur, err := getAccount(ctx, s.db, id)
	if err == errAccountNotFound {
		s.fail(w, r, newErr(codeNotFound, "account_not_found:"+id))
		return
	}
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "account_get_error: "+err.Error()))
		return
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "tx_begin_error: "+err.Error()))
		return
	}
	defer tx.Rollback(context.Background())

	// Audit BEFORE the delete (same tx). from = current flags; to = nil (no post-state).
	fromSnap := ptrSnapshot(cur.Status.Paused, cur.Status.PauseReason, cur.Status.Quarantined, cur.Status.Disabled)
	if err := writeAuditEvent(ctx, tx, id, cur.UserID, "deleted", fromSnap, nil, nil, requestIDFrom(r.Context())); err != nil {
		s.fail(w, r, newErr(codeInternalError, "audit_write_error: "+err.Error()))
		return
	}
	if _, err := tx.Exec(ctx, "DELETE FROM send_managed.accounts WHERE id=$1", id); err != nil {
		s.fail(w, r, s.translateAccountWrite(err))
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.fail(w, r, s.translateAccountWrite(err))
		return
	}
	s.logOK(requestIDFrom(r.Context()), r, "account_deleted id="+id)
	w.WriteHeader(http.StatusNoContent)
}

// accountEvent is one audit row as returned by the events read (§7.4). reason IS operator-facing
// here (the operator's own note); credential material is never present (the table holds none).
type accountEvent struct {
	ID        string         `json:"id"`
	AccountID *string        `json:"account_id"`
	UserID    string         `json:"user_id"`
	Actor     string         `json:"actor"`
	Action    string         `json:"action"`
	FromFlags *flagsSnapshot `json:"from_flags"`
	ToFlags   *flagsSnapshot `json:"to_flags"`
	Reason    *string        `json:"reason"`
	RequestID *string        `json:"request_id"`
	CreatedAt string         `json:"created_at"`

	// cursorTS: created_at at full microsecond precision, for the keyset cursor only (never serialized) —
	// see account_store.go's accountResource.cursorTS.
	cursorTS string `json:"-"`
}

// handleAccountEvents: GET /v1/admin/accounts/{id}/events (§7.4). Cursor-paginated audit trail
// for one account (most-recent-first is friendlier, but keyset stays ascending on (created_at,
// id) for stability; operators page forward). The {id} may reference an account whose rows have
// nulled account_id after a delete — so we also match on the path id directly while it exists.
func (s *Server) handleAccountEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isUUID(id) {
		s.fail(w, r, newErr(codeNotFound, "account_id_malformed"))
		return
	}
	limit := parseLimit(r)
	cur, cerr := parseCursor(r, cursorScopeAccountEvents(id))
	if cerr != nil {
		s.fail(w, r, cerr)
		return
	}

	conds := []string{"e.account_id = $1"}
	args := []any{id}
	if cur != nil {
		args = append(args, cur.CreatedAt)
		p1 := "$" + strconv.Itoa(len(args))
		args = append(args, cur.ID)
		p2 := "$" + strconv.Itoa(len(args))
		conds = append(conds, "(e.created_at, e.id) > ("+p1+"::timestamptz, "+p2+"::uuid)")
	}
	args = append(args, limit+1)
	limitParam := "$" + strconv.Itoa(len(args))
	query := `SELECT e.id, e.account_id, e.user_id, e.actor, e.action, e.from_flags, e.to_flags,
	          e.reason, e.request_id,
	          to_char(e.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	          to_char(e.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
	          FROM send_managed.account_events e WHERE ` + strings.Join(conds, " AND ") +
		" ORDER BY e.created_at ASC, e.id ASC LIMIT " + limitParam

	ctx, cancel := dbCtx(r.Context())
	defer cancel()
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		s.fail(w, r, newErr(codeInternalError, "events_query_error: "+err.Error()))
		return
	}
	defer rows.Close()

	items := []accountEvent{}
	for rows.Next() {
		var (
			ev               accountEvent
			fromJSON, toJSON []byte
		)
		if err := rows.Scan(&ev.ID, &ev.AccountID, &ev.UserID, &ev.Actor, &ev.Action,
			&fromJSON, &toJSON, &ev.Reason, &ev.RequestID, &ev.CreatedAt, &ev.cursorTS); err != nil {
			s.fail(w, r, newErr(codeInternalError, "events_scan_error: "+err.Error()))
			return
		}
		ev.FromFlags = decodeFlags(fromJSON)
		ev.ToFlags = decodeFlags(toJSON)
		items = append(items, ev)
	}
	if err := rows.Err(); err != nil {
		s.fail(w, r, newErr(codeInternalError, "events_rows_error: "+err.Error()))
		return
	}

	// Cursor from the LAST RETURNED item at FULL sort-key precision — see handleAccountList.
	var nextCursor *string
	if len(items) > limit {
		last := items[limit-1]
		items = items[:limit]
		c := encodeCursor(cursorScopeAccountEvents(id), last.cursorTS, last.ID)
		nextCursor = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nextCursor})
}
