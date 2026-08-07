package main

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// =============================================================================
// N1 / D12 — the migration runner's DSN scrub, and the DRIFT CONTROL for its copy
// =============================================================================
//
// This tool is a separate Go module whose Docker build context copies only tools/send-managed-migrate/,
// so it carries a COPY of services/control/internal/dsnscrub rather than importing it (see
// dsnscrub.go's header). A copy that nobody checks is a copy that drifts: the next person fixes a
// hole in one file, ships, and this tool keeps leaking. A comment asking them to edit both is not a
// control, so TestDSNScrub_MigrateCopyHasNotDriftedFromControl compares the two files' LOGIC on
// every run and fails when they diverge.
//
// NO NETWORK. Every DSN below either fails at the PARSE stage (before any socket) or points at
// 127.0.0.1:1, which is refused without a DNS lookup. Nothing here skips — the harness counts a skip
// as an unexecuted test and turns the package red, correctly.

const canary = "CANARYSECRET1234"

// canonicalScrubPath is the file this one is a copy of. Relative to the package dir, which is
// `go test`'s working directory.
const canonicalScrubPath = "../../services/control/internal/dsnscrub/dsnscrub.go"

// leakyDSNs are the shapes MEASURED to leak at pgx v5.7.4 through the parse stage, plus the two pgx
// redacts itself (kept so a future pgx regression on those is caught with no edit here).
func leakyDSNs() map[string]string {
	return map[string]string{
		"url query password":    "postgres://u@127.0.0.1:5432/db?password=" + canary + "&sslmode=bogusmode",
		"url query sslpassword": "postgres://u@127.0.0.1:5432/db?sslpassword=" + canary + "&sslmode=bogusmode",
		"kv spaced password":    "host=127.0.0.1 password = " + canary + " dbname=db sslmode=bogusmode",
		"url query bad port":    "postgres://u@127.0.0.1:99999999/db?password=" + canary,
		"kv unspaced password":  "host=127.0.0.1 password=" + canary + " dbname=db sslmode=bogusmode",
		"url userinfo":          "postgres://u:" + canary + "@127.0.0.1:5432/db?sslmode=bogusmode",
	}
}

// pgxRedactsThese are the two shapes pgx v5.7.4 handles on its own, skipped by the anti-vacuity test
// below (asserting "pgx still leaks this" for a shape pgx redacts would fail for the right reason and
// muddy the signal).
var pgxRedactsThese = map[string]bool{"kv unspaced password": true, "url userinfo": true}

// TestDSNScrub_MigrateNoCredentialSurvives asserts the property through pgx.Connect — the exact call
// Runner.connect makes at runner.go:47, whose error reaches stderr via main.go's fatal(err.Error()).
func TestDSNScrub_MigrateNoCredentialSurvives(t *testing.T) {
	for name, dsn := range leakyDSNs() {
		t.Run(name, func(t *testing.T) {
			_, err := pgx.Connect(context.Background(), dsn)
			if err == nil {
				t.Fatal("expected a parse error — without one this case proves nothing")
			}
			got := scrubDSNError(err, dsn)
			if strings.Contains(got.Error(), canary) {
				t.Fatalf("N1: the credential LEAKED into the error text that main.go prints to stderr:\n%s", got.Error())
			}
			if strings.Contains(got.Error(), dsn) {
				t.Fatalf("the whole DSN survived in the error text:\n%s", got.Error())
			}
		})
	}
}

// TestDSNScrub_MigratePgxStillLeaksAtThisPin is the ANTI-VACUITY control. It asserts the RAW driver
// error still leaks, so the test above is defending something. The trigger is part of the finding:
// M1's `pool_max_conns=notanumber` produces a message with NO DSN in it at this pin, so porting M1's
// test would have passed every case without a credential ever reaching an assertion.
func TestDSNScrub_MigratePgxStillLeaksAtThisPin(t *testing.T) {
	for name, dsn := range leakyDSNs() {
		if pgxRedactsThese[name] {
			continue
		}
		t.Run(name, func(t *testing.T) {
			_, err := pgx.Connect(context.Background(), dsn)
			if err == nil {
				t.Fatalf("PREMISE CHANGED for %q: the trigger no longer produces an error at this pin.\n"+
					"Re-measure which trigger carries the DSN — a trigger that does not fire makes every\n"+
					"other test in this file vacuous.", name)
			}
			if !strings.Contains(err.Error(), canary) {
				t.Fatalf("PREMISE CHANGED for %q: the pinned pgx no longer leaks the credential itself.\n"+
					"raw error: %s\nGood news, not a bug — but re-read pgconn/errors.go and decide whether\n"+
					"scrubDSNError is still load-bearing before deleting anything.", name, err.Error())
			}
		})
	}
}

