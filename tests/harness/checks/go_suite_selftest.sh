#!/usr/bin/env bash
# go_suite_selftest — BLOCKING paired control for run.sh's go_suite verdict logic.
#
# THE DEFECT (found 2026-11-08, demonstrated on a throwaway package; no real masked failure was found in the green runs):
# go_suite judged a package with `echo "$out" | grep -qE "^--- FAIL|^FAIL\b"` under `set -o pipefail`. With -q the reader
# exits at the FIRST match; once $out is bigger than a pipe buffer (64KB) `echo` is still writing, dies of a broken pipe,
# and the pipeline reports "no match" — so a package with a real `--- FAIL` early in verbose output, followed by enough
# later output, was recorded as PASSING. Real `go test -v` output for the larger packages here is well within reach of that.
# It is a FALSE GREEN, the direction that costs: nothing else in the harness would notice. Separately, the verdict filter
# kept only `--- PASS|FAIL` lines, so a failing test's REASON never reached the log (a red run on a real GitHub runner said
# only "--- FAIL: TestEvaluate_T5" and could not be explained).
#
# WHAT IT DOES: extracts the REAL count_ran() and go_suite() from run.sh (not a copy — a copy would be a test of a file
# nobody runs), runs them under the same `set -euo pipefail` against two throwaway Go packages, and asserts on the verdicts:
#   (a) a package whose FIRST test fails and whose LATER test prints ~260KB  -> must be recorded FAIL, with the assertion
#       text printed. Restoring the `echo | grep -q` form makes this case fail (verified).
#   (b) a package whose tests all pass                                         -> must be recorded PASS (the accept control:
#       a go_suite broken into rejecting everything would otherwise pass (a)).
# It also fails if the functions cannot be extracted, so renaming them cannot turn this into a vacuous pass.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUN="$HERE/../run.sh"
command -v go >/dev/null 2>&1 || { echo "go_suite_selftest FAIL: go is not on PATH"; exit 1; }

src_count="$(awk '/^count_ran\(\) \{/,/^}/' "$RUN")"
src_suite="$(awk '/^go_suite\(\) \{/,/^}/' "$RUN")"
if [[ "$src_count" != *"count_ran()"* || "$src_suite" != *"go_suite()"* ]]; then
  echo "go_suite_selftest FAIL: could not extract count_ran()/go_suite() from $RUN — they were renamed or moved, and this control would be testing nothing"
  exit 1
fi
eval "$src_count"
eval "$src_suite"

TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT
export GO_SUITE_TIMEOUT=120s GOTOOLCHAIN=local GOFLAGS=-mod=mod GOCACHE="${GOCACHE:-$TMP/gocache}"
ok()  { echo "  PASS: $1"; }     # the real run.sh versions only count; the VERDICT TEXT is what is asserted on
bad() { echo "  FAIL: $1"; }

mkdir -p "$TMP/failing" "$TMP/passing"
printf 'module selftest/failing\ngo 1.26\n' >"$TMP/failing/go.mod"
cat >"$TMP/failing/a_test.go" <<'GO'
package failing

import (
	"fmt"
	"strings"
	"testing"
)

func TestA_FailsEarly(t *testing.T) { t.Errorf("boom: wanted %d, got %d", 1, 2) }

// Runs AFTER the failure and prints far more than a pipe buffer.
func TestB_BigOutputAfterIt(t *testing.T) {
	fmt.Println(strings.Repeat("filler line to outgrow the 64KB pipe buffer\n", 6000))
}
GO
printf 'module selftest/passing\ngo 1.26\n' >"$TMP/passing/go.mod"
cat >"$TMP/passing/a_test.go" <<'GO'
package passing

import "testing"

func TestOne(t *testing.T) {}
func TestTwo(t *testing.T) {}
GO

fail=0
check() { # <name> <condition-result: 0=ok> <detail>
  if [[ "$2" -eq 0 ]]; then echo "  ok: $1"; else echo "go_suite_selftest FAIL [$1]: $3"; fail=1; fi
}

# NOT piped into anything: the verdict text is captured whole, then asserted on with here-strings.
out_fail="$(go_suite "$TMP/failing" "selftest-failing-package" 2>&1 || true)"
out_pass="$(go_suite "$TMP/passing" "selftest-passing-package" 2>&1 || true)"

# size of the fixture's raw output, printed so a reader can see the case really is bigger than a pipe buffer
# (`|| true` inside the braces: the fixture is DELIBERATELY failing, so go test exits 1, and under pipefail that would fail
# this command substitution and let `set -e` kill the script before a single assertion ran.)
size="$( { cd "$TMP/failing" && go test -count=1 -v . 2>&1 || true; } | wc -c )"
check "the failing fixture's verbose output exceeds a 64KB pipe buffer (${size} bytes)" "$([[ "$size" -gt 65536 ]] && echo 0 || echo 1)" "fixture too small to exercise the hazard"

check "a package with an early FAIL and a LATER 260KB of output is recorded FAIL" \
  "$(grep -qF '  FAIL: selftest-failing-package' <<<"$out_fail" && echo 0 || echo 1)" \
  "go_suite did NOT record the failure — a failing package was reported as passing (false green). Output was: $out_fail"
check "...and is NOT also recorded PASS" \
  "$(grep -qF '  PASS: selftest-failing-package' <<<"$out_fail" && echo 1 || echo 0)" "the failing package was ALSO recorded PASS"
check "...and the assertion text is printed (the reason, not just the verdict)" \
  "$(grep -qF 'boom: wanted 1, got 2' <<<"$out_fail" && echo 0 || echo 1)" "the failure detail did not reach the output. Output was: $out_fail"
check "a package whose tests all pass is recorded PASS (accept control)" \
  "$(grep -qF '  PASS: selftest-passing-package' <<<"$out_pass" && ! grep -qF '  FAIL: selftest-passing-package' <<<"$out_pass" && echo 0 || echo 1)" \
  "a passing package was not recorded PASS. Output was: $out_pass"

if [[ "$fail" -eq 0 ]]; then
  echo "go_suite_selftest OK: 5 assertions — an early FAIL followed by >64KB of output is still recorded FAIL with its reason; a passing package is still PASS"
fi
exit "$fail"
