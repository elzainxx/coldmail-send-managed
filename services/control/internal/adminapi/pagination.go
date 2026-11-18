package adminapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/coldmail-send-managed/control/internal/pagecursor"
)

// Cursor pagination (§1.4): keyset on (created_at, id) — stable under concurrent inserts (no
// offset). ?limit= default 50, max 200. ?cursor= is opaque (base64 of "created_at|id").
// Response envelope: { "items": [...], "next_cursor": "…" | null }.

const (
	defaultLimit = 50
	maxLimit     = 200
)

// parseLimit reads ?limit=, clamping to [1, maxLimit] with a default. A malformed value falls
// back to the default rather than erroring (a convenience knob, not a validated field).
func parseLimit(r *http.Request) int {
	q := r.URL.Query().Get("limit")
	if q == "" {
		return defaultLimit
	}
	n, err := strconv.Atoi(q)
	if err != nil || n <= 0 {
		return defaultLimit
	}
	if n > maxLimit {
		return maxLimit
	}
	return n
}

// cursor carries the keyset position. CreatedAt is the sort key at FULL microsecond precision
// ("2006-01-02T15:04:05.000000Z"), NOT the second-truncated created_at the resources serialize: a cursor
// that rounds the sort key down compares as earlier than the row it names and re-admits it (and every row
// of the same second) on the next page. Handlers build it from the resource's unexported cursorTS.
type cursor struct {
	CreatedAt string
	ID        string
}

// The scope a list route's cursor is valid for (pagecursor): a cursor minted by one of these routes is refused
// by every other, and the account-events cursor is bound to ITS account as well.
const (
	cursorScopeAccounts = "admin.accounts"
	cursorScopePools    = "admin.pools"
)

func cursorScopeAccountEvents(accountID string) string { return "admin.account_events|" + accountID }

// parseCursor decodes ?cursor= for the list route whose scope is given (Phase 3 close-out B2). Returns
// (nil, nil) when absent. A bad, malformed or forged cursor — not base64url, not <timestamp>|<uuid>, a
// timestamp or id that would not survive the SQL casts, or a cursor minted for ANOTHER route or account — is
// a 400 validation_failed with details [cursor:malformed], decided before any query runs: it can never be a
// 500, and no row is read for it. The two refusals look identical to the caller; the internal reason (logged
// against the request id) tells them apart.
func parseCursor(r *http.Request, scope string) (*cursor, *apiError) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return nil, nil
	}
	pos, err := pagecursor.Decode(raw, scope)
	if err != nil {
		reason := "cursor_malformed"
		if errors.Is(err, pagecursor.ErrWrongScope) {
			reason = "cursor_wrong_scope"
		}
		return nil, newErr(codeValidationFailed, reason).
			withDetails(fieldDetail{Field: "cursor", Issue: issueMalformed})
	}
	return &cursor{CreatedAt: pos.CreatedAt, ID: pos.ID}, nil
}

// encodeCursor builds the opaque next_cursor from the LAST RETURNED item's keyset (the page's own final
// row — never the extra row fetched only to learn whether another page exists), bound to the route's scope.
func encodeCursor(scope, createdAt, id string) string {
	return pagecursor.Encode(scope, createdAt, id)
}