// TestDSNScrub_MigrateCleanErrorsKeepTheirDetail is the other half of the anti-vacuity pairing: a
// scrub that fired on everything would pass every leak test above while destroying every diagnosis.
// The likeliest real failure for a migration run is an unreachable database, and that message — which
// carries no credential — is what an operator needs to see.
func TestDSNScrub_MigrateCleanErrorsKeepTheirDetail(t *testing.T) {
	// Port 1 on loopback: refused deterministically, with no DNS lookup and no wildcard-resolver
	// dependency, so this case cannot degrade into a skip.
	dsn := "postgres://u:" + canary + "@127.0.0.1:1/db?connect_timeout=1"
	_, err := pgx.Connect(context.Background(), dsn)
	if err == nil {
		t.Fatal("expected a connect failure against 127.0.0.1:1 — something is listening on port 1")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("PREMISE CHANGED: the connect-stage error now carries the credential, so this is no\n"+
			"longer a clean error. Re-measure and move it to leakyDSNs(). raw: %s", err.Error())
	}
	if got := scrubDSNError(err, dsn); got.Error() != err.Error() {
		t.Fatalf("a clean error was scrubbed, costing the operator the whole diagnosis:\n"+
			"  raw:      %s\n  scrubbed: %s", err.Error(), got.Error())
	}
}

// TestDSNScrub_MigrateConnectionRefusedSurvives pins the over-suppression counterweight at THIS
// tool's most common real error. The first version of the candidate extraction collected every bare
// token, so `port=5432` made "5432" a candidate and connection-refused — the single most common
// database error — was replaced by the suppression notice. The full matrix of structural values lives
// with the canonical package; the drift control above is what carries it over to this copy.
func TestDSNScrub_MigrateConnectionRefusedSurvives(t *testing.T) {
	dsn := "host=db.internal port=5432 dbname=sendmanaged user=migrator"
	msg := "dial tcp 10.0.0.5:5432: connect: connection refused"
	if got := scrubDSNError(errors.New(msg), dsn); got.Error() != msg {
		t.Fatalf("a message with NO credential in it was suppressed, which would make every database\n"+
			"outage look like a DSN syntax error:\n  raw:      %s\n  scrubbed: %s", msg, got.Error())
	}
}

// TestDSNScrub_MigrateMessageNamesThisToolsEnvVars exists because the drift comparison below
// NORMALISES the suppression message away — the two copies deliberately name different env vars. That
// makes the message the one part of the file drift cannot police, so it is asserted here directly:
// the replacement text must name the variables THIS tool actually reads (main.go:41), or the operator
// is told to check something that does not apply.
func TestDSNScrub_MigrateMessageNamesThisToolsEnvVars(t *testing.T) {
	got := scrubDSNError(errors.New("cannot parse `"+"postgres://u:ab@h/db"+"`: nope"), "postgres://u:ab@h/db")
	for _, want := range []string{"MIGRATE_DATABASE_URL", "DATABASE_URL"} {
		if !strings.Contains(got.Error(), want) {
			t.Fatalf("the suppression message does not name %s, which main.go:41 reads — an operator is\n"+
				"left with no idea which variable to look at. got: %s", want, got.Error())
		}
	}
}

// TestDSNScrub_MigrateNilIsNil — scrubDSNError composes at a return site with no guard.
func TestDSNScrub_MigrateNilIsNil(t *testing.T) {
	if scrubDSNError(nil, "postgres://u:"+canary+"@h/db") != nil {
		t.Fatal("scrubDSNError(nil) must be nil")
	}
}

// -----------------------------------------------------------------------------
// THE DRIFT CONTROL
// -----------------------------------------------------------------------------

