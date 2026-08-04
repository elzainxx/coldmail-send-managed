#!/usr/bin/env bash
# contract_drift — BLOCKING CI gate. Asserts the Control SERVES exactly the routes documented
# in Docs/api-contract.md, and no undocumented ones, for the Phase-0 surface. Additive-only:
# a documented route the Control doesn't answer, or a served route that's undocumented, fails.
#
# Health routes are probed on the host; the mTLS workerlink routes are probed IN-NETWORK from
# the worker container (host->localhost is intercepted by host AV TLS-inspection; OD-19).
set -euo pipefail
cd "$(dirname "$0")/../../.."
DC="${COMPOSE_INVOCATION:-docker compose -f docker-compose.yml -f tests/harness/compose.test.yml}"

fail=0

# --- Documented Phase-0 routes (must match api-contract.md tables) ---
# health (Domain: none in Phase 0)
if ! curl -fsS http://localhost:8080/healthz >/dev/null 2>&1; then echo "contract_drift FAIL: GET /healthz not served"; fail=1; fi
# readyz answers 200 or 503 (both are "served"); treat connection refusal as not-served.
code="$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/readyz 2>/dev/null || echo 000)"
[[ "$code" == "200" || "$code" == "503" ]] || { echo "contract_drift FAIL: GET /readyz not served (got $code)"; fail=1; }

# workerlink mTLS routes (probed in-network). Expect 200 (register w/ token) then 200 (heartbeat).
WORKERTOK="$(grep -E '^DEV_WORKER_TOKEN=' .env 2>/dev/null | cut -d= -f2- || true)"
probe() { $DC exec -T worker sh -c "curl -s -o /dev/null -w '%{http_code}' --cert /secrets/mtls/worker.crt --key /secrets/mtls/worker.key --cacert /secrets/mtls/ca.crt -X POST https://control:8443$1 -H 'content-type: application/json' -d '$2'" 2>/dev/null || echo 000; }
rc="$(probe /v1/worker/register "{\"box_id\":\"dev-worker-1\",\"token\":\"$WORKERTOK\"}")"
[[ "$rc" == "200" ]] || { echo "contract_drift FAIL: POST /v1/worker/register not served as documented (got $rc)"; fail=1; }
rc="$(probe /v1/worker/heartbeat '{"box_id":"dev-worker-1"}')"
[[ "$rc" == "200" ]] || { echo "contract_drift FAIL: POST /v1/worker/heartbeat not served as documented (got $rc)"; fail=1; }

