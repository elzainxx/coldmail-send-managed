#!/usr/bin/env bash
# phase11_cancel.sh — POST /v1/cancel, LIVE (M2-G3). Drives the running compose stack (real Control, real worker
# over real mTLS, real Postgres, real Mailpit as the SMTP sink), sourced by run.sh AFTER phase8_variation.sh and
# reusing phase3_send.sh's $DC / $PSQL / $PSQL1 / $PLTOK / $WORKER_UUID / $CANARY / ok / bad and its pool + account
# fixtures (default_shared_pool_id still points at the suite pool). NEVER mocks (CLAUDE.md Rule 3).
#
# WHAT ONLY THE WHOLE REAL PIPE CAN PROVE, and how this suite makes each claim non-vacuous:
#
#   * "after a cancel NOTHING new is claimed or sent — the SINK's count stops while the in-flight items finish".
#     A campaign of 10 is run for a fresh tenant while the one worker is FROZEN (docker pause — the same cgroup-
#     freezer technique tests/harness/drills/postgres_outage.sh proved on a real runner), so exactly one item is
#     genuinely IN FLIGHT (`sending`, pushed to a worker that cannot report) and the rest are queued or churning
#     through the claim/requeue loop. The cancel lands in that state. The worker is then released: the in-flight
#     item must be delivered exactly once, and the sink's count for this campaign must then STOP — checked again
#     after a window longer than the dispatcher's tick and the pacing delay, which is what would let a leaked
#     queued item through if there were one.
#   * the cancelled items never reached the sink: the sink holds EXACTLY one message per item that ended `sent`
#     (counted by this campaign's own recipients, so an unrelated background send from an earlier suite cannot
#     perturb it), and every cancelled item has no send_logs row and no provider_message_id.
#   * the response is a partition of the campaign (cancelled + in_flight + already_terminal = total), a repeat
#     cancel of a settled campaign is a no-op that writes nothing, and a DIFFERENT tenant's cancel of the same
#     campaign id answers an empty 200.
#
# The assertions are written against the DATABASE's final state and against the response's own numbers, never
# against a particular interleaving of the claim loop: with one frozen worker the dispatcher legitimately churns
# claimed-but-unpushable items queued -> assigned -> queued once per tick (that is the pre-existing "no idle
# worker" requeue), so how many items are momentarily `assigned` when the cancel lands varies from run to run.
# Whatever that number is, the invariants below hold — and the churn is what exercises the cancel guard in the
# live system (an in-flight item that is requeued after the cancel must end `cancelled`, not `queued`).
#
# Total time the worker is frozen is kept well under job_max_processing_time_sec (120 s): there are no long
# sleeps between `docker pause` and `docker unpause`.
#
# NOTE: sourced by run.sh (`set -euo pipefail`). Do not re-set shell options. Every command tolerates failure
# explicitly so `set -e` never kills the run mid-suite, and the worker is ALWAYS unpaused before the suite ends.

C11_API="http://localhost:8080"
C11_MAILPIT="http://localhost:8025"
C11_WRK="coldmail-send-managed-worker-1"   # the container name the CI workflow and the outage drill already use
C11_N=10

