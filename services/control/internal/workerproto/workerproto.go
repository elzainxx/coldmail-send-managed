// Package workerproto defines the Phase-3 worker↔Control control-channel message shapes (§2.2–§2.4).
// All messages are JSON objects with a `type` discriminator and a `request_id` for correlation,
// exchanged as newline-delimited JSON frames over the held mTLS stream (§2.1: the specific wire
// framing is an implementation choice; the LOCKED requirements are mutual auth + encryption +
// Control-initiated push on the held stream — the worker ships no RPC dependency, so line-delimited
// JSON over crypto/tls is the dependency-free realization on both sides).
//
// Two-vocabulary rule (§2.6): nothing on this internal channel flows verbatim to a platform
// caller. error_reason is an INTERNAL slug (logged, mapped to error_class='unknown' in Phase 3);
// smtp.secret is the decrypted plaintext scoped to ONE job and is never logged/persisted.
package workerproto

// Message type discriminators (§2.2). Additive-only.
const (
	TypeRegister    = "register"     // worker → Control: authenticate on stream open (§3.1)
	TypeRegisterAck = "register_ack" // Control → worker: accept/reject registration
	TypeHeartbeat   = "heartbeat"    // worker → Control: liveness ping (§3.2)
	TypePushJob     = "push_job"     // Control → worker: one job + its decrypted credential (§2.3)
	TypeReport      = "report"       // worker → Control: outcome of one job (§2.4)
	TypeReportAck   = "report_ack"   // Control → worker: outcome DURABLY persisted (Phase 7 §1.3, P7-4)
)

// Outcome values (§2.4). Phase 3 does NOT classify an error taxonomy (Phase 5).
const (
	OutcomeAccepted = "accepted"
	OutcomeError    = "error"
	OutcomeBounced  = "bounced" // hard 550; the locked state machine already models sending→bounced
)

// SMTP security modes (§2.3). Derived by the Control from the port, explicit so the worker does not
// re-derive policy.
const (
	SecurityStartTLS    = "starttls"     // 587/25: STARTTLS required before AUTH
	SecurityImplicitTLS = "implicit_tls" // 465: TLS at connect
)

// Register is the worker's first message on stream open (§3.1).
type Register struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	WorkerID  string `json:"box_id"`
	Token     string `json:"token"` // authorization AUTHORITY (verified vs send_managed.boxes.token_hash)
	// Optional diagnostics merged into box_metadata (§3.5); never read by dispatch.
	WorkerVersion string `json:"box_version,omitempty"`
	Hostname      string `json:"hostname,omitempty"`
	StartedAt     string `json:"started_at,omitempty"`
}

// RegisterAck is the Control's accept/reject (§3.1). On reject the Control closes the stream and the
// worker learns only the clean code (two-vocabulary rule); no job/credential is ever pushed.
type RegisterAck struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Accepted  bool   `json:"accepted"`
	Code      string `json:"code,omitempty"` // e.g. "unauthenticated" on reject
	// OutcomeBufferTTLSec delivers the seeded box_outcome_buffer_ttl_sec (Phase 7 §1.3, P7-4) to the
	// DB-less worker on accept: the worker retains/replays unacked outcomes up to this age, then drops them
	// and falls back to Control reclaim. Additive-only, omitempty so older workers/tests still parse; the
	// worker falls back to its bootstrap env default when it is 0/absent. Config-not-code: the worker holds
	// no seeded value of its own — it consumes what the Control reads from send_managed.config (the same
	// pattern as smtp_operation_timeout_sec on push_job).
	//
	// CHECKED, NOT CHANGED (Config Consumption Sweep closeout, 2026-07-27): an operator CAN reach
	// box_outcome_buffer_ttl_sec=0 without violating P7-11's ordering invariant (which only requires
	// >= startup_reconnect_window_sec, itself uncapped below), and the worker's own
	// `if (msg.outcome_buffer_ttl_sec > 0)` check (services/worker/src/stream.js) then falls back to its
	// bootstrap default rather than a genuine "buffer nothing" — the SAME shape of gap CSSSkipBelowPx
	// had. Deliberately left as-is here rather than fixed: unlike CSSSkipBelowPx (whose §6 range
	// 0-20 explicitly names 0 as a legitimate value), the locked Phase 7 spec (§5.3/P7-11) never
	// states that 0 means anything ("disable buffering entirely" is not a documented feature) — so
	// treating 0 as meaningful here would be INVENTING semantics for a locked spec, not fixing a
	// documented one, which CLAUDE.md Rule 2 reserves for an Open Decision. Flagged, not fixed.
	OutcomeBufferTTLSec int `json:"outcome_buffer_ttl_sec,omitempty"`
}

// Heartbeat refreshes last_heartbeat_at even when idle (§3.2).
type Heartbeat struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	WorkerID  string `json:"box_id"`
}

