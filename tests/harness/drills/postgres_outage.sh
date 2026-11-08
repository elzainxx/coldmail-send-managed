#!/usr/bin/env bash
# postgres_outage — the DATABASE-OUTAGE DRILL, scripted. Docs/send-managed-local-test-guide.md, Stage 9 step 8:
#   "the Control halts cleanly, does not crash-loop, and INVENTS NOTHING — no job is marked sent or failed during the
#    outage. On recovery it resumes with no double-processing. This is the single most important assertion in the guide."
#
# ON-DEMAND, not a per-push CI step (like the E11 load test): it needs the DEFAULT, volume-backed Postgres stack — under
# DEV_RAM_PG=1 a `docker stop postgres` DESTROYS the database, and this script refuses to run there — and it takes minutes.
#
#   bash scripts/dev.sh certs && bash scripts/dev.sh reset && bash tests/harness/drills/postgres_outage.sh
#
# WHAT MAKES IT A REAL DRILL AND NOT THE EASY CASE (the guide's own warning: "an outage with nothing in flight is the easy
# case and skips the whole question"):
#   * a job is genuinely IN FLIGHT when the database dies — the worker is paused (cgroup freezer), so the job is pushed and
#     pinned `sending` with no outcome reported;
#   * a SNAPSHOT of totals AND max(seq) is taken before the plug is pulled — counting rows afterwards cannot distinguish
#     "nothing was written" from "something plausible was written";
#   * a run request is made DURING the outage, against a job_item created beforehand, and must be REFUSED — and afterwards
#     no job may exist for it (a `queued` job we could not persist is an invented outcome);
#   * the worker is released only AFTER recovery, and the job must then be delivered EXACTLY ONCE.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../../.."

PROJ=coldmail-send-managed; SCHEMA=send_managed; API=http://localhost:8080; MAILPIT=http://localhost:8025
PG="$PROJ-postgres-1"; CTL="$PROJ-control-1"; WRK="$PROJ-worker-1"
OUTAGE_SECS="${OUTAGE_SECS:-50}"
pass=0; failn=0
ok()  { echo "  PASS: $1"; pass=$((pass+1)); }
bad() { echo "  FAIL: $1"; failn=$((failn+1)); }
PSQL() { docker exec -i "$PG" psql -U postgres -d platform -tAq -c "$1" 2>/dev/null | tr -d '\r' || true; }
ADMKEY="$(grep -E '^ADMIN_API_KEY=' .env | cut -d= -f2-)"; PLTOK="$(grep -E '^PLATFORM_TOKEN=' .env | cut -d= -f2-)"
SYSUSER=00000000-0000-0000-0000-000000000000
mailpit_count() { curl -s "$MAILPIT/api/v1/messages" | grep -oE '"messages_count":[0-9]+' | cut -d: -f2 || true; }
snap() { PSQL "SELECT (SELECT count(*) FROM $SCHEMA.jobs)||' '||(SELECT count(*) FROM $SCHEMA.job_events)||' '||(SELECT count(*) FROM $SCHEMA.send_logs)||' '||COALESCE((SELECT max(seq) FROM $SCHEMA.job_events),0)"; }
LEAK='SQLSTATE|5432|send_managed_app|postgres|select |dial tcp|connection refused|pgx'
cleanup() { docker unpause "$WRK" >/dev/null 2>&1 || true; docker start "$PG" >/dev/null 2>&1 || true; }
trap cleanup EXIT

echo "== preconditions =="
tm="$(docker inspect -f '{{json .HostConfig.Tmpfs}}' "$PG")"
if echo "$tm" | grep -q 'postgresql/data'; then
  echo "REFUSING: $PG keeps its data on a tmpfs (DEV_RAM_PG=1). Stopping it would destroy the database and this drill would measure nothing. Run without DEV_RAM_PG." >&2; exit 2
fi
ok "Postgres is volume-backed (no tmpfs on its data directory): a stop/start keeps the data"
[ "$(docker inspect -f '{{.State.Running}}' "$CTL")" = true ] && [ "$(docker inspect -f '{{.State.Running}}' "$WRK")" = true ] || { bad "control and worker must both be running"; exit 1; }