# Messages in the sink addressed to THIS campaign's recipients (p11-<n>@example.com). Counted by recipient, not by
# the mailbox total: an earlier suite's job can legitimately still be draining into the same sink.
c11_mine_count() {
  local n
  n="$(curl -s "$C11_MAILPIT/api/v1/messages?limit=500" | grep -o '"Address":"p11-[0-9]*@example.com"' | wc -l | tr -d ' ')" || true
  echo "${n:-0}"
}
c11_state_count() { PSQL "SELECT count(*) FROM send_managed.jobs WHERE campaign_id='$1' AND state='$2'" || true; }
c11_inflight_count() { PSQL "SELECT count(*) FROM send_managed.jobs WHERE campaign_id='$1' AND state IN ('assigned','sending')" || true; }
c11_events_total() { PSQL "SELECT count(*) FROM send_managed.job_events e JOIN send_managed.jobs j ON j.id=e.job_id WHERE j.campaign_id='$1'" || true; }
c11_states() { PSQL "SELECT string_agg(state::text||'='||n::text, ' ' ORDER BY state::text) FROM (SELECT state, count(*) n FROM send_managed.jobs WHERE campaign_id='$1' GROUP BY state) s" || true; }
c11_json_int() { echo "$1" | grep -oE "\"$2\":[0-9]+" | head -1 | cut -d: -f2 || true; }
c11_cancel() { # $1=campaign $2=user -> "<body>\n<http code>"
  curl -s -w '\n%{http_code}' -X POST "$C11_API/v1/cancel" -H "Authorization: Bearer $PLTOK" -H 'content-type: application/json' \
    -d "{\"campaign_id\":\"$1\",\"user_id\":\"$2\"}" || true
}

# --- the worker must be registered and active before we freeze it -------------------------------------------------
c11_ready=0
st=""
for _ in $(seq 1 30); do
  st="$(PSQL "SELECT status FROM send_managed.boxes WHERE id='${WORKER_UUID}'" || true)"
  [[ "$st" == "active" ]] && { c11_ready=1; break; }
  sleep 1
done
[[ "$c11_ready" == "1" ]] && ok "P11: the worker is registered and active before the cancel campaign starts" || bad "P11: the worker is not active (status: ${st:-?}) — the live cancel suite cannot run"

# --- a fresh tenant and ONE campaign of 10 items -----------------------------------------------------------------
C11_TENANT="$(PSQL1 "SELECT gen_random_uuid()" || true)"
C11_OTHER="$(PSQL1 "SELECT gen_random_uuid()" || true)"
C11_CAMPAIGN="$(PSQL1 "SELECT gen_random_uuid()" || true)"
PSQL "INSERT INTO public.users (id,email,is_active) VALUES ('${C11_TENANT}','p11-${C11_TENANT}@sender.local',true),('${C11_OTHER}','p11o-${C11_OTHER}@sender.local',true) ON CONFLICT (id) DO NOTHING" >/dev/null
C11_ITEMS=()
for i in $(seq 1 "$C11_N"); do
  ji="$(PSQL1 "INSERT INTO public.job_items (campaign_id,user_id,recipient_email,rendered_subject,rendered_body,reply_to,status) VALUES ('${C11_CAMPAIGN}','${C11_TENANT}','p11-${i}@example.com','Cancel suite ${i}','<p>cancel ${i}</p>','reply@ourdomain.com','pending') RETURNING id" || true)"
  C11_ITEMS+=("$ji")
done
[[ "${#C11_ITEMS[@]}" == "$C11_N" && -n "${C11_ITEMS[0]}" ]] && ok "P11: seeded a campaign of $C11_N job_items for a fresh tenant" || bad "P11: could not seed the campaign (${#C11_ITEMS[@]} items)"

# --- freeze the worker, then run the whole campaign ---------------------------------------------------------------
C11_MINE_BASE="$(c11_mine_count)"
docker pause "$C11_WRK" >/dev/null 2>&1 || true
c11_run_bad=0
for ji in "${C11_ITEMS[@]}"; do
  code="$(curl -s -o /dev/null -w '%{http_code}' -X POST "$C11_API/v1/run" -H "Authorization: Bearer $PLTOK" -H 'content-type: application/json' -d "{\"job_item_id\":\"$ji\",\"user_id\":\"$C11_TENANT\"}" || true)"
  [[ "$code" == "202" ]] || c11_run_bad=$((c11_run_bad + 1))
done
[[ "$c11_run_bad" == "0" ]] && ok "P11: all $C11_N items accepted by /v1/run (202) while the worker is frozen" || bad "P11: $c11_run_bad of $C11_N runs were not 202"

# Wait until an item is genuinely in flight: pushed to the frozen worker (state=sending).
c11_sending=0
for _ in $(seq 1 60); do
  n="$(c11_state_count "$C11_CAMPAIGN" sending)"
  [[ "${n:-0}" -ge 1 ]] && { c11_sending=1; break; }
  sleep 1
