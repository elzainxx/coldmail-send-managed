package platformapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// External status codes (§6 / two-vocabulary rule). Additive-only; never rename/repurpose.
const (
	codeValidationFailed = "validation_failed"  // 400
	codeUnauthenticated  = "unauthenticated"    // 401
	codeNotFound         = "not_found"          // 404
	codeMethodNotAllowed = "method_not_allowed" // 405 (additive, fix-plan item 7b / audit D17)
	codeConflict         = "conflict"           // 409
	codeInternalError    = "internal_error"     // 500
	codePaused           = "paused"             // 503 (additive, fix-plan item 12)
)

// codePaused is the intake refusal at the `refuse_new` and `stopped` rungs of the dispatch ladder
// (send_managed_0025, dispatch_rung.go). Three notes, because each was a decision:
//
//   - IT IS SPELLED THE SAME AS M1's AND M3's. M1's control-plane emits `paused` over 503 for exactly
//     this condition — an operator deliberately pulled a kill-switch rung — and a platform client
//     should not need a per-module word for one state (canonical-module-contract §1.3). Transcribed
//     from M1's control-plane/internal/api/errors.go, not imported: the modules are separate Go
//     modules (OD-7), so this is a checked copy, and M1's own comment about the reverse direction
//     applies here too — a rename on either side is invisible to the other. That direction is
//     fix-plan item 14's job. What IS checked today is M1's errors_test.go senderStatuses table,
//     which fails if M1 maps `paused` to anything but 503.
//
//   - 503 AND NOT 429. A rate limit says "you are asking too often"; this says "this module is not
//     working right now, through no fault of your request." A client that treats 503 as retriable
//     back-off does the right thing; one that reads 429 might legitimately conclude it should shard
//     its callers, which would not help at all.
//
//   - IT IS NOT `unavailable`. M1 carries BOTH, distinguishing "this deployment is not wired" from
//     "an operator pulled a rung". This module has no unwired-deployment state to express (it cannot
//     boot without its DB), so it takes only the half it can actually emit. Adding an `unavailable`
//     it never returns would put a code in the published error_codes[] that no request can produce.

// issue slugs — the fixed field-level vocabulary (never free-text echoing input). The full
// documented enum is Docs/api-contract.md's `details[].issue`; this domain uses the four below.
const (
	issueRequired   = "required"
	issueMalformed  = "malformed"
	issueOutOfRange = "out_of_range"
	issueNotFound   = "not_found"
)

var fixedMessages = map[string]string{
	codeValidationFailed: "One or more fields are invalid.",
	codeUnauthenticated:  "Missing or invalid platform service token.",
	codeNotFound:         "The requested resource was not found.",
	codeMethodNotAllowed: "This method is not allowed on this path.",
	codeConflict:         "The request conflicts with the current state.",
	codeInternalError:    "An unexpected error occurred.",
	codePaused:           "Dispatch is paused by an operator; retry later.",
}

var codeStatus = map[string]int{
	codeValidationFailed: http.StatusBadRequest,
	codeUnauthenticated:  http.StatusUnauthorized,
	codeNotFound:         http.StatusNotFound,
	codeMethodNotAllowed: http.StatusMethodNotAllowed,
	codeConflict:         http.StatusConflict,
	codeInternalError:    http.StatusInternalServerError,
	codePaused:           http.StatusServiceUnavailable,
}

// fieldDetail is one structured field-level error.
type fieldDetail struct {
	Field string `json:"field"`
	Issue string `json:"issue"`
}

// apiError is the handler-internal error carrier. message is a curated safe sentence;
// internalReason is logged against the request_id and NEVER serialized (two-vocabulary rule).
type apiError struct {
	code           string
	message        string
	details        []fieldDetail
	internalReason string
}

func (e *apiError) Error() string { return e.code + ": " + e.internalReason }

func newErr(code, internalReason string) *apiError {
	return &apiError{code: code, internalReason: internalReason}
}

func (e *apiError) withDetails(d ...fieldDetail) *apiError {
	e.details = append(e.details, d...)
	return e
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}
type errorBody struct {
	Code      string        `json:"code"`
	Message   string        `json:"message"`
	Details   []fieldDetail `json:"details,omitempty"`
	RequestID string        `json:"request_id"`
}

// newRequestID mints an opaque correlation id bridging the two vocabularies.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req_0000000000000000"
	}
	return "req_" + hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

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
