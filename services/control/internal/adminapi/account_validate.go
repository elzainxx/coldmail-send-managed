package adminapi

import (
	"net"
	"regexp"
	"strings"
)

// Account field validation (§3) mirrors the Phase 1 CHECK constraints so the API rejects with a
// clean envelope BEFORE the DB (the CHECK remains the backstop). Each returns a fieldDetail
// (nil if valid) so a handler can collect all field issues into one validation_failed envelope.

// fromAddressRe matches the Phase 1 accounts_from_address_valid_check regex exactly.
var fromAddressRe = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)

// hostnameRe: a DNS hostname label sequence — letters/digits/dots/hyphens only, no colon (so a
// ":port" suffix is rejected), no scheme, no path (§3). IP literals are validated separately via
// net.ParseIP so IPv6 (which legitimately contains colons) is still accepted as a bare literal.
// This is a SYNTACTIC check only — a live SMTP connectivity probe is DEFER-TO-PHASE-3 (OD-8).
var hostnameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.\-]*[A-Za-z0-9])?$`)

func validateSMTPHost(host string) *fieldDetail {
	if host == "" || len(host) > 255 {
		return &fieldDetail{Field: "smtp_host", Issue: issueMalformed}
	}
	// Accept a bare IP literal (v4 or v6) OR a DNS hostname. Reject anything with a scheme,
	// path, or a ":port" suffix on a hostname (§3: "no scheme/port/path"). An IPv6 literal is a
	// bare address here (no brackets, no port) — net.ParseIP accepts it; a "host:port" string
	// parses as neither a valid IP nor a hostname, so it is rejected.
	if net.ParseIP(host) != nil {
		return nil
	}
	if strings.ContainsAny(host, ":/\\ @") || !hostnameRe.MatchString(host) {
		return &fieldDetail{Field: "smtp_host", Issue: issueMalformed}
	}
	return nil
}

func validateSMTPPort(port int) *fieldDetail {
	if port < 1 || port > 65535 {
		return &fieldDetail{Field: "smtp_port", Issue: issueOutOfRange}
	}
	return nil
}

func validateSMTPUsername(u string) *fieldDetail {
	if u == "" || len(u) > 320 {
		return &fieldDetail{Field: "smtp_username", Issue: issueMalformed}
	}
	return nil
}

func validateFromAddress(a string) *fieldDetail {
	if a == "" || len(a) > 320 || !fromAddressRe.MatchString(a) {
		return &fieldDetail{Field: "from_address", Issue: issueMalformed}
	}
	return nil
}

func validateFromName(n *string) *fieldDetail {
	if n != nil && len(*n) > 255 {
		return &fieldDetail{Field: "from_name", Issue: issueMalformed}
	}
	return nil
}

// DailyLimitCeiling is a HARDCODED SANITY BOUND on daily_limit, NOT a tuning dial (OD-24, ruling
// option (c) — same category as the fallback-chain maxDepth guard). It is a fat-finger guard: it
// rejects an absurd value (an operator typing an extra digit or two — e.g. 5_000_000 instead of
// 500) while comfortably admitting every realistic per-mailbox provider limit (Google Workspace
// ~2,000/day; Amazon SES account quotas are far higher but a single SMTP *mailbox* never
// legitimately sends anywhere near 100,000/day).
//
// Why a bound and not a config key: seeding a `daily_limit_ceiling` row would edit the locked
// Phase 9 §4 50-key reference (and the config_drift gate) for a dial nobody would ever turn. The
// §3/§6.4 "configurable ceiling" wording is satisfied by a bound rather than a dial. Why safe to
// hardcode: a too-high daily_limit degrades GRACEFULLY at send time — a provider throttle maps to
// the throttle error class → pause, and a provider suspension maps to auth_failure → quarantine,
// both already handled downstream — so this is a REPUTATION guard, not a correctness guard.
// Promoting it to a real seeded config key later is a trivial additive change (new key + wire
// this validator to it). See Docs/history/open-decisions.md OD-24 and ADR-0009's sibling reasoning.
const DailyLimitCeiling = 100_000

// validateDailyLimit checks 0 < daily_limit <= DailyLimitCeiling. The ≥ daily_sent_count rule on
// PATCH is handled separately as a 409 (§3.1); the Phase 1 accounts_daily_limit_check (> 0) is
// the DB backstop for the lower bound.
func validateDailyLimit(limit int) *fieldDetail {
	if limit <= 0 || limit > DailyLimitCeiling {
		return &fieldDetail{Field: "daily_limit", Issue: issueOutOfRange}
	}
	return nil
}

// collect drops nil details and returns the non-nil ones.
func collect(ds ...*fieldDetail) []fieldDetail {
	var out []fieldDetail
	for _, d := range ds {
		if d != nil {
			out = append(out, *d)
		}
	}
	return out
}