done
[[ "$c11_sending" == "1" ]] && ok "P11: an item is genuinely IN FLIGHT (sending) on the frozen worker — the cancel lands on a campaign with work in both states" \
  || bad "P11: no item reached sending within 60s with the worker frozen (states: $(c11_states "$C11_CAMPAIGN"))"

# --- the cancel -------------------------------------------------------------------------------------------------------
C11_RESP="$(c11_cancel "$C11_CAMPAIGN" "$C11_TENANT")"
C11_CODE="$(echo "$C11_RESP" | tail -1)"
C11_BODY="$(echo "$C11_RESP" | head -1)"
[[ "$C11_CODE" == "200" ]] && ok "P11: POST /v1/cancel -> 200" || bad "P11: cancel returned $C11_CODE: $C11_BODY"
C11_CANCELLED="$(c11_json_int "$C11_BODY" cancelled)"; C11_INFLIGHT="$(c11_json_int "$C11_BODY" in_flight)"; C11_TERMINAL="$(c11_json_int "$C11_BODY" already_terminal)"
echo "$C11_BODY" | grep -q "\"campaign_id\":\"$C11_CAMPAIGN\"" && ok "P11: the response echoes the campaign_id" || bad "P11: no campaign_id in the response: $C11_BODY"
if [[ -n "$C11_CANCELLED" && -n "$C11_INFLIGHT" && -n "$C11_TERMINAL" && "$((C11_CANCELLED + C11_INFLIGHT + C11_TERMINAL))" == "$C11_N" ]]; then
  ok "P11: the response is a PARTITION of the campaign: cancelled=$C11_CANCELLED + in_flight=$C11_INFLIGHT + already_terminal=$C11_TERMINAL = $C11_N"
else
  bad "P11: cancelled(${C11_CANCELLED:-?}) + in_flight(${C11_INFLIGHT:-?}) + already_terminal(${C11_TERMINAL:-?}) != $C11_N: $C11_BODY"
fi
[[ "${C11_INFLIGHT:-0}" -ge 1 ]] && ok "P11: the cancel reported the in-flight item(s) as in_flight=$C11_INFLIGHT (not cancelled)" || bad "P11: in_flight=${C11_INFLIGHT:-?}, want >= 1 — the item on the frozen worker was not reported"

# Nothing is left queued: a queued item after a cancel is a claimable item after a cancel.
[[ "$(c11_state_count "$C11_CAMPAIGN" queued)" == "0" ]] && ok "P11: no item of the campaign is queued after the cancel" || bad "P11: $(c11_state_count "$C11_CAMPAIGN" queued) item(s) still queued after the cancel"
# The in-flight item is untouched in STATE and carries the cancel flag.
FLAGGED_SENDING="$(PSQL "SELECT count(*) FROM send_managed.jobs WHERE campaign_id='$C11_CAMPAIGN' AND state='sending' AND cancel_requested_at IS NOT NULL")"
[[ "${FLAGGED_SENDING:-0}" -ge 1 ]] && ok "P11: the in-flight item is still sending, flagged for cancellation (its attempt will finish; a failure would not be retried)" || bad "P11: no flagged sending item after the cancel"
# The sink saw nothing of this campaign while the worker was frozen.
C11_MINE_FROZEN="$(c11_mine_count)"
[[ "$C11_MINE_FROZEN" == "$C11_MINE_BASE" ]] && ok "P11: the sink received nothing of this campaign while the worker was frozen ($C11_MINE_BASE -> $C11_MINE_FROZEN)" || bad "P11: the sink received this campaign's mail while the worker was frozen ($C11_MINE_BASE -> $C11_MINE_FROZEN)"

# --- let the claim/requeue churn of the flagged `assigned` items settle (milliseconds per tick; bounded) -------------
for _ in $(seq 1 15); do
  [[ "$(c11_state_count "$C11_CAMPAIGN" assigned)" == "0" ]] && break
  sleep 1
