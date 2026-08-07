#!/usr/bin/env bash
# dsn_scrub_wiring — BLOCKING CI gate. Every place shipped code hands a DSN to the driver must scrub
# the resulting error, and this gate is what fails when one doesn't.
#
# WHY IT EXISTS, AND IT IS NOT A HYPOTHETICAL. N1/D12 was fixed by adding internal/dsnscrub plus a
# nine-test package that proves the scrub works. Break-verification then showed what that package
# CANNOT prove. With `dsnscrub.Err(err, dsn)` reverted to plain `err` at internal/db's call sites:
#
#     services/control/internal/dsnscrub  ->  ok      (0.010s)     <-- still fully GREEN
#     go build ./...                      ->  clean
#     go vet ./internal/db/               ->  clean
#
# Nothing in the toolchain objects to a leaking call site. The unit package tests the FUNCTION; the
# leak lives at the CALL. That gap is this repo's most expensive recurring shape — a test or fix that
# exists but is wired to nothing — and the standing rule is that the answer to it is never a comment
# reminding the next person. So:
#
#   RULE 1  Every connect-ish call in shipped code either scrubs before the NEXT such call, or
#           carries a NOT-A-LOG-PATH verdict that this gate re-derives rather than trusts.
#   RULE 2  The discovered set of call sites must equal the DECLARED inventory below. A new, moved or
#           deleted call site fails until a human updates the inventory — which is the point: adding
#           a fifth place that dials Postgres should force the same decision the first four did.
#   RULE 3  A NONZERO FLOOR. Discovery finding nothing must be RED, not green. This is not
#           speculative either: module 1's declared==ran control passed three of ten self-test
#           branches purely because declared=0 compared equal to ran=0. A gate whose subject vanished
#           reports success about nothing.
#
# TWO DEFECTS IN THIS GATE'S OWN FIRST DRAFT, both found by break-verification and both fixed here.
# They are recorded because each is an instance of a class this repo keeps paying for:
#
#   a) THE WINDOW CAUGHT THE NEIGHBOUR'S SCRUB. The first draft asked "does a scrub appear within N
#      lines after the call". db.go has three call sites within 14 lines of each other, so removing
#      the wrap from the FIRST one left the gate green — it found the SECOND one's scrub and credited
#      it to the first. A gate that passes for the wrong reason is not a control. The fix is that a
#      call site's window now ENDS AT THE NEXT CALL SITE in the same file, so a scrub can only ever
#      be credited to the call it actually follows.
#
#   b) A COMMENT SATISFIED IT. Replacing the wrap with `// TODO: re-enable dsnscrub.Err(err, dsn)`
#      left the gate green. That is the same defect found three times in one day across these repos:
#      module 1's rule 1 matched `go test ./...` inside a comment about itself, both config
#      consumption gates accepted a TODO naming a key as that key's consumer, and test_enumeration
#      accepted a commented-out CI step. A commented-out fix is the LIKELIEST way to reach this — it
#      is exactly what someone writes while disabling something they mean to restore. The fix is that
#      every match in this gate now runs against COMMENT-STRIPPED, STRING-STRIPPED code.
#
# SCOPE IS SHIPPED CODE: services/ and tools/, i.e. what goes into the production images. tests/harness
# is deliberately NOT in scope — it is a separate test-only module whose harness.go prints its DSN via
# t.Fatalf and panic into test output, using the per-run dev credential that scripts/dev.sh generates.
# That is a real (smaller) exposure and it is recorded in Docs/future-work.md rather than silently
# ignored; the reason it is not fixed here is that the harness module cannot import the canonical
# package either, so closing it means a THIRD copy of the scrub, and the copy this gate already has to
# police is tools/send-managed-migrate's.
#
# tools/provision-worker is in scope by directory and contains no driver call at all — which is why the
# nonzero floor and the discovered==declared equality are the load-bearing rules here rather than a
# per-directory expectation: a call appearing there for the first time must fail this gate, not be
# waved through because nobody listed that directory.
#
# NO PAIRED SELFTEST FILE, DELIBERATELY — and this is a claim that was checked, not assumed. The other
# gates here have selftests because the real tree cannot reach some of their branches (the RESERVED
# expiry branch has no RESERVED keys left to evaluate). Every branch of THIS gate is reachable by
# breaking the real tree: unwrap any call site (rule 1), add or delete one (rule 2), remove them all
# (rule 3), bind a discarded error (the NOT-A-LOG-PATH re-derivation), comment out a wrap (defect b).
# All were broken and observed red before this gate was committed.
set -euo pipefail
cd "$(dirname "$0")/../../.."

