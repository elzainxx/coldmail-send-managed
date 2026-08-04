// Package configordering holds the two documented config-ordering invariants (CLAUDE.md Rule 1;
// Phase 7 §5.3 authoritative) as pure, dependency-free functions — no DB import, no HTTP/error-
// envelope knowledge. This is the SINGLE source of truth both enforcement points call, so the
// admin PUT boundary (internal/adminapi) and the config-LOAD path (internal/dispatch) can never
// drift onto two different definitions of the same invariant (P9-OD-2, RULED-AND-CLOSED).
package configordering

import "fmt"

// Violation describes one invariant breach: the key being set/loaded, its candidate value, the
// counterpart key's name and CURRENT value, and the constraint verb ("must be <" / "must be >=").
// Callers render this however their own error-envelope convention requires.
type Violation struct {
	Key              string
	Value            int
	Constraint       string // "must be <" or "must be >="
	CounterpartKey   string
	CounterpartValue int
}

// Detail renders the violation WITHOUT the "config_ordering_violation: " prefix — the exact
// format the config-LOAD path has always used internally (preserved byte-for-byte so existing
// tests asserting that string keep passing unchanged).
func (v *Violation) Detail() string {
	return fmt.Sprintf("%s (%d) %s %s (%d)", v.Key, v.Value, v.Constraint, v.CounterpartKey, v.CounterpartValue)
}

func (v *Violation) Error() string { return "config_ordering_violation: " + v.Detail() }

// CheckSMTPTimeoutVsLease enforces invariant (a), P6-11: smtp_operation_timeout_sec must be
// STRICTLY less than job_max_processing_time_sec, so an account hang trips the worker's own
// per-operation timeout (a scored, worker-reported failure) before the job lease expires (an
// unscored Control reclaim that would thrash). Equality is also a violation.
func CheckSMTPTimeoutVsLease(smtpTimeoutSec, leaseSec int) *Violation {
	if smtpTimeoutSec >= leaseSec {
		return &Violation{
			Key: "smtp_operation_timeout_sec", Value: smtpTimeoutSec, Constraint: "must be <",
			CounterpartKey: "job_max_processing_time_sec", CounterpartValue: leaseSec,
		}
	}
	return nil
}

// CheckBufferTTLVsReconnectWindow enforces invariant (b), P7-11: box_outcome_buffer_ttl_sec must
// be greater than or equal to startup_reconnect_window_sec, so a worker reconnecting late-but-in-
// window still finds its buffered outcome (else the outcome already expired -> a silent
// duplicate on reclaim-and-resend). Bare equality is the floor, not itself a violation.
func CheckBufferTTLVsReconnectWindow(bufferTTLSec, reconnectWindowSec int) *Violation {
	if bufferTTLSec < reconnectWindowSec {
		return &Violation{
			Key: "box_outcome_buffer_ttl_sec", Value: bufferTTLSec, Constraint: "must be >=",
			CounterpartKey: "startup_reconnect_window_sec", CounterpartValue: reconnectWindowSec,
		}
	}
	return nil
}