// renames maps this copy's identifiers to the canonical package's. Every entry is a DELIBERATE
// difference: the copy lives in package main alongside runner.go, so it cannot use unqualified names
// like `Err` that only read correctly as dsnscrub.Err.
//
// This table is itself load-bearing: adding an entry is how you declare "this difference is
// intended". Anything NOT in here that differs is drift, and fails.
var renames = map[string]string{
	"scrubDSNError":           "Err",
	"scrubbedDSNError":        "scrubbedError",
	"dsnCredentialCandidates": "credentialCandidates",
	"structuralDSNKeys":       "structuralKeys",
	"knownDSNKey":             "knownKey",
}

// suppressionMsgRe matches the replacement-text literal, which differs by design (each tool names its
// own env vars). It is normalised away here and asserted separately by
// TestDSNScrub_MigrateMessageNamesThisToolsEnvVars.
var suppressionMsgRe = regexp.MustCompile(`"connection string rejected by the driver[^"]*"`)

// TestDSNScrub_MigrateCopyHasNotDriftedFromControl is the mechanism that replaces "remember to edit
// both files". It strips comments and blank lines, applies the declared renames, normalises the
// per-tool message, and compares what is left LINE BY LINE. Any behavioural edit to either file that
// is not mirrored in the other fails here, naming the first differing line.
//
// It does NOT skip when the canonical file is missing. A drift control that quietly passes because it
// could not find its subject is the exact false-green this project has been digging out of: a gate
// whose subject vanished, compared against nothing, and reported success.
func TestDSNScrub_MigrateCopyHasNotDriftedFromControl(t *testing.T) {
	canonicalRaw, err := os.ReadFile(canonicalScrubPath)
	if err != nil {
		t.Fatalf("cannot read the canonical scrub at %s: %v\n"+
			"This test is the ONLY thing keeping this tool's copy from drifting, so a missing subject is a\n"+
			"FAILURE, not a skip. If the canonical package moved, update canonicalScrubPath.", canonicalScrubPath, err)
	}
	copyRaw, err := os.ReadFile("dsnscrub.go")
	if err != nil {
		t.Fatalf("cannot read this package's dsnscrub.go: %v", err)
	}

	canonical := normalizeScrub(string(canonicalRaw), nil)
	local := normalizeScrub(string(copyRaw), renames)

	if len(canonical) == 0 {
		t.Fatal("normalising the canonical file produced NOTHING — normalizeScrub is broken, and an " +
			"empty-vs-empty comparison would pass while proving nothing")
	}

	for i := 0; i < len(canonical) || i < len(local); i++ {
		var c, l string
		if i < len(canonical) {
			c = canonical[i]
		}
		if i < len(local) {
			l = local[i]
		}
		if c != l {
			t.Fatalf("DRIFT: this tool's copy of the DSN scrub no longer matches the canonical one.\n"+
				"  canonical: %s (line %d of %d significant lines)\n"+
				"    %q\n"+
				"  this copy: tools/send-managed-migrate/dsnscrub.go (line %d of %d)\n"+
				"    %q\n\n"+
				"Both files must carry the same logic — this tool ships in its own Docker image and cannot\n"+
				"import the canonical package. Mirror the change, or, if the difference is intentional,\n"+
				"add it to the `renames` table so it is DECLARED rather than discovered.",
				canonicalScrubPath, i+1, len(canonical), c, i+1, len(local), l)
		}
	}
}

// normalizeScrub reduces a source file to its significant lines: no comments, no blank lines, no
// package clause, indentation collapsed, declared renames applied, the per-tool suppression message
// replaced by a placeholder.
func normalizeScrub(src string, rename map[string]string) []string {
	if rename != nil {
		for from, to := range rename {
			src = regexp.MustCompile(`\b`+regexp.QuoteMeta(from)+`\b`).ReplaceAllString(src, to)
		}
	}
	src = suppressionMsgRe.ReplaceAllString(src, `"SUPPRESSION_MESSAGE"`)
	var out []string
	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimSpace(strings.ReplaceAll(raw, "\r", ""))
		if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "package ") {
			continue
		}
		// Collapse runs of internal whitespace so a gofmt alignment change in one file (the map
		// literal aligns differently once a longer key is added) is not reported as drift.
		out = append(out, strings.Join(strings.Fields(line), " "))
	}
	return out
}