// Send is the rendered message content the worker transmits (§2.3, headers per §8).
type Send struct {
	Recipient   string            `json:"recipient"`
	FromAddress string            `json:"from_address"`
	FromName    string            `json:"from_name,omitempty"`
	ReplyTo     string            `json:"reply_to,omitempty"`
	Subject     string            `json:"subject"`
	Body        string            `json:"body"`
	Headers     map[string]string `json:"headers"` // the §8 minimum guaranteed set the Control built
}

// SMTP carries the connection + the decrypted plaintext credential for ONE send (§2.3). secret
// is memory-only on both sides, never logged/persisted, and zeroed after the session (§3.3/§4.4).
type SMTP struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
	Secret   string `json:"secret"`   // DECRYPTED plaintext SMTP password — scoped to this job
	Security string `json:"security"` // "starttls" | "implicit_tls"
	// OperationTimeoutSec delivers the seeded smtp_operation_timeout_sec (Phase 6 §5, P6-11) to the
	// DB-less worker: the per-operation SMTP timeout (connect/greeting/socket-read on MAIL/RCPT/DATA)
	// the worker must apply. Additive-only, omitempty so older frames/tests still parse; the worker falls
	// back to its bootstrap env default when it is 0/absent. Enforced Control-side to stay <
	// job_max_processing_time_sec (config-load reject) so an account hang surfaces as a worker-REPORTED
	// timeout, not an unscored Control lease reclaim (§2.4, P6-10).
	OperationTimeoutSec int `json:"operation_timeout_sec,omitempty"`
}

// PushJob delivers one job + its credential down the held stream (§2.3). One push = one send.
type PushJob struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	JobID     string `json:"job_id"`
	// AttemptToken is the PERSISTED per-attempt identity (Phase 8 Chunk 1, P7-OD-6). The Control mints it
	// by incrementing send_managed.jobs.attempt_token in the same guarded assigned→sending UPDATE that
	// dispatches this attempt, so it is monotonic, non-saturating, and unforgeable by the worker. The worker
	// echoes it VERBATIM in its report; the Control rejects any report whose token is not the job's
	// current one (a stale replay from a superseded attempt). Distinct from RequestID: request_id is a
	// transient per-frame correlation id the Control never stores, so it cannot survive a Control restart
	// or be compared against the DB. Additive-only, omitempty so pre-0020 frames/tests still parse; 0
	// or absent means "no token" and is never treated as a mismatch.
	AttemptToken   int64  `json:"attempt_token,omitempty"`
	Send           Send   `json:"send"`
	SMTP           SMTP   `json:"smtp"`
	LeaseExpiresAt string `json:"lease_expires_at"` // RFC3339; the worker's own deadline (§2.3)
	// Variation is the Control's resolved content-variation toggle snapshot (Phase 8 Chunk 2, P8-2
	// chosen option (a)): an additive, per-job-fresh wire field, never a schema change. Nil (omitted
	// from the frame) or a Variation whose fields are all at their Phase-3-neutral values (§4.2)
	// means "apply nothing" — the worker's assembler then produces byte-for-byte Phase 3 output. The
	// worker never reads send_managed.config; it applies exactly the resolved booleans/strings/arrays here.
	Variation *Variation `json:"variation,omitempty"`
}