# --- Phase 2 operator admin routes (Domain B) ---
# These are behind the operator-key gate. Probed WITHOUT a key: a real, mounted route answers
# 401 unauthenticated (route exists but gated); a missing route would 404. So 401 proves the
# route is served, and is the additive-only assertion. (The behavioral suite proves the authed
# behavior; here we only prove the surface matches the contract.) Method-appropriate probes.
adm() { # $1=method $2=path -> http code (no auth header)
  curl -s -o /dev/null -w '%{http_code}' -X "$1" "http://localhost:8080$2" 2>/dev/null || echo 000
}
# Only run the admin-route checks if the surface is mounted (ADMIN_API_KEY_HASHES configured).
# A collection GET without a key must be 401 when mounted; 404 means the surface is off.
adm_probe="$(adm GET /v1/admin/accounts)"
if [[ "$adm_probe" == "401" ]]; then
  declare -A ADMIN_ROUTES=(
    ["GET /v1/admin/accounts"]=1 ["POST /v1/admin/accounts"]=1
    ["GET /v1/admin/accounts/00000000-0000-0000-0000-000000000000"]=1
    ["PATCH /v1/admin/accounts/00000000-0000-0000-0000-000000000000"]=1
    ["DELETE /v1/admin/accounts/00000000-0000-0000-0000-000000000000"]=1
    ["GET /v1/admin/accounts/00000000-0000-0000-0000-000000000000/events"]=1
    ["POST /v1/admin/accounts/00000000-0000-0000-0000-000000000000/quarantine"]=1
    ["POST /v1/admin/accounts/00000000-0000-0000-0000-000000000000/unquarantine"]=1
    ["POST /v1/admin/accounts/00000000-0000-0000-0000-000000000000/disable"]=1
    ["POST /v1/admin/accounts/00000000-0000-0000-0000-000000000000/enable"]=1
    ["POST /v1/admin/accounts/00000000-0000-0000-0000-000000000000/pause"]=1
    ["POST /v1/admin/accounts/00000000-0000-0000-0000-000000000000/unpause"]=1
    ["GET /v1/admin/pools"]=1 ["POST /v1/admin/pools"]=1
    ["GET /v1/admin/pools/00000000-0000-0000-0000-000000000000"]=1
    ["PATCH /v1/admin/pools/00000000-0000-0000-0000-000000000000"]=1
    ["DELETE /v1/admin/pools/00000000-0000-0000-0000-000000000000"]=1
    ["GET /v1/admin/config"]=1 ["GET /v1/admin/config/daily_limit_default"]=1
    ["PUT /v1/admin/config/daily_limit_default"]=1
    ["GET /v1/admin/health"]=1
  )
  for spec in "${!ADMIN_ROUTES[@]}"; do
    m="${spec%% *}"; p="${spec#* }"
    rc="$(adm "$m" "$p")"
    [[ "$rc" == "401" ]] || { echo "contract_drift FAIL: $m $p served as $rc, want 401 (route missing or not gated)"; fail=1; }
  done
  # An UNDOCUMENTED admin path must NOT be gated as a real route — it should 404 (no handler).
  und_adm="$(adm GET /v1/admin/secret-undocumented)"
  [[ "$und_adm" == "404" ]] || { echo "contract_drift FAIL: undocumented admin path returned $und_adm, want 404 — surface wider than the contract"; fail=1; }
  # DERIVED from the array, not written as a literal. This line said "22 routes" while ADMIN_ROUTES
  # held 21 entries: the gate was checking the right number of routes and REPORTING the wrong one, so
  # the green line was misleading evidence rather than a failure. That is the print-only drift class
  # already recorded twice in Docs/history (phase-1-schema-delta.md, phase-2-strip-shared-pool.md) —
  # a count that reads ${#ADMIN_ROUTES[@]} cannot drift from the set it describes.
  echo "contract_drift: Phase 2+9.5 admin surface mounted — ${#ADMIN_ROUTES[@]} routes gated (401), undocumented 404."
else
  echo "contract_drift: admin surface not mounted (GET /v1/admin/accounts -> $adm_probe); skipping Phase 2 route checks (set ADMIN_API_KEY_HASHES to enable)."
fi

# --- Phase 3 platform routes (Domain A) ---
# Behind the platform service-token gate. Probed WITHOUT a token: a real, mounted route answers
# 401 unauthenticated (route exists but gated); a missing route 404s. So 401 proves the route is
# served (the behavioral suite proves authed behavior). Method-appropriate probes.
pf() { curl -s -o /dev/null -w '%{http_code}' -X "$1" "http://localhost:8080$2" 2>/dev/null || echo 000; }
pf_probe="$(pf POST /v1/run)"
if [[ "$pf_probe" == "401" ]]; then
  declare -A PLATFORM_ROUTES=(
    ["POST /v1/quote"]=1 ["POST /v1/run"]=1 ["GET /v1/report"]=1 ["GET /v1/describe"]=1
  )
  for spec in "${!PLATFORM_ROUTES[@]}"; do
    m="${spec%% *}"; p="${spec#* }"
    rc="$(pf "$m" "$p")"
    [[ "$rc" == "401" ]] || { echo "contract_drift FAIL: $m $p served as $rc, want 401 (platform route missing or not gated)"; fail=1; }
  done
  # Derived for the same reason as the admin line above. This one happens to be correct today (4
  # entries, 4 claimed), which is exactly why it is worth deriving — a correct literal is one edit away
  # from being a wrong one, and nothing would catch it.
  echo "contract_drift: Phase 3 platform surface mounted — ${#PLATFORM_ROUTES[@]} routes gated (401)."
