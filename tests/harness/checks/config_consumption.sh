#!/usr/bin/env bash
# config_consumption — BLOCKING CI gate. Closes the gap config_drift structurally CANNOT catch:
# config_drift proves a key is SEEDED, DOCUMENTED, and matches the reference — never that a
# CONSUMER exists. Two Phase 9.5 review-gate passes each found, by accident, a key that was
# seeded/documented/passing config_drift but read by NO code path (an operator turns the dial and
# nothing happens — worse than a hardcoded constant, since it's a false promise of control). This
# gate makes that discipline permanent: every key in the manifest must EITHER have a real,
# grep-verifiable consumer, or be explicitly marked RESERVED in both the consumers ledger and the
# operator-facing config reference. Mirrors the ADR-0008 grants-invariant discipline: turn a fact
# that must be remembered into one that cannot be forgotten.
set -euo pipefail
cd "$(dirname "$0")/../../.."

MANIFEST="Docs/config-manifest.txt"
LEDGER="Docs/config-consumers.txt"
REFDOC="Docs/config-reference.md"

fail=0

# The `|| true` on both lines is load-bearing, not defensive noise. Under this file's `set -euo
# pipefail`, a manifest or ledger with ZERO non-comment lines makes grep exit 1, which kills the gate
# mid-check with rc=1 and NO OUTPUT AT ALL. rc=1 reads as "the gate failed" in CI, so the symptom is
# not silence but a MISLEADING failure with no reason attached — and the two worst inputs (an emptied
# manifest, an emptied ledger) are exactly the ones that would produce it. Found 2026-08-05 by this
# gate's new paired control, case 6, which is precisely what a control is for; the identical shape had
# already been found and fixed in test_enumeration.sh days earlier.
manifest_keys="$(grep -vE '^\s*#|^\s*$' "$MANIFEST" | sort -u || true)"
ledger_keys="$(grep -vE '^\s*#|^\s*$' "$LEDGER" | cut -d'|' -f1 | sort -u || true)"

# 1) Every manifest key has EXACTLY one ledger line, and vice versa (no silent omission, no stale
#    leftover entry for a removed key).
missing_from_ledger="$(comm -23 <(echo "$manifest_keys") <(echo "$ledger_keys") || true)"
if [[ -n "$missing_from_ledger" ]]; then
  echo "config_consumption FAIL: key(s) seeded/documented but MISSING from $LEDGER (no consumption disposition recorded):"
  echo "$missing_from_ledger" | sed 's/^/  - /'
  fail=1
fi
stale_in_ledger="$(comm -13 <(echo "$manifest_keys") <(echo "$ledger_keys") || true)"
if [[ -n "$stale_in_ledger" ]]; then
  echo "config_consumption FAIL: $LEDGER has stale entry/entries for a key no longer in $MANIFEST:"
  echo "$stale_in_ledger" | sed 's/^/  - /'
  fail=1
fi

# 2) For each ledger line, verify the disposition actually holds in the tree.
while IFS='|' read -r key pattern; do
  [[ -z "$key" || "$key" =~ ^# ]] && continue
  if [[ "$pattern" == "RESERVED" ]]; then
    # A RESERVED key must be readable-as-such by an operator: the exact marker string must appear
    # on that key's own row in the human-facing config reference, not just in this ledger.
    if ! grep -q "\`$key\`.*RESERVED / NOT-YET-CONSUMED" "$REFDOC"; then
      echo "config_consumption FAIL: '$key' is marked RESERVED in $LEDGER but $REFDOC has no matching RESERVED / NOT-YET-CONSUMED marker on its row — an operator reading the docs would not know this dial does nothing"
      fail=1
    fi
    continue
  fi
  # A CONSUMED key's pattern must match some real file OTHER than the config loader, the
  # write-time validator, or a test — i.e. a genuine runtime consumer, not just "this key is known
  # to exist somewhere."
  #
  # THE MATCH IS AGAINST CODE, NOT COMMENTS (fixed 2026-08-05). This check previously grepped whole
  # files, so a COMMENT naming the pattern satisfied it. Break-verified on this tree: replacing
  # runner.go:118's `time.Duration(cfg.DispatchTickIntervalMS) * time.Millisecond` with a hardcoded
  # 2000ms correctly turned the gate RED — but adding
  #     return 2000 * time.Millisecond // TODO: restore cfg.DispatchTickIntervalMS
  # turned it GREEN again while the dispatch tick ignored its dial. The gate then reported "all 54
  # config key(s) have a verified consumer" about a tree where one of them was inert.
  #
  # That is the same defect found the same day in module 1's test-enumeration rule 1, which matched
  # `go test ./...` inside a comment about itself: a gate satisfied by PROSE. It is the mirror image
  # of the warning-comment problem — there, a comment failed to be a control; here, a comment
  # satisfied one. Both are only findable by breaking the guarded thing and watching.
  #
  # A TODO naming the key is the LIKELIEST way to reach it, not a contrived one: it is what someone
  # writes while temporarily disabling a dial, and it is precisely when the operator-facing promise
  # ("this key does something") stops being true.
  #
  # Stripping is line-based `//` removal. Verified safe on this tree before adopting: no ledger
  # pattern contains `//` or `:`, and no consumer line matches the pattern after a `://`, so no URL
  # or string literal is truncated into a false negative. Both languages here (.go, .js) use `//`.
  # Block comments (/* ... */) are NOT stripped — deliberately, since a line-based pass cannot do it
  # correctly, and erring toward ACCEPTING a block-commented mention is the safe direction: a false
  # PASS is caught by the next reviewer, whereas a false FAIL on real code would train people to add
  # exemptions. Module 3 carries the identical fix and a paired control that pins the `//` case.
  hits=""
  while IFS= read -r f; do
    [[ -z "$f" ]] && continue
    if sed -e 's|//.*||' "$f" | grep -qE "$pattern"; then
      hits+="$f"$'\n'
    fi
  done < <(grep -rlE "$pattern" --include=*.go --include=*.js . 2>/dev/null \
    | grep -vE '_test\.go$|/dispatch/config\.go$|/adminapi/config_validate\.go$' || true)
  if [[ -z "${hits//[$'\n']/}" ]]; then
    # Distinguish "nowhere at all" from "only in a comment" — the remedies are opposite. The first
    # means the consumer was never written; the second means it was REMOVED and a note left behind,
    # so the reader needs to be told the note is not the thing.
    commented="$(grep -rlE "$pattern" --include=*.go --include=*.js . 2>/dev/null \
      | grep -vE '_test\.go$|/dispatch/config\.go$|/adminapi/config_validate\.go$' || true)"
    if [[ -n "$commented" ]]; then
      echo "config_consumption FAIL: '$key' matches pattern '$pattern' ONLY INSIDE COMMENTS, never in code:"
      echo "$commented" | sed '/^$/d;s/^/    - /'
      echo "  A comment mentioning the key is not a consumer. The operator turns this dial and nothing"
      echo "  happens, which is worse than a hardcoded constant — it is a false promise of control."
      echo "  Fix: restore the real read, or dispose of the key (row+manifest+refdoc+ledger together)."
    else
      echo "config_consumption FAIL: '$key' has no real consumer matching pattern '$pattern' outside the config loader/validator/tests — seeded+documented but structurally unconsumed (the exact class of bug this gate exists to catch)"
    fi
    fail=1
  fi
done < "$LEDGER"

if [[ "$fail" -eq 0 ]]; then
  n="$(echo "$manifest_keys" | grep -c . || true)"
  echo "config_consumption OK: all $n config key(s) have a verified consumer or an explicit, operator-visible RESERVED marker"
fi
exit "$fail"
