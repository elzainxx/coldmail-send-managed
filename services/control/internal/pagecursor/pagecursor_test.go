package pagecursor

// Pure unit tests of the cursor codec (no database): the reasons each bad value is refused, the control that a
// genuine cursor is accepted, and the scope binding. The ROUTE tests that prove each paginated route answers 400
// (never 500) live beside the routes (platformapi/cursor_forged_test.go, adminapi/cursor_forged_test.go) and run
// against real Postgres.

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

const (
	tsOK  = "2026-11-18T10:00:00.123456Z"
	idOK  = "6f0c1e52-3b7a-4d19-9a4e-2c8f5b1d7e30"
	scope = "platform.report|u1|c1"
)

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func TestGenuineCursorRoundTrips(t *testing.T) {
	pos, err := Decode(Encode(scope, tsOK, idOK), scope)
	if err != nil {
		t.Fatalf("control: a cursor minted by Encode was refused: %v", err)
	}
	if pos.CreatedAt != tsOK || pos.ID != idOK {
		t.Fatalf("round trip changed the position: %+v", pos)
	}
	// the legacy shape (every cursor minted before this change) is still accepted, with the same strictness
	legacy, err := Decode(b64(tsOK+"|"+idOK), scope)
	if err != nil || legacy.CreatedAt != tsOK || legacy.ID != idOK {
		t.Fatalf("a legacy two-field cursor was refused or altered: %+v %v", legacy, err)
	}
	// the admin routes emit a fixed-width microsecond timestamp; it must parse too
	if _, err := Decode(Encode("admin.pools", "2026-11-18T10:00:00.000001Z", idOK), "admin.pools"); err != nil {
		t.Fatalf("an admin-style cursor was refused: %v", err)
	}
	// an offset timestamp (pgx may format in local time) parses too
	if _, err := Decode(Encode(scope, "2026-11-18T12:00:00.5+02:00", idOK), scope); err != nil {
		t.Fatalf("an offset timestamp was refused: %v", err)
	}
}

func TestBadCursorsAreMalformed(t *testing.T) {
	long := strings.Repeat("A", maxRawLen+1)
	cases := map[string]string{
		"not base64url":                 "!!!not-base64!!!",
		"padded base64 (not raw)":       base64.URLEncoding.EncodeToString([]byte(tsOK + "|" + idOK + "x")),
		"empty after decode":            b64(""),
		"no pipe":                       b64("justtext"),
		"timestamp is not a time":       b64("yesterday|" + idOK),
		"id is not a uuid":              b64(tsOK + "|not-a-uuid"),
		"id is a uuid without hyphens":  b64(tsOK + "|6f0c1e523b7a4d199a4e2c8f5b1d7e30"),
		"id with SQL":                   b64(tsOK + "|' OR 1=1 --"),
		"empty id":                      b64(tsOK + "|"),
		"empty timestamp":               b64("|" + idOK),
		"three fields":                  b64("v1|" + tsOK + "|" + idOK),
		"five fields":                   b64("v1|" + tsOK + "|" + idOK + "|aa|bb"),
		"unknown version":               b64("v9|" + tsOK + "|" + idOK + "|" + scopeTag(scope)),
		"v1 with a bad timestamp":       b64("v1|x|" + idOK + "|" + scopeTag(scope)),
		"v1 with a bad id":              b64("v1|" + tsOK + "|x|" + scopeTag(scope)),
		"far-future-looking garbage ts": b64("99999-99-99T99:99:99Z|" + idOK),
		"oversized value":               long,
		"empty string":                  "",
	}
	for name, raw := range cases {
		_, err := Decode(raw, scope)
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: Decode(%q) = %v, want ErrMalformed", name, truncate(raw), err)
		}
	}
}

func TestCursorOfAnotherScopeIsRefusedAsWrongScope(t *testing.T) {
	minted := Encode("platform.report|u1|c1", tsOK, idOK)
	for name, other := range map[string]string{
		"another campaign":      "platform.report|u1|c2",
		"another user":          "platform.report|u2|c1",
		"another route":         "admin.accounts",
		"the same ids reversed": "platform.report|c1|u1",
	} {
		_, err := Decode(minted, other)
		if !errors.Is(err, ErrWrongScope) {
			t.Errorf("%s: Decode = %v, want ErrWrongScope", name, err)
		}
	}
	// control: the SAME cursor for its own scope is accepted — the refusals above are the scope, not the codec
	if _, err := Decode(minted, "platform.report|u1|c1"); err != nil {
		t.Fatalf("control: the cursor was refused for its own scope: %v", err)
	}
}

// A malformed body is reported as malformed even when the scope tag is also wrong (the field checks come first),
// so the log reason is the most basic one.
func TestMalformedBeatsWrongScope(t *testing.T) {
	raw := b64("v1|nope|" + idOK + "|0000000000000000")
	if _, err := Decode(raw, scope); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Decode = %v, want ErrMalformed", err)
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