else
  echo "contract_drift: platform surface not mounted (POST /v1/run -> $pf_probe); skipping Phase 3 route checks (set PLATFORM_TOKEN_HASHES to enable)."
fi

# --- describe's DECLARATIONS match the source they are derived from (fix-plan items 6 + 8) ---
# The behavioral suite proves the derivation (a real ALTER TYPE ... ADD VALUE shows up in the next
# response). What a GATE adds is the doc<->served lockstep this file exists for, plus a check that the
# SERVED surface still carries every vocabulary the source declares — a describe that quietly stopped
# publishing one would leave clients on their stale hardcoded lists, which is the failure item 8 is
# about, and no route-existence probe would notice.
#
# Every count below is DERIVED on both sides (the served JSON vs. a grep of the declaration source).
# Not one is written as a literal — the same rule as the ${#ADMIN_ROUTES[@]} line above, adopted
# because this gate has already shipped a wrong hardcoded count (see the note there).
#
# `|| true` on every count pipeline is LOAD-BEARING under this file's `set -euo pipefail`: grep exits 1
# when it matches NOTHING, pipefail propagates that out of the command substitution, and `set -e` would
# then kill this gate mid-run with no message at all — printing only the route lines and exiting 1,
# which reads as an infrastructure problem rather than the honest "describe is missing a field" the
# comparisons below would have printed. The `|| true` never masks a failure here: every value is judged
# by an explicit comparison, including a >0 vacuity check on each want_* side.
PLATFORM_TOKEN_VALUE="$(grep -E '^PLATFORM_TOKEN=' .env 2>/dev/null | cut -d= -f2- || true)"
if [[ -n "$PLATFORM_TOKEN_VALUE" ]]; then
  desc="$(curl -fsS -H "Authorization: Bearer ${PLATFORM_TOKEN_VALUE}" \
    http://localhost:8080/v1/describe 2>/dev/null || true)"
  if [[ -z "$desc" ]]; then
    echo "contract_drift FAIL: GET /v1/describe returned nothing with the dev platform token"; fail=1
  else
    VOC_SRC="services/control/internal/platformapi/describe_vocabularies.go"

    # error_codes[] vs platformapi's codeStatus map. The map is what writeError resolves a status
    # from, so its entry count is the number of codes this module can emit.
    want_codes="$(grep -cE '^\s+code[A-Za-z]+:\s+http\.Status' services/control/internal/platformapi/errors.go || true)"
    got_codes="$(grep -oE '"code":"[a-z_]+","http_status":[0-9]+' <<<"$desc" | wc -l | tr -d ' ' || true)"
    want_codes="${want_codes:-0}"; got_codes="${got_codes:-0}"
    [[ "$want_codes" -gt 0 ]] || { echo "contract_drift FAIL: read 0 codeStatus entries from errors.go — this check would be vacuous"; fail=1; }
    if [[ "$got_codes" != "$want_codes" ]]; then
      echo "contract_drift FAIL: describe publishes $got_codes error codes, codeStatus declares $want_codes"; fail=1
    fi
    # The fixed MESSAGE must never be published (two-vocabulary rule): a client must branch on the
    # code, never on operator-facing English. Go marshals map keys alphabetically, so a leaked
    # message would sit immediately after http_status.
    if grep -qE '"code":"[a-z_]+","http_status":[0-9]+,"message"' <<<"$desc"; then
      echo "contract_drift FAIL: describe's error_codes[] publishes a message — clients would match on message text"; fail=1
    fi
    # Doc lockstep, one direction only: the api-contract error table is the UNION across domains (it
    # also carries adminapi's config_ordering_violation and the reserved forbidden/unprocessable/
    # rate_limited), so a documented code need not be emittable on THIS surface — but an emittable one
    # must be documented.
    while read -r c; do
      [[ -n "$c" ]] || continue
      grep -q "| \`$c\` |" Docs/api-contract.md || { echo "contract_drift FAIL: describe publishes error code $c but api-contract.md's error table does not list it"; fail=1; }
    done < <(grep -oE '"code":"[a-z_]+","http_status"' <<<"$desc" | sed -E 's/"code":"([a-z_]+)".*/\1/' || true)

    # vocabularies{} vs its declaration source: one served entry per exposedDBEnums entry (source
    # db_enum) and per staticVocabularies entry (source module_constant).
    want_db="$(grep -cE '^\s+\{Type: "[a-z_]+"' "$VOC_SRC" || true)"
    want_static="$(grep -cE '^\s+Name:\s+"[a-z_]+",$' "$VOC_SRC" || true)"
    got_db="$(grep -oE '"source":"db_enum"' <<<"$desc" | wc -l | tr -d ' ' || true)"
    got_static="$(grep -oE '"source":"module_constant"' <<<"$desc" | wc -l | tr -d ' ' || true)"
    want_db="${want_db:-0}"; want_static="${want_static:-0}"
    got_db="${got_db:-0}"; got_static="${got_static:-0}"
    [[ "$want_db" -gt 0 && "$want_static" -gt 0 ]] || { echo "contract_drift FAIL: read $want_db/$want_static vocabulary declarations from $VOC_SRC — the greps are broken and this check would be vacuous"; fail=1; }
    [[ "$got_db" == "$want_db" ]] || { echo "contract_drift FAIL: describe publishes $got_db db_enum vocabularies, exposedDBEnums declares $want_db"; fail=1; }
    [[ "$got_static" == "$want_static" ]] || { echo "contract_drift FAIL: describe publishes $got_static module_constant vocabularies, staticVocabularies declares $want_static"; fail=1; }
    # Both loops below match a published vocabulary entry with `"<name>":\{[^{}]*"source"` rather than
    # a fixed key ORDER. encoding/json marshals map keys ALPHABETICALLY, so an entry serializes as
    # {"fields":...,"source":...,"values":...} — a `"<name>":{"source"` pattern would never match, and
    # module 3's exclusion loop was exactly that until its own fail-verification caught it (a check that
    # cannot fire is worse than no check: it reports green for a property nobody is testing). The
    # `[^{}]*` is safe because a vocabulary entry's own value contains arrays but no nested objects,
    # and requiring "source" inside it also rules out an accidental match on a same-named key elsewhere.
    #
    # Every exposedDBEnums type must be published — a count alone would pass if one type were emitted
    # twice and another dropped.
    while read -r t; do
      [[ -n "$t" ]] || continue
      grep -qE "\"$t\":\{[^{}]*\"source\":\"db_enum\"" <<<"$desc" || { echo "contract_drift FAIL: exposedDBEnums declares $t but describe does not publish it as a db_enum vocabulary"; fail=1; }
    done < <(grep -oE '^\s+\{Type: "[a-z_]+"' "$VOC_SRC" | sed -E 's/.*"([a-z_]+)".*/\1/' || true)
    # The DELIBERATELY excluded types must be absent, so the exclusion is enforced on the wire and not
    # merely asserted in Go. FIVE of this module's seven enums are excluded, so this loop carries more
    # weight here than in module 3 — the exclusion list is the bulk of the accounting, not a footnote.
    while read -r t; do
      [[ -n "$t" ]] || continue
      # `if grep`, not `grep && {...}`: a bare `grep &&` whose grep does not match is the LAST command
      # of the loop body, so its exit 1 becomes the loop's status and set -e would kill the gate on the
      # HEALTHY path (the excluded type correctly absent) — the inverted-guard trap.
      if grep -qE "\"$t\":\{[^{}]*\"source\"" <<<"$desc"; then
        echo "contract_drift FAIL: $t is declared NOT exposed but describe publishes it"; fail=1
      fi
    done < <(grep -oE '^\s+"[a-z_]+": "' "$VOC_SRC" | sed -E 's/.*"([a-z_]+)".*/\1/' || true)

    # pricing.models[] (item 6): the declared set must be served, and pricing.model must be a member.
    grep -q '"models":\[' <<<"$desc" || { echo "contract_drift FAIL: describe does not publish pricing.models[] (fix-plan item 6)"; fail=1; }
    served_model="$(grep -oE '"model":"[a-z_]+"' <<<"$desc" | head -1 | sed -E 's/.*"([a-z_]+)"$/\1/' || true)"
    if [[ -n "$served_model" ]]; then
      grep -qE "\"models\":\[[^]]*\"$served_model\"" <<<"$desc" || { echo "contract_drift FAIL: pricing.model=$served_model is not a member of the published models[]"; fail=1; }
    else
      echo "contract_drift FAIL: describe publishes no pricing.model"; fail=1
    fi
    # The tier axis IS the pool_kind axis (see $VOC_SRC on why pool_kind is declared db_enum even
    # though describeTiers writes the ids as Go literals), so every advertised tier id must be one of
    # the published pool_kind values. The Go test pins both directions against the live catalog; this
    # is the on-the-wire half, and it is the one that would catch a describe served by a container
    # whose binary predates a pool_kind addition.
    pool_vals="$(grep -oE '"pool_kind":\{[^{}]*\}' <<<"$desc" || true)"
    if [[ -z "$pool_vals" ]]; then
      echo "contract_drift FAIL: describe publishes no pool_kind vocabulary — the tier ids have nothing to be checked against"; fail=1
    else
      tier_n=0
      while read -r id; do
        [[ -n "$id" ]] || continue
        tier_n=$((tier_n + 1))
        grep -qE "\"values\":\[[^]]*\"$id\"" <<<"$pool_vals" || { echo "contract_drift FAIL: describe advertises tier id $id, which is not a published pool_kind value — a tier the schema cannot express routes nowhere"; fail=1; }
      done < <(grep -oE '"id":"[a-z_]+"' <<<"$desc" | sed -E 's/.*"([a-z_]+)"$/\1/' || true)
      [[ "$tier_n" -gt 0 ]] || { echo "contract_drift FAIL: read 0 tier ids out of describe — this check would be vacuous"; fail=1; }
    fi

    echo "contract_drift: describe declarations derived-and-served — ${got_codes} error codes (== codeStatus), ${got_db} db_enum + ${got_static} module_constant vocabularies (== declaration source), pricing.model in models[], every tier id a published pool_kind."
  fi
