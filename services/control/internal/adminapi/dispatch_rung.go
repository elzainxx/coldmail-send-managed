package adminapi

// dispatch_rung.go — fix-plan item 12, the admin-surface half. The rung ladder as the §3.2 health
// snapshot needs it.
//
// WHY A THIRD COPY EXISTS. internal/adminapi does not import internal/dispatch (the same structural
// isolation that gives this package its own readConfigInt/readConfigBool/readConfigString rather than
// calling dispatch.LoadConfig). internal/platformapi has a copy for the same reason. Three copies of a
// ladder is a drift hazard, so it is held by a MECHANISM, not by discipline:
// TestDispatchRung_EveryCopyOfTheLadderMatchesThisOne (in internal/dispatch) reads the source of every
// copy and fails if the names or the ordering differ — and it also fails if a FOURTH copy appears that
// the test does not know about, because "the check covers less than the property" is the exact failure
// this alignment pass keeps finding.

// The rung names and ordering, held identical to internal/dispatch/rung.go by the test named above.
const (
	rungServing        = "serving"
	rungAcceptAndDefer = "accept_and_defer"
	rungRefuseNew      = "refuse_new"
	rungStopped        = "stopped"
)

var rungSeverity = map[string]int{
	rungServing:        0,
	rungAcceptAndDefer: 1,
	rungRefuseNew:      2,
	rungStopped:        3,
}

// resolveRung composes dispatch_mode with the legacy dispatch_enabled boolean exactly as
// dispatch.resolveRung does — MAXIMUM severity, with dispatch_enabled=false a floor of
// accept_and_defer. It exists so the health snapshot reports the rung the Control is actually at,
// rather than the raw value of one of the two dials.
//
// An unrecognized mode resolves to accept_and_defer, matching the Control's own fail-toward-safer
// disposition (§1.11.4). The admin surface reports what the Control WILL DO, never what the row says:
// echoing a garbage mode string back to an operator as though it were in effect is the same
// looks-normal-while-ignored failure (M1's D2) that this whole item exists to remove.
func resolveRung(mode string, dispatchEnabled bool) string {
	if _, ok := rungSeverity[mode]; !ok {
		mode = rungAcceptAndDefer
	}
	if !dispatchEnabled && rungSeverity[mode] < rungSeverity[rungAcceptAndDefer] {
		return rungAcceptAndDefer
	}
	return mode
}
