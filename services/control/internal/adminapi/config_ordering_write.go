package adminapi

// config_ordering_write.go — the admin PUT-boundary half of the two config-ordering invariants
// (CLAUDE.md Rule 1; Phase 7 §5.3; Phase 9 §4.6/E4). P9-OD-2 (RULED-AND-CLOSED, Docs/
// open-decisions.md): before this file, only the config-LOAD path (internal/dispatch/config.go,
// read on the Control's own tick cadence) enforced these invariants — a PUT of a value that would
// invert either pair returned a clean 200, the bad value landed in send_managed.config, and the
// violation surfaced only later, as a LATENT tick error far from the change that caused it (the
// exact "false promise of control" defect class the 9.5 consumption sweep already closed twice).
// This closes the gap at the point of the mistake: reject the WRITE, don't let it land.
//
// Both invariant CHECKS are the SAME pure functions internal/dispatch/config.go calls
// (internal/configordering) — reused, not reimplemented, so load-time and write-time can never
// silently diverge on what "valid" means for these two pairs.

import (
	"context"
	"fmt"

	"github.com/coldmail-send-managed/control/internal/configordering"
)

// checkConfigOrderingOnWrite validates the RESULTING pair for a PUT to key with candidate value
// newValue — not the submitted value in isolation. Reads the counterpart key's CURRENT stored
// value fresh (never cached) so a write is validated against real, live state. Returns nil for
// any key with no ordering counterpart (every other config key). On violation, returns a clean
// apiError naming the conflicting counterpart key, its current value, and the required relation —
// so an operator is never left guessing which key to change or by how much (two-vocabulary rule:
// this detail is safe operational information, not an internal reason/credential/SQL detail).
func checkConfigOrderingOnWrite(ctx context.Context, db pgxDB, key string, newValue int) *apiError {
	// counterpartVal/explain are keyed on `key` (the one being WRITTEN) — used for the error
	// message, which must name the OTHER key regardless of which side of the invariant `key`
	// happens to be. configordering.Violation's own Key/CounterpartKey fields are fixed to the
	// invariant's semantic structure (dependent value vs boundary value), NOT to which key this
	// particular write touched, so the message is built from these locals, never from v directly.
	var (
		v              *configordering.Violation
		counterpartKey string
		counterpartVal int
		explain        string
	)
	switch key {
	case "smtp_operation_timeout_sec":
		counterpartKey = "job_max_processing_time_sec"
		lease, err := readConfigIntDirect(ctx, db, counterpartKey)
		if err != nil {
			return counterpartReadErr(counterpartKey, err)
		}
		counterpartVal = lease
		v = configordering.CheckSMTPTimeoutVsLease(newValue, lease)
		explain = "smtp_operation_timeout_sec must stay strictly below job_max_processing_time_sec"
	case "job_max_processing_time_sec":
		counterpartKey = "smtp_operation_timeout_sec"
		timeout, err := readConfigIntDirect(ctx, db, counterpartKey)
		if err != nil {
			return counterpartReadErr(counterpartKey, err)
		}
		counterpartVal = timeout
		v = configordering.CheckSMTPTimeoutVsLease(timeout, newValue)
		explain = "job_max_processing_time_sec must stay strictly above smtp_operation_timeout_sec"
	case "box_outcome_buffer_ttl_sec":
		counterpartKey = "startup_reconnect_window_sec"
		window, err := readConfigIntDirect(ctx, db, counterpartKey)
		if err != nil {
			return counterpartReadErr(counterpartKey, err)
		}
		counterpartVal = window
		v = configordering.CheckBufferTTLVsReconnectWindow(newValue, window)
		explain = "box_outcome_buffer_ttl_sec must stay at or above startup_reconnect_window_sec"
	case "startup_reconnect_window_sec":
		counterpartKey = "box_outcome_buffer_ttl_sec"
		buffer, err := readConfigIntDirect(ctx, db, counterpartKey)
		if err != nil {
			return counterpartReadErr(counterpartKey, err)
		}
		counterpartVal = buffer
		v = configordering.CheckBufferTTLVsReconnectWindow(buffer, newValue)
		explain = "startup_reconnect_window_sec must stay at or below box_outcome_buffer_ttl_sec"
	default:
		return nil // no ordering counterpart for this key
	}
	if v == nil {
		return nil
	}
	return newErr(codeConfigOrderingViolation, "config_ordering_violation: "+v.Detail()).
		withMessage(fmt.Sprintf("This value conflicts with %s (currently %d): %s.",
			counterpartKey, counterpartVal, explain)).
		withDetails(fieldDetail{Field: "value", Issue: issueConflict})
}

// counterpartReadErr wraps a failed read of a key's ordering counterpart. Both members of both
// pairs are seeded since send_managed_0010/0018/0019, so a read failure here is an internal fault, not
// a validation question — fail closed (reject the write) rather than silently skip the check.
func counterpartReadErr(counterpartKey string, err error) *apiError {
	return newErr(codeInternalError, "config_ordering_counterpart_read_failed key="+counterpartKey+": "+err.Error())
}

// readConfigIntDirect reads one integer-typed send_managed.config key's CURRENT value, unwrapping the
// {"v": N} envelope. Distinct from getConfig's caller-facing configRow because this needs only
// the raw int, and must read fresh inside the SAME TRANSACTION as the write it is guarding — never
// a cached value, and never on a separate connection (see handleConfigPut on why the transaction,
// not merely the freshness, is what makes the check sound).
func readConfigIntDirect(ctx context.Context, db pgxDB, key string) (int, error) {
	row, err := getConfig(ctx, db, key)
	if err != nil {
		return 0, err
	}
	n, ok := asInt(row.Value)
	if !ok {
		return 0, fmt.Errorf("config %q is not an integer", key)
	}
	return int(n), nil
}
