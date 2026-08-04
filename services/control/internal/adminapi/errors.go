// Package adminapi is the Control's operator-only admin surface (Domain B): /v1/admin/*.
// Phase 2 scope — account & pool CRUD + lifecycle, config management, credential intake. It is
// the ONLY writer of account credentials (Encrypt-only; never Decrypt — OD-7). NO dispatch,
// routing, SMTP, scoring, or auto-transitions live here (those are Phases 4/5+).
//
// Two-vocabulary rule (CLAUDE.md Rule 5): a rich INTERNAL reason is logged against a request_id
// and NEVER serialized; the client only ever sees {code, message, details[], request_id}. No
// credential, ciphertext, key-id, SQL, SQLSTATE, stack trace, or internal hostname crosses the
// client boundary.
package adminapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// External status codes (§1.3) — a stable, additive-only enum. NEVER rename/repurpose.
const (
	codeValidationFailed        = "validation_failed"         // 400
	codeConfigOrderingViolation = "config_ordering_violation" // 400 (Phase 9 §4.6/E4; P9-OD-2 RULED-AND-CLOSED)
	codeUnauthenticated         = "unauthenticated"           // 401
	codeForbidden               = "forbidden"                 // 403 (reserved; unused in V1's single scope)
	codeNotFound                = "not_found"                 // 404
	codeMethodNotAllowed        = "method_not_allowed"        // 405 (additive, fix-plan item 7b / audit D17)
	codeConflict                = "conflict"                  // 409
	codeUnprocessable           = "unprocessable"             // 422
	codeRateLimited             = "rate_limited"              // 429 (off by default in V1)
	codeInternalError           = "internal_error"            // 500
)

// issue slugs (§1.2) — the fixed field-level vocabulary. Never free-text echoing input.
const (
	issueRequired       = "required"
	issueOutOfRange     = "out_of_range"
	issueMalformed      = "malformed"
	issueNotFound       = "not_found"
	issueConflict       = "conflict"
	issueForbiddenValue = "forbidden_value"
)

// fixedMessages maps an external code to its FIXED, caller-safe sentence. A handler may override
// the message ONLY with another curated constant (never an interpolation of untrusted input).
var fixedMessages = map[string]string{
	codeValidationFailed:        "One or more fields are invalid.",
	codeConfigOrderingViolation: "This value would break a required ordering relationship with another config key.",
	codeUnauthenticated:         "Missing or invalid admin API key.",
	codeForbidden:               "This action is not permitted.",
	codeNotFound:                "The requested resource was not found.",
	codeMethodNotAllowed:        "This method is not allowed on this path.",
	codeConflict:                "The request conflicts with the current state.",
	codeUnprocessable:           "The request is well-formed but cannot be processed.",
	codeRateLimited:             "Too many requests.",
	codeInternalError:           "An unexpected error occurred.",
}

var codeStatus = map[string]int{
	codeValidationFailed:        http.StatusBadRequest,
	codeConfigOrderingViolation: http.StatusBadRequest,
	codeUnauthenticated:         http.StatusUnauthorized,
	codeForbidden:               http.StatusForbidden,
	codeNotFound:                http.StatusNotFound,
	codeMethodNotAllowed:        http.StatusMethodNotAllowed,
	codeConflict:                http.StatusConflict,
	codeUnprocessable:           http.StatusUnprocessableEntity,
	codeRateLimited:             http.StatusTooManyRequests,
	codeInternalError:           http.StatusInternalServerError,
}

// fieldDetail is one structured field-level error (§1.2 details[]). Never free-text.
type fieldDetail struct {
	Field string `json:"field"`
	Issue string `json:"issue"`
}

// apiError is the handler-internal error carrier. Its `message` is a curated safe sentence and
// `details` are structured; `internalReason` is the RICH internal string that is logged against
// the request_id and NEVER serialized to the client (two-vocabulary rule).
type apiError struct {
	code           string
	message        string // safe, curated; defaults to fixedMessages[code] if empty
	details        []fieldDetail
	internalReason string // logged, never returned
}

func (e *apiError) Error() string { return e.code + ": " + e.internalReason }

// newErr builds an apiError with the fixed message for its code.
func newErr(code, internalReason string) *apiError {
	return &apiError{code: code, internalReason: internalReason}
}

// withDetails attaches structured field details.
func (e *apiError) withDetails(d ...fieldDetail) *apiError {
	e.details = append(e.details, d...)
	return e
}

// withMessage overrides the message with a CURATED constant (caller must pass a fixed string,
// never interpolated untrusted/sensitive input — §1.2).
func (e *apiError) withMessage(m string) *apiError {
	e.message = m
	return e
}

// errorEnvelope is the exact wire shape (§1.2). details omitted when empty.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}
type errorBody struct {
	Code      string        `json:"code"`
	Message   string        `json:"message"`
	Details   []fieldDetail `json:"details,omitempty"`
	RequestID string        `json:"request_id"`
}

// newRequestID mints an opaque, high-entropy correlation id. It bridges the two vocabularies:
// the client quotes it; the internal reason lives in logs keyed by it.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand should never fail; degrade to a static marker rather than panic a request.
		return "req_0000000000000000"
	}
	return "req_" + hex.EncodeToString(b[:])
}

// writeJSON serializes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError serializes the clean external envelope for e (internal reason is NOT included).
func writeError(w http.ResponseWriter, requestID string, e *apiError) {
	msg := e.message
	if msg == "" {
		msg = fixedMessages[e.code]
	}
	status, ok := codeStatus[e.code]
	if !ok {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, errorEnvelope{Error: errorBody{
		Code:      e.code,
		Message:   msg,
		Details:   e.details,
		RequestID: requestID,
	}})
}
