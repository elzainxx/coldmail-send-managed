// Package pagecursor is the one place a list route's opaque `cursor` is built and read (Phase 3 close-out B2).
//
// EVERY paginated route of this module — the campaign report's items (Domain A) and the operator's account,
// pool and account-event lists (Domain B) — pages on the same keyset (created_at, id) and hands the caller an
// opaque `next_cursor` to pass back. Before this package each route had its own copy of "base64url decode,
// split on |", which accepted any value with a pipe in it; a base64 value whose body was not
// `<timestamp>|<uuid>` reached the `::timestamptz` / `::uuid` casts in SQL and came back as a 500. A client
// (or an attacker) must never be able to turn a query-string value into a server error, so:
//
//   - Decode accepts exactly two shapes and VALIDATES every field before it can reach SQL: the timestamp must
//     parse as RFC 3339 and the id must be a canonical UUID. Anything else is ErrMalformed, which every route
//     answers as `400 validation_failed` with details `[cursor:malformed]`, never a 500.
//   - A cursor is bound to the SCOPE it was minted for (the route, and for a tenant route the user and the
//     campaign). A cursor minted for another scope is ErrWrongScope, answered identically (the same 400), and
//     no row of the other scope is ever read: the query was never run. The scope travels as a short hash, so
//     it discloses nothing and is not a secret: it is a CHECK AGAINST MIX-UPS (a cursor of campaign A used on
//     campaign B, of one user's report on another's, of /accounts on /pools), not authentication. A caller can
//     still write a well-formed cursor of its OWN scope by hand; that names a position inside its own,
//     tenant-scoped result set, so the worst it can do is skip or repeat rows the caller may read anyway.
//
// Wire format. Current (v1): base64url(no padding) of `v1|<created_at>|<id>|<scope tag>`. Legacy: base64url of
// `<created_at>|<id>` — what every cursor minted before this change looks like. A legacy cursor is STILL
// ACCEPTED (so a walk that is in progress across a deploy does not break), validated as strictly as v1, but it
// carries no scope and so cannot be refused for the wrong one; new cursors are always v1. The format is opaque
// to clients (the contract says "pass back exactly what next_cursor returned").
package pagecursor

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
)

// ErrMalformed: the value is not a cursor this package could have minted (not base64url, the wrong number of
// fields, an unparseable timestamp, an id that is not a canonical UUID, too long). Routes answer 400.
var ErrMalformed = errors.New("cursor malformed")

// ErrWrongScope: a well-formed v1 cursor minted for a different route, user or campaign. Routes answer the
// SAME 400 as ErrMalformed (the caller cannot tell the two apart); the distinction exists for the log.
var ErrWrongScope = errors.New("cursor minted for a different scope")

// maxRawLen bounds the query-string value before any decoding: a real cursor is under 120 bytes, so 512 is
// generous and a megabyte of garbage is refused without being decoded.
const maxRawLen = 512

// Position is a keyset position: the (created_at, id) of the LAST row of the previous page. Both fields are
// the validated original strings, safe to pass to `$n::timestamptz` / `$n::uuid`.
type Position struct {
	CreatedAt string
	ID        string
}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// scopeTag is the short, non-reversible tag of a scope string.
func scopeTag(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(sum[:])[:16]
}

// Encode builds the opaque next_cursor for the last row of a page. createdAt and id come from the row and are
// emitted verbatim; scope names what the cursor is valid for (see the package comment).
func Encode(scope, createdAt, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte("v1|" + createdAt + "|" + id + "|" + scopeTag(scope)))
}

// Decode reads and validates a non-empty cursor for the given scope. It returns ErrMalformed for any value
// that is not a well-formed cursor and ErrWrongScope for a well-formed v1 cursor minted for another scope.
func Decode(raw, scope string) (Position, error) {
	if raw == "" || len(raw) > maxRawLen {
		return Position{}, ErrMalformed
	}
	dec, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return Position{}, ErrMalformed
	}
	parts := strings.Split(string(dec), "|")
	var pos Position
	switch len(parts) {
	case 2: // legacy: created_at|id
		pos = Position{CreatedAt: parts[0], ID: parts[1]}
	case 4: // v1: v1|created_at|id|scope tag
		if parts[0] != "v1" {
			return Position{}, ErrMalformed
		}
		pos = Position{CreatedAt: parts[1], ID: parts[2]}
	default:
		return Position{}, ErrMalformed
	}
	if _, perr := time.Parse(time.RFC3339Nano, pos.CreatedAt); perr != nil {
		return Position{}, ErrMalformed
	}
	if !uuidRe.MatchString(pos.ID) {
		return Position{}, ErrMalformed
	}
	if len(parts) == 4 && parts[3] != scopeTag(scope) {
		return Position{}, ErrWrongScope
	}
	return pos, nil
}
