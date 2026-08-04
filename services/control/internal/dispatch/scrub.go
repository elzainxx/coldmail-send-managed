package dispatch

import "encoding/json"

// mustJSON marshals a header map for the send_logs.headers JSONB column. A header map of strings
// never fails to marshal; on the impossible error path it returns an empty object rather than
// panicking a report.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// scrubSmtpResponse is the outcome handler's defense-in-depth choke point (§5.5). The REAL scrub
// happens on the worker (services/worker/src/smtp.js scrubSecret), which now redacts both the plaintext
// AND the base64/base64url forms of the credential before the response ever leaves the worker — so a
// pathological server that echoes the AUTH argument cannot land a decodable secret here. The Control
// zeroed its own plaintext copy at push (§3.3) and has no per-request secret at outcome time, so
// it cannot re-derive the value to match; this returns the (already worker-scrubbed) response
// unchanged. Kept as a named choke point + guarded by the no-secret-leak gate and S1 (which
// exercises a base64-echoing sink), so the whole chain is asserted clean.
func scrubSmtpResponse(resp string) string {
	return resp
}