done

# --- idempotent: once settled, a repeat cancel is a no-op that writes nothing and answers the same --------------------
C11_EV_MID="$(c11_events_total "$C11_CAMPAIGN")"
C11_RESP2="$(c11_cancel "$C11_CAMPAIGN" "$C11_TENANT")"
C11_RESP3="$(c11_cancel "$C11_CAMPAIGN" "$C11_TENANT")"
C11_BODY2="$(echo "$C11_RESP2" | head -1)"
[[ "$(echo "$C11_RESP2" | tail -1)" == "200" && "$C11_BODY2" == "$(echo "$C11_RESP3" | head -1)" ]] \
  && ok "P11: repeat cancels of the settled campaign answer 200 with the same body (idempotent)" || bad "P11: repeat cancels differ: 2nd=$C11_BODY2 3rd=$(echo "$C11_RESP3" | head -1)"
[[ "$(c11_json_int "$C11_BODY2" cancelled)" == "$(c11_state_count "$C11_CAMPAIGN" cancelled)" && "$(c11_json_int "$C11_BODY2" in_flight)" == "$(c11_inflight_count "$C11_CAMPAIGN")" ]] \
  && ok "P11: the repeat cancel's numbers match the database (cancelled=$(c11_json_int "$C11_BODY2" cancelled), in_flight=$(c11_json_int "$C11_BODY2" in_flight))" \
  || bad "P11: the repeat cancel's numbers do not match the database: $C11_BODY2 vs $(c11_states "$C11_CAMPAIGN")"
[[ "$(c11_events_total "$C11_CAMPAIGN")" == "$C11_EV_MID" ]] && ok "P11: the repeat cancels wrote no job_events row ($C11_EV_MID)" || bad "P11: the repeat cancels wrote job_events rows ($C11_EV_MID -> $(c11_events_total "$C11_CAMPAIGN"))"

# --- another tenant cannot cancel it -------------------------------------------------------------------------------
C11_XRESP="$(c11_cancel "$C11_CAMPAIGN" "$C11_OTHER")"
C11_XBODY="$(echo "$C11_XRESP" | head -1)"
[[ "$(echo "$C11_XRESP" | tail -1)" == "200" && "$(c11_json_int "$C11_XBODY" cancelled)" == "0" && "$(c11_json_int "$C11_XBODY" in_flight)" == "0" && "$(c11_json_int "$C11_XBODY" already_terminal)" == "0" ]] \
  && ok "P11: another tenant's cancel of this campaign id is an empty 200 {0,0,0} (no existence disclosure)" || bad "P11: cross-tenant cancel answered: $C11_XRESP"
[[ "$(c11_events_total "$C11_CAMPAIGN")" == "$C11_EV_MID" ]] && ok "P11: the cross-tenant cancel wrote nothing" || bad "P11: the cross-tenant cancel changed the campaign"

# --- release the worker: the in-flight item(s) finish, exactly once ------------------------------------------------
docker unpause "$C11_WRK" >/dev/null 2>&1 || true
c11_settled=0
for _ in $(seq 1 120); do
  n="$(c11_inflight_count "$C11_CAMPAIGN")"
  [[ "${n:-1}" == "0" ]] && { c11_settled=1; break; }
  sleep 1
done
[[ "$c11_settled" == "1" ]] && ok "P11: after the worker was released every in-flight item finished (none left assigned/sending)" \
  || bad "P11: items still in flight 120s after the worker was released ($(c11_inflight_count "$C11_CAMPAIGN"))"

