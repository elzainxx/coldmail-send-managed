#!/usr/bin/env bash
# config_drift — BLOCKING CI gate. Asserts the live send_managed.config key set equals the
# phase-aware manifest (Docs/config-manifest.txt) AND the config-reference.md table. This
# structurally forbids a hardcoded tuning constant: a dial that isn't a seeded row with a
# documented default cannot pass. Grows monotonically to 50 keys after send_managed_0021 (OD-6).
#
# Phase 0 seeds ZERO config keys, so the expected set is empty. The gate still runs so the
# discipline is live from day one (a stray seeded key, or a documented-but-unseeded key,
# fails immediately).
set -euo pipefail
cd "$(dirname "$0")/../../.."

DC="${COMPOSE_INVOCATION:-docker compose -f docker-compose.yml -f tests/harness/compose.test.yml}"
MANIFEST="Docs/config-manifest.txt"

# 1) Expected keys = non-comment, non-blank lines of the manifest.
expected="$(grep -vE '^\s*#|^\s*$' "$MANIFEST" 2>/dev/null | sort -u || true)"

# 2) Live keys seeded in send_managed.config (empty if the table doesn't exist yet — Phase 0).
live="$($DC exec -T postgres psql -U postgres -d platform -tAc \
  "SELECT key FROM send_managed.config ORDER BY key" 2>/dev/null | sort -u || true)"

# 3) Documented keys = the key column of the config-reference table (rows like | `key` | ... |).
# Key charset includes digits (e.g. retry_count_4xx) — [a-z0-9_].
documented="$(grep -oE '^\| *`[a-z0-9_]+` *\|' Docs/config-reference.md 2>/dev/null \
  | tr -d '|` ' | sort -u || true)"

fail=0
if [[ "$expected" != "$live" ]]; then
  echo "config_drift FAIL: live send_managed.config keys != manifest"
  echo "  in manifest not live: $(comm -23 <(echo "$expected") <(echo "$live") | tr '\n' ' ')"
  echo "  live not in manifest: $(comm -13 <(echo "$expected") <(echo "$live") | tr '\n' ' ')"
  fail=1
fi
if [[ "$expected" != "$documented" ]]; then
  echo "config_drift FAIL: documented (config-reference.md) keys != manifest"
  echo "  in manifest not documented: $(comm -23 <(echo "$expected") <(echo "$documented") | tr '\n' ' ')"
  echo "  documented not in manifest: $(comm -13 <(echo "$expected") <(echo "$documented") | tr '\n' ' ')"
  fail=1
fi

if [[ "$fail" -eq 0 ]]; then
  n="$(echo "$expected" | grep -c . || true)"
  echo "config_drift OK: $n config key(s); live == manifest == config-reference.md"
fi
exit "$fail"