else
  echo "contract_drift: no PLATFORM_TOKEN in .env; skipping describe declaration checks (run scripts/dev.sh certs to generate one)."
fi

# --- No undocumented route: a path NOT in the contract must 404 ---
und="$(curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/v1/undocumented-nonadmin 2>/dev/null || echo 000)"
[[ "$und" == "404" ]] || { echo "contract_drift FAIL: undocumented path did not 404 (got $und) — surface wider than the contract"; fail=1; }

# --- The contract doc actually lists these routes (doc <-> served lockstep) ---
for r in "/healthz" "/readyz" "/v1/worker/register" "/v1/worker/heartbeat" \
         "/v1/admin/accounts" "/v1/admin/pools" "/v1/admin/config" \
         "/v1/admin/accounts/{id}/quarantine" "/v1/admin/accounts/{id}/unpause" \
         "/v1/admin/accounts/{id}/events" "/v1/admin/health" \
         "/v1/quote" "/v1/run" "/v1/report" "/v1/describe"; do
  grep -q "$r" Docs/api-contract.md || { echo "contract_drift FAIL: $r served but not documented in api-contract.md"; fail=1; }
done

[[ "$fail" -eq 0 ]] && echo "contract_drift OK: served routes == documented contract (Phase 0 + Phase 2 admin)."
exit "$fail"