C11_SENT="$(c11_state_count "$C11_CAMPAIGN" sent)"; C11_CANC="$(c11_state_count "$C11_CAMPAIGN" cancelled)"
C11_OTHERST="$(PSQL "SELECT count(*) FROM send_managed.jobs WHERE campaign_id='$C11_CAMPAIGN' AND state NOT IN ('sent','cancelled')")"
[[ "$((${C11_SENT:-0} + ${C11_CANC:-0}))" == "$C11_N" && "${C11_OTHERST:-1}" == "0" && "${C11_SENT:-0}" -ge 1 ]] \
  && ok "P11: the campaign settled: sent=$C11_SENT + cancelled=$C11_CANC = $C11_N, no other state, and at least the in-flight item was delivered" \
  || bad "P11: final states are not sent+cancelled=$C11_N (sent=${C11_SENT:-?} cancelled=${C11_CANC:-?} other=${C11_OTHERST:-?}: $(c11_states "$C11_CAMPAIGN"))"

# The sink: EXACTLY one message per sent item — no cancelled item reached it, nothing arrived twice.
sleep 3
C11_MINE_NOW="$(c11_mine_count)"
[[ "$((C11_MINE_NOW - C11_MINE_BASE))" == "${C11_SENT:-0}" ]] \
  && ok "P11: the sink holds exactly one message per sent item ($C11_MINE_BASE -> $C11_MINE_NOW = +$C11_SENT): no cancelled item was delivered, none twice" \
  || bad "P11: the sink's count for this campaign moved $C11_MINE_BASE -> $C11_MINE_NOW but $C11_SENT item(s) are sent"
LEAK="$(PSQL "SELECT count(*) FROM send_managed.jobs j WHERE j.campaign_id='$C11_CAMPAIGN' AND j.state='cancelled' AND (j.provider_message_id IS NOT NULL OR EXISTS (SELECT 1 FROM send_managed.send_logs s WHERE s.job_id=j.id))")"
[[ "${LEAK:-1}" == "0" ]] && ok "P11: no cancelled item has a provider_message_id or a send_logs row (nothing of it ever reached SMTP)" || bad "P11: $LEAK cancelled item(s) show evidence of a send"
BOTH="$(PSQL "SELECT count(*) FROM (SELECT e.job_id FROM send_managed.job_events e JOIN send_managed.jobs j ON j.id=e.job_id WHERE j.campaign_id='$C11_CAMPAIGN' GROUP BY e.job_id HAVING bool_or(e.to_state='cancelled') AND bool_or(e.to_state='sent')) x")"
[[ "${BOTH:-1}" == "0" ]] && ok "P11: no item has both a 'cancelled' and a 'sent' event" || bad "P11: $BOTH item(s) have BOTH a cancelled and a sent event"
SLMULTI="$(PSQL "SELECT count(*) FROM (SELECT s.job_id FROM send_managed.send_logs s JOIN send_managed.jobs j ON j.id=s.job_id WHERE j.campaign_id='$C11_CAMPAIGN' GROUP BY s.job_id HAVING count(*) > 1) x")"
[[ "${SLMULTI:-1}" == "0" ]] && ok "P11: every delivered item was reported exactly once (one send_logs row each)" || bad "P11: $SLMULTI item(s) have more than one send_logs row"

# --- the sink's count STOPS: a window longer than the tick + the pacing delay would reveal a leaked queued item ------
C11_STATES_A="$(c11_states "$C11_CAMPAIGN")"
sleep 12
C11_MINE_LATER="$(c11_mine_count)"
C11_STATES_B="$(c11_states "$C11_CAMPAIGN")"
[[ "$C11_MINE_LATER" == "$C11_MINE_NOW" && "$C11_STATES_A" == "$C11_STATES_B" ]] \
  && ok "P11: the sink's count STOPPED: unchanged after 12s more of dispatching (this campaign's mail $C11_MINE_NOW, states $C11_STATES_B)" \
  || bad "P11: something moved after the cancel settled: sink $C11_MINE_NOW -> $C11_MINE_LATER, states '$C11_STATES_A' -> '$C11_STATES_B'"

