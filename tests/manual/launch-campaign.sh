#!/usr/bin/env bash
# launch-campaign.sh — seed a platform campaign and POST /v1/run against the LIVE dev stack.
#
# WHY THIS IS TRACKED. Walking Docs/coldmail-send-managed-local-test-guide.md needs a campaign on demand at
# nearly every stage (7, 8, 9, 10, 12). That was being done with ad-hoc throwaway shell each time,
# which is how two stages ended up not reproducible after the fact. This is that recipe, once.
#
# It is a TEST DRIVER, not product code: it writes public.job_items (the PLATFORM's table, which the
# real platform owns and Module 2 only reads) and then calls the real Domain-A endpoint with the real
# service token. It touches nothing in the `send_managed` schema — the Control does all of that, which is the
# point: the campaign has to arrive the way a real caller sends it.
#
# Usage:
#   tests/manual/launch-campaign.sh <tag> <count>            # seed + run
#   tests/manual/launch-campaign.sh <tag> <count> --seed-only # seed, print the ids, do not run
#
# Writes <tag>.ids (the send_managed.jobs ids, once run) to the CWD so a follow-up assertion can name the
# exact rows it expects, rather than "the most recent N jobs" — which is wrong the moment two runs
# overlap, and overlapping runs are exactly what Stage 9 does.
set -euo pipefail
cd "$(dirname "$0")/../.."

TAG="${1:?usage: launch-campaign.sh <tag> <count> [--seed-only]}"
COUNT="${2:?usage: launch-campaign.sh <tag> <count> [--seed-only]}"
SEED_ONLY="${3:-}"

DC="${COMPOSE_INVOCATION:-docker compose}"
PSQL="$DC exec -T postgres psql -U postgres -d platform -tAc"

# CAMPAIGN_BODY overrides the default one-line body. Needed by Stage 12 (content variation): the
# default body carries NO HTML, and most variation toggles are structural — comment injection only
# fires after a closing `</tag>`, CSS micro-variation only on `padding`/`margin` px declarations, URL
# params only on href/src. Against a tagless body every one of them is a NO-OP, so a
# byte-comparison "proves" all-toggles-off is identical to toggles-ON and the whole stage passes
# vacuously. That is not a hypothetical: it is what happened on the first attempt at Stage 12 step 1,
# and the negative control is what caught it.
#
# The literal `%d` in the value is replaced with the message number so each recipient still gets
# distinguishable content. Single quotes are doubled for SQL safety.
CAMPAIGN_BODY="${CAMPAIGN_BODY:-Body for $TAG message %d.}"

# The dev service token. Domain A only — never the admin key (CLAUDE.md Rule 6: the two auth domains
# never share a credential, so a driver that needs /v1/run must not hold the admin bearer).
PLATFORM_TOKEN="$(grep -E '^PLATFORM_TOKEN=' .env | cut -d= -f2)"
[ -n "$PLATFORM_TOKEN" ] || { echo "no PLATFORM_TOKEN in .env" >&2; exit 1; }

# Reuse the existing dev user rather than inventing one — send_managed.accounts rows are already scoped to
# it, and a fresh user_id would have no pool and every job would correctly fail routing.
#
# CAMPAIGN_USER_ID overrides it. Needed from Stage 10 on: an isolated tenant (own dedicated pool, own
# accounts) is the only way to drive an account-level outcome without contaminating the accounts every
# earlier stage's assertions still name. It is also what keeps the one-campaign-per-user guard out of
# the way — that guard is per user, so a dedicated tenant can run while another campaign is parked.
USER_ID="${CAMPAIGN_USER_ID:-$($PSQL "SELECT id FROM public.users ORDER BY created_at LIMIT 1")}"
[ -n "$USER_ID" ] || { echo "no rows in public.users — run scripts/dev.sh seed first" >&2; exit 1; }

CAMPAIGN_ID="$($PSQL "SELECT gen_random_uuid()")"

# Seed the recipients. rendered_subject carries $TAG so Mailpit can be filtered per-run by subject,
# which is how the "exactly one copy" assertions in Stage 9 steps 6-7 stay honest across runs.
$DC exec -T postgres psql -U postgres -d platform -q <<SQL
INSERT INTO public.job_items
  (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, reply_to, status)
SELECT '$CAMPAIGN_ID', '$USER_ID',
       's9-' || g || '@example.test',
       '[$TAG] message ' || g,
       replace('$(printf '%s' "$CAMPAIGN_BODY" | sed "s/'/''/g")', '%d', g::text),
       'reply@ourdomain.com',
       'pending'
FROM generate_series(1, $COUNT) g;
SQL

echo "seeded $COUNT job_items  campaign=$CAMPAIGN_ID  user=$USER_ID  tag=$TAG"
$PSQL "SELECT id FROM public.job_items WHERE campaign_id='$CAMPAIGN_ID' ORDER BY rendered_subject" \
  > "$TAG.items"
echo "  item ids -> $TAG.items"

if [ "$SEED_ONLY" = "--seed-only" ]; then
  echo "  --seed-only: not calling /v1/run"
  exit 0
fi

# /v1/run takes ONE job_item_id plus user_id (api-contract.md is authoritative; the Phase-9 spec's
# quick-reference omits user_id — that is the known-stale example R2-5, see future-work.md §7).
: > "$TAG.ids"
while read -r ITEM; do
  [ -n "$ITEM" ] || continue
  RESP="$(curl -sS -X POST http://localhost:8080/v1/run \
    -H "Authorization: Bearer $PLATFORM_TOKEN" \
    -H 'Content-Type: application/json' \
    -d "{\"job_item_id\":\"$ITEM\",\"user_id\":\"$USER_ID\"}")"
  echo "$RESP" | grep -q '"job_id"' || { echo "run failed for $ITEM: $RESP" >&2; exit 1; }
  echo "$RESP" | sed -E 's/.*"job_id":"([^"]+)".*/\1/' >> "$TAG.ids"
done < "$TAG.items"

echo "ran $(wc -l < "$TAG.ids" | tr -d ' ') jobs  ->  $TAG.ids"
echo "campaign_id=$CAMPAIGN_ID"