echo "== fixtures (admin API, as an operator would) =="
RUNID="drill-$(date +%s)"
POOL="$(curl -s -X POST "$API/v1/admin/pools" -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d "{\"kind\":\"shared\",\"name\":\"$RUNID\"}" | grep -oE '"id":"[0-9a-f-]+"' | head -1 | cut -d'"' -f4 || true)"
[ -n "$POOL" ] || { bad "could not create a pool"; exit 1; }
curl -s -o /dev/null -X PUT "$API/v1/admin/config/default_shared_pool_id" -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d "{\"value\":\"$POOL\"}"
ACCT="$(curl -s -X POST "$API/v1/admin/accounts" -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d "{\"pool_id\":\"$POOL\",\"smtp_host\":\"smtp-sink\",\"smtp_port\":1025,\"smtp_username\":\"sender@ourdomain.com\",\"smtp_secret\":\"drill-secret\",\"from_address\":\"sender@ourdomain.com\",\"from_name\":\"Drill\"}" | grep -oE '"id":"[0-9a-f-]+"' | head -1 | cut -d'"' -f4 || true)"
[ -n "$ACCT" ] || { bad "could not create an account"; exit 1; }
new_item() { PSQL "INSERT INTO public.job_items (campaign_id,user_id,recipient_email,rendered_subject,rendered_body,reply_to,status) VALUES ('$SYSUSER','$SYSUSER','$1@example.com','Drill $1','<p>drill</p>','reply@ourdomain.com','pending') RETURNING id" | grep -vE '^(INSERT|UPDATE) [0-9]' | head -1; }
run_item() { curl -s -w '\n%{http_code}' -X POST "$API/v1/run" -H "Authorization: Bearer $PLTOK" -H 'content-type: application/json' -d "{\"job_item_id\":\"$1\",\"user_id\":\"$SYSUSER\"}"; }
job_of() { PSQL "SELECT id FROM $SCHEMA.jobs WHERE job_item_id='$1'" | head -1; }
state_of() { PSQL "SELECT state FROM $SCHEMA.jobs WHERE id='$1'"; }
wait_state() { local id="$1" want="$2" secs="$3" s; for _ in $(seq 1 "$secs"); do s="$(state_of "$id")"; [ "$s" = "$want" ] && return 0; sleep 1; done; return 1; }

echo "== baseline: the send path works before anything is broken =="
JIA="$(new_item "${RUNID}-a")"; RA="$(run_item "$JIA")"; JA="$(echo "$RA" | head -1 | grep -oE '"job_id":"[0-9a-f-]+"' | cut -d'"' -f4 || true)"
wait_state "$JA" sent 40 && ok "baseline job A reached sent" || { bad "baseline job A never reached sent (state: $(state_of "$JA"))"; exit 1; }

echo "== put a job genuinely IN FLIGHT: pause the worker, then run job B =="
docker pause "$WRK" >/dev/null
JIB="$(new_item "${RUNID}-b")"; RB="$(run_item "$JIB")"; JB="$(echo "$RB" | head -1 | grep -oE '"job_id":"[0-9a-f-]+"' | cut -d'"' -f4 || true)"
inflight=0; for _ in $(seq 1 40); do st="$(state_of "$JB")"; { [ "$st" = sending ] || [ "$st" = assigned ]; } && { inflight=1; break; }; sleep 1; done
[ "$inflight" = 1 ] && ok "job B is in flight (state=$(state_of "$JB")) on a frozen worker — not the easy case" || { bad "job B never got in flight (state: $(state_of "$JB"))"; exit 1; }
JIC="$(new_item "${RUNID}-c")"   # created BEFORE the outage; its run request will be made DURING it