# --- what the platform reads back ------------------------------------------------------------------------------------
C11_REP="$(curl -s "$C11_API/v1/report?campaign_id=$C11_CAMPAIGN&user_id=$C11_TENANT&page_size=200" -H "Authorization: Bearer $PLTOK" || true)"
echo "$C11_REP" | grep -qE "\"cancelled\":${C11_CANC}[,}]" && echo "$C11_REP" | grep -qE "\"sent\":${C11_SENT}[,}]" \
  && ok "P11: the campaign report's totals say sent=$C11_SENT cancelled=$C11_CANC" || bad "P11: report totals do not match the database: $C11_REP"
echo "$C11_REP" | grep -q '"is_complete":true' && ok "P11: the campaign reads COMPLETE (cancelled is terminal)" || bad "P11: the campaign does not read complete: $C11_REP"
NREASON_C="$(echo "$C11_REP" | grep -o '"reason":"cancelled"' | wc -l | tr -d ' ' || true)"
NREASON_N="$(echo "$C11_REP" | grep -o '"reason":null' | wc -l | tr -d ' ' || true)"
[[ "$NREASON_C" == "$C11_CANC" && "$NREASON_N" == "$C11_SENT" ]] \
  && ok "P11: every cancelled item reports reason=cancelled ($NREASON_C) and every sent item reason=null ($NREASON_N)" \
  || bad "P11: item reasons do not match: cancelled-reasons=$NREASON_C (want $C11_CANC) null-reasons=$NREASON_N (want $C11_SENT)"
C11_SJOB="$(PSQL1 "SELECT id FROM send_managed.jobs WHERE campaign_id='$C11_CAMPAIGN' AND state='sent' LIMIT 1" || true)"
C11_SREP="$(curl -s "$C11_API/v1/report?job_id=$C11_SJOB&user_id=$C11_TENANT" -H "Authorization: Bearer $PLTOK" || true)"
echo "$C11_SREP" | grep -q '"reason":null' && echo "$C11_SREP" | grep -q '"error_class":null' && echo "$C11_SREP" | grep -q '"state":"sent"' \
  && ok "P11: the single-job report of a delivered item carries reason:null and error_class:null (keys present)" || bad "P11: single-job report of a sent item: $C11_SREP"
if echo "$C11_REP$C11_SREP" | grep -qiE "smtp_response|error_reason|250 |$CANARY"; then bad "P11: a report leaked SMTP text or the credential canary"; else ok "P11: the reports carry no SMTP text and no credential"; fi

# --- a cancelled item can never be re-run ------------------------------------------------------------------------------
C11_CJOB="$(PSQL1 "SELECT job_item_id FROM send_managed.jobs WHERE campaign_id='$C11_CAMPAIGN' AND state='cancelled' LIMIT 1" || true)"
if [[ -n "$C11_CJOB" ]]; then
  C11_JOBS_BEFORE="$(PSQL "SELECT count(*) FROM send_managed.jobs WHERE campaign_id='$C11_CAMPAIGN'")"
  C11_RERUN="$(curl -s -w '\n%{http_code}' -X POST "$C11_API/v1/run" -H "Authorization: Bearer $PLTOK" -H 'content-type: application/json' -d "{\"job_item_id\":\"$C11_CJOB\",\"user_id\":\"$C11_TENANT\"}" || true)"
  [[ "$(echo "$C11_RERUN" | tail -1)" == "200" ]] && echo "$C11_RERUN" | head -1 | grep -q '"state":"cancelled"' \
    && ok "P11: re-running a cancelled job_item answers the existing job, still cancelled (never re-queued)" || bad "P11: re-run of a cancelled item answered: $C11_RERUN"
  [[ "$(PSQL "SELECT count(*) FROM send_managed.jobs WHERE campaign_id='$C11_CAMPAIGN'")" == "$C11_JOBS_BEFORE" ]] \
    && ok "P11: the re-run created no job row" || bad "P11: the re-run changed the job count"
else
  bad "P11: no cancelled item to re-run (the campaign ended with none cancelled — the cancel did not stop anything)"
fi

# --- ALWAYS leave the worker running (idempotent: a no-op if it was already released above) ----------------------------
docker unpause "$C11_WRK" >/dev/null 2>&1 || true