fail=0

# --- the DECLARED inventory -------------------------------------------------------------------
# One line per call site: <file>|<call text>|<verdict>|<why>
#   SCRUBBED       — the error is returned to a caller that logs it; the scrub must be present.
#   NOT-A-LOG-PATH — the error is DISCARDED at the call and never bound, so nothing can format it.
#
# The key is the CALL TEXT, not a line number: a line number would make this inventory fail on any
# edit above it, which trains people to update it mechanically without reading it.
INVENTORY=(
  "services/control/internal/db/db.go|pgxpool.ParseConfig(dsn)|SCRUBBED|boot: main.go log.Fatalf; MEASURED to echo the whole DSN at pgx v5.7.4"
  "services/control/internal/db/db.go|pgxpool.NewWithConfig(ctx, cfg)|SCRUBBED|boot: same log.Fatalf; lazy at this pin but not contractually so"
  "services/control/internal/db/db.go|pool.Ping(pingCtx)|SCRUBBED|boot: same log.Fatalf; clean at this pin, scrubbed anyway so a pgx change cannot open it"
  "services/control/internal/singleton/singleton.go|pgx.Connect(ctx, dsn)|SCRUBBED|boot: main.go log.Fatalf on singleton acquire"
  "services/control/internal/singleton/singleton.go|pgx.Connect(ctx, g.dsn)|SCRUBBED|RUNTIME, REPEATEDLY: logReacquireFailure logs it on the 1st and every 12th attempt for as long as the lock stays lost — the worst of the paths"
  "services/control/internal/httpapi/health.go|h.db.Pool.Ping(ctx)|NOT-A-LOG-PATH|compared to nil inline; the error value is never bound, so there is nothing to format"
  "tools/send-managed-migrate/runner.go|pgx.Connect(cctx, r.DSN)|SCRUBBED|main.go fatal(err.Error()) to stderr — the output an operator pastes into a ticket"
)

# Identifiers that count as scrubbing. The migrate tool is a separate module with its own copy (see
# tools/send-managed-migrate/dsnscrub.go), so both spellings are accepted here; the two copies are held
# together by TestDSNScrub_MigrateCopyHasNotDriftedFromControl, not by this gate.
SCRUB_RE='dsnscrub\.Err\(|scrubDSNError\('

# The widest a window can get when a call site is the LAST one in its file. Where another call site
# follows, the window ends there instead — see defect (a) above.
MAX_WINDOW=14

CALL_RE='pgx\.Connect\(|pgxpool\.ParseConfig\(|pgxpool\.NewWithConfig\(|pgxpool\.New\(|pgx\.ParseConfig\(|\.Ping\('