S0="$(snap)"; M0="$(mailpit_count)"; R0="$(docker inspect -f '{{.RestartCount}}' "$CTL")"
read -r J0 E0 L0 SEQ0 <<<"$S0"
echo "== SNAPSHOT before the plug is pulled =="
echo "   jobs=$J0 job_events=$E0 send_logs=$L0 max(seq)=$SEQ0 | mailpit=$M0 | control RestartCount=$R0 | job B state=$(state_of "$JB")"

echo "== docker stop postgres (${OUTAGE_SECS}s outage) =="
T_STOP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
docker stop "$PG" >/dev/null
sleep "$((OUTAGE_SECS / 2))"

echo "== four things to check WHILE IT IS DOWN =="
[ "$(docker inspect -f '{{.State.Running}}' "$CTL")" = true ] && [ "$(docker inspect -f '{{.RestartCount}}' "$CTL")" = "$R0" ] \
  && ok "1. control is running with 0 new restarts (no crash-loop)" || bad "1. control is not running or restarted (RestartCount $(docker inspect -f '{{.RestartCount}}' "$CTL") vs $R0)"
curl -s -o /tmp/drill-health.$$ -w '%{http_code}' -H "Authorization: Bearer $ADMKEY" "$API/v1/admin/health" >/tmp/drill-health-code.$$ || true
HC="$(cat /tmp/drill-health-code.$$)"; HB="$(cat /tmp/drill-health.$$)"
RUNOUT="$(run_item "$JIC")"; RC="$(echo "$RUNOUT" | tail -1)"; RBODY="$(echo "$RUNOUT" | head -1)"
{ [ "$HC" -ge 400 ] 2>/dev/null && [ "$RC" -ge 400 ] 2>/dev/null; } && ok "3a. health ($HC) and run ($RC) FAIL CLOSED during the outage" || bad "3a. health=$HC run=$RC — expected both to refuse"
echo "$RBODY" | grep -q -E '"state":"queued"|"job_id"' && bad "3b. run returned a job it could not persist: $RBODY" || ok "3b. run did not hand back a queued job it could not persist"
if echo "$HB $RBODY" | grep -qiE "$LEAK"; then bad "4. an error envelope leaked internal detail: $HB $RBODY"; else ok "4. both error envelopes carry no SQLSTATE / DSN / table / driver detail"; fi
echo "   health body: $(echo "$HB" | cut -c1-140) | run body: $(echo "$RBODY" | cut -c1-140)"
sleep "$((OUTAGE_SECS - OUTAGE_SECS / 2))"
LOGS="$(docker logs --since "$T_STOP" "$CTL" 2>&1 || true)"
echo "$LOGS" | grep -q 'db outage DETECTED' && ok "2a. the log says: db outage DETECTED" || bad "2a. no 'db outage DETECTED' line in the control log"
NP="$(echo "$LOGS" | grep -c 'loops remain PAUSED, no state invented' || true)"
{ [ "${NP:-0}" -ge 1 ] && [ "${NP:-0}" -le 25 ]; } && ok "2b. 'loops remain PAUSED, no state invented' on a backoff, not a flood ($NP lines in ${OUTAGE_SECS}s)" || bad "2b. PAUSED lines = $NP (want 1..25)"
[ "$(mailpit_count)" = "$M0" ] && ok "5. nothing was delivered during the outage (mailpit $M0 -> $(mailpit_count))" || bad "5. mailpit count moved during the outage"

echo "== docker start postgres =="
docker start "$PG" >/dev/null
for _ in $(seq 1 90); do [ "$(docker inspect -f '{{.State.Health.Status}}' "$PG" 2>/dev/null)" = healthy ] && break; sleep 1; done
resolved=0; for _ in $(seq 1 120); do docker logs --since "$T_STOP" "$CTL" 2>&1 | grep -q 'db outage RESOLVED' && { resolved=1; break; }; sleep 1; done
[ "$resolved" = 1 ] && ok "6. the control logged: db outage RESOLVED — RESUMING loops from stored state" || bad "6. no 'db outage RESOLVED' line"
docker logs --since "$T_STOP" "$CTL" 2>&1 | grep 'db outage RESOLVED' | head -1 | cut -c1-170 | sed 's/^/   /'
[ "$(docker inspect -f '{{.RestartCount}}' "$CTL")" = "$R0" ] && ok "7. control RestartCount is still $R0 after recovery" || bad "7. control restarted during the drill"