// Variation is the worker-consumed, per-job-fresh snapshot of the Phase 8 content-variation config
// (§2.2 of the locked spec). Every field is a plain value — no logic, no config-table shape — so
// unmarshaling never requires the worker to understand send_managed.config at all. HeaderStyle=="none" and
// Precedence=="auto" are the Phase-3-neutral values (NOT Go zero values, so the Control always sets
// them explicitly); the boolean toggles rely on Go's zero value (false) coinciding with "off".
//
// Chunk 2 stamps this with a literal Phase-3-neutral snapshot (services/control/internal/dispatch/
// variation.go); Chunk 5 replaces that source with live send_managed.config reads once send_managed_0020 seeds
// the fourteen keys (config-not-code — deferred to the chunk that actually seeds them).
type Variation struct {
	HTMLMinify       bool   `json:"html_minify,omitempty"`
	CommentInjection bool   `json:"comment_injection,omitempty"`
	CommentIntensity string `json:"comment_intensity,omitempty"`
	// CommentIntensityRates is a Phase 9.5 S11 addition (§2.4): the per-bucket comment-injection
	// rate table (percent 0-100), replacing the worker's own hardcoded {low:5,medium:15,high:30}. Nil
	// (a pre-9.5 Control, or the field simply omitted) means "use the worker's own built-in defaults" —
	// the worker's resolver degrades to that exactly as it does for every other absent field (§4).
	CommentIntensityRates *CommentIntensityRates `json:"comment_intensity_rates,omitempty"`
	CSSMicro              bool                   `json:"css_micro,omitempty"`
	// CSSMagnitudePx/ZeroWidthRate/UserAgentRate deliberately have NO omitempty (unlike every other
	// field here): each is a numeric parameter whose valid range includes 0, so "0" and "field
	// absent" must stay distinguishable — an operator-set rate of 0 must reach the worker as a real 0,
	// never silently collapse to the worker's own default via the omitted-field fallback path (P8-OD-1).
	CSSMagnitudePx int `json:"css_magnitude_px"` // param of CSSMicro (amended, P8-OD-1 RULED-WIDEN)
	// CSSSkipBelowPx, like CSSMagnitudePx/ZeroWidthRate/UserAgentRate, deliberately has NO omitempty:
	// the px value below which css_micro leaves a padding/margin declaration untouched
	// (variation_css_micro_params.skip_below_px, replacing the worker's own hardcoded "<3px"). The
	// spec's own §6 range for this sub-key is 0-20 — an operator-set 0 ("vary every declaration, no
	// matter how small") is a legitimate, real value that must reach the worker as a real 0, never
	// silently collapse to the worker's own default via the omitted-field fallback path (same class of
	// bug P8-OD-1 fixed for the other three numeric params).
	CSSSkipBelowPx       int      `json:"css_skip_below_px"`
	UniqueURLParams      bool     `json:"unique_url_params,omitempty"`
	ZeroWidth            bool     `json:"zero_width,omitempty"`
	ZeroWidthRate        float64  `json:"zero_width_rate"` // param of ZeroWidth (amended, P8-OD-1 RULED-WIDEN)
	EncodingRotation     bool     `json:"encoding_rotation,omitempty"`
	MIMEBoundaryRotation bool     `json:"mime_boundary_rotation,omitempty"`
	DynamicPreheader     bool     `json:"dynamic_preheader,omitempty"`
	PreheaderTemplates   []string `json:"preheader_templates,omitempty"`
	HeaderStyle          string   `json:"header_style,omitempty"`
	UserAgentInjection   bool     `json:"user_agent_injection,omitempty"`
	UserAgentRate        float64  `json:"user_agent_rate"` // param of UserAgentInjection (amended, P8-OD-1 RULED-WIDEN)
	Precedence           string   `json:"precedence,omitempty"`
	ThreadIndex          bool     `json:"thread_index,omitempty"`
}

// CommentIntensityRates is the wire shape of variation_comment_intensity_rates (Phase 9.5 S11,
// §2.4): percent (0-100) of closing tags injected per variation_comment_intensity bucket.
type CommentIntensityRates struct {
	Low    int `json:"low"`
	Medium int `json:"medium"`
	High   int `json:"high"`
}

// Report is the worker's outcome for one job (§2.4). provider_message_id present iff accepted.
// smtp_response is the raw final SMTP line (never the credential). error_reason is an internal
// slug on failure. NONE of this flows verbatim to a platform caller.
type Report struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	JobID     string `json:"job_id"`
	// AttemptToken echoes push_job.attempt_token VERBATIM (Phase 8 Chunk 1, P7-OD-6) — the identity of
	// the attempt that produced this outcome. The Control compares it to send_managed.jobs.attempt_token and
	// applies the report only when they match; a mismatch is a stale replay from a superseded attempt
	// and mutates nothing. Additive-only, omitempty; 0/absent = a pre-0020 worker, which degrades to
	// Phase 7 behavior (the different-worker attribution guard) rather than being rejected.
	AttemptToken      int64  `json:"attempt_token,omitempty"`
	Outcome           string `json:"outcome"` // accepted | error | bounced
	ProviderMessageID string `json:"provider_message_id,omitempty"`
	SMTPResponse      string `json:"smtp_response,omitempty"`
	ErrorReason       string `json:"error_reason,omitempty"`
}

// ReportAck is the Control's POST-PERSIST acknowledgement of a report (Phase 7 §1.3, P7-4). The Control
// sends it ONLY AFTER the report's outcome is durably committed to the DB (or determined to be a
// clean idempotent no-op — already-persisted / not-in-flight, which is equally "the Control owns this
// job's resolution, the worker may drop it"). Until a worker receives report_ack for a job, it RETAINS the
// outcome and replays it on (re)connection (bounded by box_outcome_buffer_ttl_sec). Carrying job_id
// (not just request_id) lets the worker match the ack even across a reconnect where request_ids differ.
// Two-vocabulary: the ack carries only {job_id, request_id, accepted} — never internal reasons.
type ReportAck struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	JobID     string `json:"job_id"`
	Accepted  bool   `json:"accepted"` // true = durably persisted (or clean no-op) → worker may drop the buffered outcome
}

// error_reason slugs (§2.4). Internal only; mapped to error_class='unknown' in Phase 3 (§5.4).
const (
	ReasonConnectRefused    = "connect_refused"
	ReasonAuthFailed        = "auth_failed"
	ReasonStartTLSUnavail   = "starttls_unavailable"
	ReasonDataRejected4xx   = "data_rejected_4xx"
	ReasonDataRejected5xx   = "data_rejected_5xx"
	ReasonLeaseExpired      = "lease_expired"
	ReasonTimeout           = "timeout"
	ReasonAcceptedWithoutID = "accepted_without_id" // §4.3 defensive backstop (should be impossible)
)
