package adminapi

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
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

// cursor carries the keyset position. created_at is an RFC3339-ish string as stored/serialized.
type cursor struct {
	CreatedAt string
	ID        string
}

// parseCursor decodes ?cursor=. Returns (nil, nil) when absent, or (nil, apiError) if malformed.
func parseCursor(r *http.Request) (*cursor, *apiError) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return nil, nil
	}
	dec, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, newErr(codeValidationFailed, "cursor_malformed").
			withDetails(fieldDetail{Field: "cursor", Issue: issueMalformed})
	}
	parts := strings.SplitN(string(dec), "|", 2)
	if len(parts) != 2 || parts[1] == "" {
		return nil, newErr(codeValidationFailed, "cursor_malformed").
			withDetails(fieldDetail{Field: "cursor", Issue: issueMalformed})
	}
	return &cursor{CreatedAt: parts[0], ID: parts[1]}, nil
}

// encodeCursor builds the opaque next_cursor from the last item's keyset.
func encodeCursor(createdAt, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt + "|" + id))
}