echo "== recovery, with the worker STILL frozen: nothing may have been invented =="
sleep 10
S1="$(snap)"; read -r J1 E1 L1 SEQ1 <<<"$S1"
echo "   after : jobs=$J1 job_events=$E1 send_logs=$L1 max(seq)=$SEQ1   (before: jobs=$J0 job_events=$E0 send_logs=$L0 max(seq)=$SEQ0)"
[ "$J1" = "$J0" ] && ok "8. jobs_total unchanged $J0 -> $J1: no job was fabricated (the refused run created none)" || bad "8. jobs_total $J0 -> $J1"
OLDEV="$(PSQL "SELECT count(*) FROM $SCHEMA.job_events WHERE seq <= $SEQ0")"
[ "$OLDEV" = "$E0" ] && ok "9. every pre-outage event is still there, none added at or below the snapshot's max(seq) ($OLDEV = $E0)" || bad "9. events at/below max(seq) $SEQ0: $OLDEV, expected $E0"
[ -z "$(job_of "$JIC")" ] && ok "10. no job exists for the run request made during the outage" || bad "10. a job exists for the refused run: $(job_of "$JIC")"
SB="$(state_of "$JB")"; { [ "$SB" = sending ] || [ "$SB" = assigned ]; } && ok "11. job B is still $SB — the frozen worker reported nothing, so the control claimed nothing" || bad "11. job B is '$SB' with the worker frozen: an outcome nobody reported"
[ "$(mailpit_count)" = "$M0" ] && ok "12. still nothing delivered (mailpit $M0)" || bad "12. mailpit moved to $(mailpit_count) with the worker frozen"

echo "== release the worker; job B must be delivered EXACTLY ONCE =="
docker unpause "$WRK" >/dev/null
if wait_state "$JB" sent 180; then ok "13. job B reached sent after recovery"; else bad "13. job B did not reach sent in 180s (state: $(state_of "$JB"))"; fi
sleep 8
MB="$(mailpit_count)"; LB="$(PSQL "SELECT count(*) FROM $SCHEMA.send_logs WHERE job_id='$JB'")"
{ [ "$MB" = "$((M0 + 1))" ] && [ "$LB" = 1 ]; } && ok "14. exactly one delivery for B: mailpit $M0 -> $MB, send_logs rows for B = $LB (no double-processing)" || bad "14. mailpit $M0 -> $MB, send_logs for B = $LB (want +1 and 1)"
echo "   B's event chain: $(PSQL "SELECT string_agg(COALESCE(from_state::text,'-')||'>'||to_state||':'||actor, '  ' ORDER BY seq) FROM $SCHEMA.job_events WHERE job_id='$JB'")"
[ "$(snap | cut -d' ' -f1)" = "$J0" ] && ok "15. jobs_total is still $J0: recovery re-processed the existing job, it did not create another" || bad "15. jobs_total moved to $(snap | cut -d' ' -f1)"

echo "== intake resumed: the refused request, retried now, goes through once =="
RR="$(run_item "$JIC")"; RRC="$(echo "$RR" | tail -1)"; JC="$(echo "$RR" | head -1 | grep -oE '"job_id":"[0-9a-f-]+"' | cut -d'"' -f4 || true)"
{ [ "$RRC" = 202 ] && wait_state "$JC" sent 60; } && ok "16. the retried run returned 202 and the job reached sent" || bad "16. retried run: HTTP $RRC, state '$(state_of "$JC")'"
[ "$(mailpit_count)" = "$((M0 + 2))" ] && ok "17. mailpit is $M0 + 2 (B and C): one delivery each" || bad "17. mailpit $(mailpit_count), want $((M0 + 2))"

rm -f /tmp/drill-health.$$ /tmp/drill-health-code.$$
echo; echo "postgres_outage: $pass passed, $failn failed"
[ "$failn" -eq 0 ]
