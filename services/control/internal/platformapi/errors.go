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
)

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
}

var codeStatus = map[string]int{
	codeValidationFailed: http.StatusBadRequest,
	codeUnauthenticated:  http.StatusUnauthorized,
	codeNotFound:         http.StatusNotFound,
	codeMethodNotAllowed: http.StatusMethodNotAllowed,
	codeConflict:         http.StatusConflict,
	codeInternalError:    http.StatusInternalServerError,
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