# --- code_only: strip comments AND string/char literal CONTENTS -------------------------------
# Emits one output line per input line (so line numbers survive) containing only executable code.
# Quote contents are dropped, not kept: this gate asks whether code CALLS the scrub, and a string
# that happens to contain "dsnscrub.Err(" is not a call. Block comments and raw strings carry state
# across lines, so inblock/inraw are file-scoped; interpreted string and rune literals cannot span a
# line in Go, so those reset per line.
code_only() {
  awk '
    { sub(/\r$/, "") }
    {
      line = $0; out = ""; i = 1; n = length(line); instr = 0; inchar = 0
      while (i <= n) {
        c = substr(line, i, 1); two = substr(line, i, 2)
        if (inblock)     { if (two == "*/") { inblock = 0; i += 2 } else { i++ } ; continue }
        if (inraw)       { if (c == "`") { inraw = 0 } ; i++ ; continue }
        if (instr)       { if (c == "\\") { i += 2; continue }
                           if (c == "\"") { instr = 0 } ; i++ ; continue }
        if (inchar)      { if (c == "\\") { i += 2; continue }
                           if (c == "'"'"'") { inchar = 0 } ; i++ ; continue }
        if (two == "//") { break }
        if (two == "/*") { inblock = 1; i += 2; continue }
        if (c == "\"")   { instr = 1; i++; continue }
        if (c == "`")    { inraw = 1; i++; continue }
        if (c == "'"'"'"){ inchar = 1; i++; continue }
        out = out c; i++
      }
      print out
    }
  ' "$1"
}

for d in services tools; do
  [[ -d "$d" ]] || { echo "dsn_scrub_wiring FAIL: scope directory '$d' does not exist — this gate's scope is stale and it would be measuring nothing."; exit 1; }
done

# --- discovery ---------------------------------------------------------------------------------
# Per-file so line numbers survive the comment strip. Not `go list`/AST: this gate must run with no Go
# toolchain and must see a file that does not compile — which is exactly when a half-finished call
# site is most likely to be sitting there. dsnscrub.go itself is excluded (it names no driver call;
# the exclusion is belt-and-braces against its documentation of the shapes).
SCOPED_FILES="$(find services tools -name '*.go' -not -name '*_test.go' -not -name 'dsnscrub.go' | sort)"

found=""
while IFS= read -r f; do
  [[ -z "$f" ]] && continue
  while IFS= read -r hit; do
    [[ -z "$hit" ]] && continue
    found+="$f:$hit"$'\n'
  done < <(code_only "$f" | grep -nE "$CALL_RE" || true)
done <<<"$SCOPED_FILES"

nfound="$(printf '%s' "$found" | grep -c . || true)"

# --- RULE 3: the nonzero floor -----------------------------------------------------------------
# Checked FIRST. If discovery is broken or the code moved wholesale, every rule below would compare
# empty against empty and pass.
if [[ "$nfound" -eq 0 ]]; then
  echo "dsn_scrub_wiring FAIL: discovery found ZERO driver call sites in services/ and tools/."
  echo "  That is not a pass. Either the call pattern moved (update CALL_RE) or the code did (update"
  echo "  the inventory) — but a gate that cannot find its subject must never report success. This is"
  echo "  the same class as a declared==ran check passing on declared=0."
  exit 1
fi
if [[ "$nfound" -lt "${#INVENTORY[@]}" ]]; then
  echo "dsn_scrub_wiring FAIL: discovery found $nfound call site(s) but the inventory declares ${#INVENTORY[@]}."
  echo "  A call site was deleted or moved out of scope. Confirm that is intended and update the"
  echo "  inventory; do not widen the pattern until you know which one went."
  printf '%s' "$found" | sed '/^$/d;s/^/    /'
  fail=1
fi

# --- RULE 2: discovered == declared ------------------------------------------------------------
declared_keys=""
for entry in "${INVENTORY[@]}"; do
  IFS='|' read -r f marker verdict why <<<"$entry"
  declared_keys+="$f|$marker"$'\n'
  if [[ -z "$why" ]]; then
    echo "dsn_scrub_wiring FAIL: inventory entry '$f|$marker' has an EMPTY reason. A verdict with no"
    echo "  reason is the exemption-becomes-permanent shape this repo has hit three times."
    fail=1
  fi
  nmark=0
  [[ -f "$f" ]] && nmark="$(code_only "$f" | grep -cF -- "$marker" || true)"
  if [[ "$nmark" -eq 0 ]]; then
    echo "dsn_scrub_wiring FAIL: the inventory declares '$marker' in $f, but it is not there (in code —"
    echo "  a mention in a comment does not count)."
    echo "  The inventory is describing code that no longer exists, so its verdict is not about"
    echo "  anything. A stale declaration is worse than a missing one: it reads as coverage."
    fail=1
  elif [[ "$nmark" -gt 1 ]]; then
    echo "dsn_scrub_wiring FAIL: '$marker' appears $nmark times in $f, so this inventory entry is"
    echo "  AMBIGUOUS — the rule below would check only the first occurrence and silently vouch for"
    echo "  the rest. Make the call texts distinct, or add one entry per occurrence."
    fail=1
  fi
done

undeclared=""
while IFS= read -r hit; do
  [[ -z "$hit" ]] && continue
  file="${hit%%:*}"
  rest="${hit#*:}"
  lineno="${rest%%:*}"
  matched=0
  while IFS= read -r key; do
    [[ -z "$key" ]] && continue
    kf="${key%%|*}"
    km="${key#*|}"
    if [[ "$kf" == "$file" ]] && [[ "$hit" == *"$km"* ]]; then
      matched=1
      break
    fi
  done <<<"$declared_keys"
  if [[ "$matched" -eq 0 ]]; then
    undeclared+="$file:$lineno"$'\n'
  fi
done <<<"$found"

if [[ -n "${undeclared//[$'\n']/}" ]]; then
  echo "dsn_scrub_wiring FAIL: driver call site(s) that the inventory does not declare:"
  printf '%s' "$undeclared" | sed '/^$/d;s/^/    - /'
  echo "  Every place this code hands a connection string to pgx needs a decision recorded: does its"
  echo "  error reach a log, and is it scrubbed? Add a line to INVENTORY in this file with the reason."
  echo "  Do NOT widen the pattern to make this pass."
  fail=1
fi

# --- RULE 1: each SCRUBBED site actually scrubs, BEFORE THE NEXT CALL SITE ----------------------
# The window boundary is what makes this attribute a scrub to the RIGHT call. db.go's three sites sit
# within 14 lines of each other; with a fixed-N window, deleting the first site's wrap left this gate
# green because it found the second site's. See defect (a) in the header.
for entry in "${INVENTORY[@]}"; do
  IFS='|' read -r f marker verdict why <<<"$entry"
  [[ "$verdict" == "SCRUBBED" ]] || continue
  [[ -f "$f" ]] || continue
  code="$(code_only "$f")"
  lineno="$(printf '%s\n' "$code" | grep -nF -- "$marker" | head -1 | cut -d: -f1 || true)"
  [[ -n "$lineno" ]] || continue

  # First call site strictly BELOW this one, in code. That is where this window stops.
  next="$(printf '%s\n' "$code" | tail -n "+$((lineno + 1))" | grep -nE "$CALL_RE" | head -1 | cut -d: -f1 || true)"
  end=$((lineno + MAX_WINDOW))
  if [[ -n "$next" ]]; then
    boundary=$((lineno + next - 1))   # the line before the next call site
    [[ "$boundary" -lt "$end" ]] && end="$boundary"
  fi

  window="$(printf '%s\n' "$code" | sed -n "${lineno},${end}p")"
  if ! grep -qE "$SCRUB_RE" <<<"$window"; then
    echo "dsn_scrub_wiring FAIL: $f:$lineno — '$marker' is declared SCRUBBED, but no dsnscrub.Err/"
    echo "  scrubDSNError call appears between it and the next driver call (lines $lineno-$end)."
    echo "  Why this site matters: $why"
    echo "  MEASURED at pgx v5.7.4: a rejected connection string is echoed WHOLE into the error, and"
    echo "  pgx's own redaction misses ?password=, ?sslpassword= and the honored 'password = SECRET'."
    echo "  The internal/dsnscrub unit tests stay GREEN when this wrap is removed, and so do go build"
    echo "  and go vet — this gate is the only thing that notices."
    echo "  NOTE: comments are stripped before matching, so a TODO naming dsnscrub.Err does NOT count."
    fail=1
  fi
done

# --- the NOT-A-LOG-PATH verdicts are RE-DERIVED, not taken on faith ---------------------------
# A site declared NOT-A-LOG-PATH is one whose error is never bound to a variable. If someone later
# binds it, the verdict silently becomes false — the STALE ASSERTION class. So re-derive it.
for entry in "${INVENTORY[@]}"; do
  IFS='|' read -r f marker verdict why <<<"$entry"
  [[ "$verdict" == "NOT-A-LOG-PATH" ]] || continue
  [[ -f "$f" ]] || continue
  text="$(code_only "$f" | grep -F -- "$marker" | head -1 || true)"
  [[ -n "$text" ]] || continue
  if ! grep -qE '(==|!=)[[:space:]]*nil|_[[:space:]]*(:?=)' <<<"$text"; then
    echo "dsn_scrub_wiring FAIL: $f — '$marker' is declared NOT-A-LOG-PATH, but its error is no longer"
    echo "  discarded inline: $text"
    echo "  Something now HOLDS that error, so it can be formatted and logged. Re-derive the verdict:"
    echo "  either scrub it and change the entry to SCRUBBED, or explain the new shape."
    fail=1
  fi
done

if [[ "$fail" -eq 0 ]]; then
  nscrub=0
  for entry in "${INVENTORY[@]}"; do
    IFS='|' read -r _ _ verdict _ <<<"$entry"
    [[ "$verdict" == "SCRUBBED" ]] && nscrub=$((nscrub + 1))
  done
  echo "dsn_scrub_wiring OK: $nfound driver call site(s) discovered in services/ and tools/, all ${#INVENTORY[@]} declared; $nscrub scrubbed before the next driver call, $(( ${#INVENTORY[@]} - nscrub )) re-derived as discarding their error inline."
fi
exit "$fail"
