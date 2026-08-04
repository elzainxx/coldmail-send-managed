package adminapi

import (
	"fmt"
	"net/http"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

// Authenticator gates /v1/admin/* with an operator bearer key (§4.2). V1 has exactly one scope,
// platform_admin (§4.3): any valid key grants it. Keys are stored HASHED (bcrypt, same discipline
// as send_managed.boxes.token_hash) in the admin service's env/secret manager — NO per-principal DB
// table in V1 (OD-9 dropped). Comparison is constant-time via bcrypt.
type Authenticator struct {
	// keyHashes are bcrypt hashes of the accepted admin keys. Empty => fail-closed (reject all),
	// mirroring the workerlink registry's fail-closed posture.
	keyHashes []string
}

// NewAuthenticator builds an Authenticator from a set of bcrypt key-hashes. Blank entries are
// dropped. An empty result fails closed.
func NewAuthenticator(keyHashes []string) *Authenticator {
	var hs []string
	for _, h := range keyHashes {
		if strings.TrimSpace(h) != "" {
			hs = append(hs, h)
		}
	}
	return &Authenticator{keyHashes: hs}
}

// authenticate reports whether the presented bearer key matches any configured hash. Fail-closed:
// no configured hashes, or a missing/blank key, rejects. bcrypt.CompareHashAndPassword is
// constant-time with respect to the correct key.
func (a *Authenticator) authenticate(bearer string) bool {
	if len(a.keyHashes) == 0 || bearer == "" {
		return false
	}
	ok := false
	// Check every hash (do not early-return on first match) so timing does not reveal which key
	// index matched. bcrypt itself is constant-time against the stored hash.
	for _, h := range a.keyHashes {
		if bcrypt.CompareHashAndPassword([]byte(h), []byte(bearer)) == nil {
			ok = true
		}
	}
	return ok
}

// bearerToken extracts the token from an "Authorization: Bearer <key>" header (case-insensitive
// scheme). Returns "" if absent/malformed.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// requireAuth wraps a handler with the operator-key gate. On failure it writes a clean
// 401 unauthenticated envelope (no internal reason) and logs the rejection against the
// request_id. The admin key is NEVER logged. It also installs a panic recover (defense in
// depth): a bug that panics inside a handler must still degrade to a clean 500 envelope with a
// request_id (two-vocabulary rule, CLAUDE.md Rule 5) — NEVER a bare connection reset with no
// response. The recover runs before any handler body, so the rid it uses is the same one the
// handler chain sees.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid := newRequestID()
		r = r.WithContext(withRequestID(r.Context(), rid))

		defer func() {
			if rec := recover(); rec != nil {
				// The panic value/stack is internal detail: logged against the rid, never
				// serialized. If the handler already wrote a status this WriteHeader is a no-op
				// (net/http guards it), but a mid-handler panic before any write still yields a
				// clean envelope.
				s.logError(rid, r, newErr(codeInternalError, fmt.Sprintf("panic_recovered: %v", rec)))
				writeError(w, rid, newErr(codeInternalError, "panic_recovered"))
			}
		}()

		if !s.auth.authenticate(bearerToken(r)) {
			s.logReject(rid, r, "admin_key_invalid_or_missing")
			writeError(w, rid, newErr(codeUnauthenticated, "admin_key_invalid_or_missing"))
			return
		}
		next(w, r)
	}
}
