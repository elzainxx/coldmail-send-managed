#!/usr/bin/env bash
# phase13_reconnect.sh — THE WORKER NEVER GIVES UP, LIVE (Phase 3 close-out B1). Drives the running compose stack
# (real Control, real worker over real mTLS, real Postgres, real Mailpit), sourced by run.sh LAST — after
# phase12_text_part.sh — reusing phase3_send.sh's $DC / $PSQL / $PSQL1 / $PLTOK / $ADMKEY / $SYSUSER / $WORKER_UUID /
# ok / bad and its pool + account fixtures. NEVER mocks (CLAUDE.md Rule 3). It runs on the CI runner only.
#
# THE CLAIM. Until close-out B1 the worker made about thirty one-second attempts to reach the Control and then EXITED
# (1) — an outage longer than that needed the worker restarted by hand. It now reconnects for ever. This suite proves
# it end to end, with the Control really DOWN for more than thirty attempts:
#
#   1. the dials are the SEEDED ones, delivered by the Control on register_ack: the suite sets worker_reconnect_base_ms
#      50, _cap_ms 200 and _jitter_pct 20 through the admin API (and first proves the minimum legal 10 is accepted and
#      9 refused), restarts the worker as a setup step, and requires the worker to REPORT those dials in effect — so
#      the Control -> register_ack -> worker path is exercised, not assumed. (Small dials are what let the outage run
#      past thirty attempts in seconds; with the production 1 s / 30 s it would take minutes.)
#   2. CONTROL (the stack with the Control up throughout): a job is sent and the worker's log carries NO
#      "reconnected after" line — a worker that is merely connected does not claim a recovery.
#   3. the Control is STOPPED. The worker's log must show at least 40 consecutive failed attempts (so > 30) and the
#      worker container must still be running with the SAME StartedAt it had before the outage: it did not give up
#      and it was not restarted.
#   4. the Control is started again. The worker must re-register on its own, its log must say it reconnected after
#      more than 30 failed attempts, and a NEW job must reach the sink (Mailpit) — with the same StartedAt, i.e. no
#      restart of the worker anywhere in the story.
#
# If the 30-attempt limit were reintroduced the worker would exit in step 3 (Running=false) and this suite fails
# there with the container's exit code; if the dials were not delivered it would see too few attempts and fail in
# step 3 with the count it did see.
#
# Restores the dials to their defaults and always leaves the Control running.
#
# NOTE: sourced by run.sh (`set -euo pipefail`). Do not re-set shell options. Every command tolerates failure
# explicitly so `set -e` never kills the run mid-suite.

P13_API="http://localhost:8080"
P13_MAILPIT="http://localhost:8025"

p13_wid() { $DC ps -q worker 2>/dev/null | head -1; }
p13_inspect() { docker inspect -f "$1" "$(p13_wid)" 2>/dev/null || true; }
p13_wlog() { $DC logs --since "$1" worker 2>&1 | tr -d '\r' || true; }
p13_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
p13_put() { # $1=key $2=value -> http code
  curl -s -o /dev/null -w '%{http_code}' -X PUT "$P13_API/v1/admin/config/$1" -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d "{\"value\":$2}" || true
}
p13_max_attempts() { # $1=since -> the highest "N consecutive failed attempt(s)" the worker has logged since then ("0" if none)
  local n
  n="$(p13_wlog "$1" | grep -oE '; [0-9]+ consecutive failed attempt' | grep -oE '[0-9]+' | sort -n | tail -1 || true)"
  echo "${n:-0}"
}
p13_wait_registered() { # $1=db clock taken BEFORE the event; up to $2 seconds
  local live
  for _ in $(seq 1 "${2:-60}"); do
    live="$(PSQL "SELECT status='active' AND last_heartbeat_at >= '$1'::timestamptz FROM send_managed.boxes WHERE id='${WORKER_UUID}'" || true)"
    [[ "$live" == "t" ]] && return 0
    sleep 1
  done
  return 1
}
p13_send_and_wait() { # $1=recipient -> 0 if the job reached sent
  local ji job st
  ji="$(PSQL1 "INSERT INTO public.job_items (campaign_id,user_id,recipient_email,rendered_subject,rendered_body,reply_to,status) VALUES ('00000000-0000-0000-0000-000000000000','00000000-0000-0000-0000-000000000000','$1','Phase 13 $1','<p>reconnect</p>','reply@ourdomain.com','pending') RETURNING id" || true)"
  job="$(curl -s -X POST "$P13_API/v1/run" -H "Authorization: Bearer $PLTOK" -H 'content-type: application/json' -d "{\"job_item_id\":\"$ji\",\"user_id\":\"$SYSUSER\"}" | grep -oE '"job_id":"[0-9a-f-]+"' | cut -d'"' -f4 || true)"
  for _ in $(seq 1 60); do
    st="$(PSQL "SELECT state FROM send_managed.jobs WHERE id='$job'" || true)"
    [[ "$st" == "sent" ]] && return 0
    sleep 1
  done
  return 1
}
# Messages in the sink addressed to $1, counted by recipient (not by the mailbox total, and independent of whether
# the list endpoint honours a query): the same technique phase11_cancel.sh uses.
p13_mail_count() { curl -s "$P13_MAILPIT/api/v1/messages?limit=500" | grep -o "\"Address\":\"$1\"" | wc -l | tr -d ' ' || true; }

# --- 1. the dials: the minimum legal value is accepted, one below it is refused, then the harness values --------------------
C_MIN="$(p13_put worker_reconnect_base_ms 10)"; C_BELOW="$(p13_put worker_reconnect_base_ms 9)"
[[ "$C_MIN" == "200" && "$C_BELOW" == "400" ]] && ok "P13: worker_reconnect_base_ms accepts the minimum legal value 10 (200) and refuses 9 (400)" \
  || bad "P13: base_ms 10 -> $C_MIN (want 200), 9 -> $C_BELOW (want 400)"
C_B="$(p13_put worker_reconnect_base_ms 50)"; C_C="$(p13_put worker_reconnect_cap_ms 200)"; C_J="$(p13_put worker_reconnect_jitter_pct 20)"
[[ "$C_B" == "200" && "$C_C" == "200" && "$C_J" == "200" ]] && ok "P13: set the harness dials through the admin API: base 50 ms, cap 200 ms, jitter 20 %" \
  || bad "P13: setting the dials answered base=$C_B cap=$C_C jitter=$C_J (want 200 each)"

# Setup step: restart the worker so its next register_ack carries the dials. (This restart is OUTSIDE the story below:
# the StartedAt that the no-restart assertions compare against is read AFTER it.)
P13_T_RESTART="$(PSQL1 "SELECT NOW()" || true)"; P13_S_RESTART="$(p13_now)"
$DC restart worker >/dev/null 2>&1 || true
if p13_wait_registered "$P13_T_RESTART" 60; then ok "P13: the worker re-registered after the setup restart"; else bad "P13: the worker did not re-register after the setup restart"; fi
P13_ADOPTED=0
for _ in $(seq 1 20); do
  if p13_wlog "$P13_S_RESTART" | grep -q 'reconnect backoff in effect: base 50 ms, cap 200 ms, jitter 20 %'; then P13_ADOPTED=1; break; fi
  sleep 1
done
[[ "$P13_ADOPTED" == "1" ]] && ok "P13: the worker reports the Control-delivered dials in effect (base 50 ms, cap 200 ms, jitter 20 %): register_ack -> worker path works" \
  || bad "P13: the worker never reported the delivered dials; its log: $(p13_wlog "$P13_S_RESTART" | tail -5)"

P13_STARTED="$(p13_inspect '{{.State.StartedAt}}')"
P13_RUNNING="$(p13_inspect '{{.State.Running}}')"
[[ -n "$P13_STARTED" && "$P13_RUNNING" == "true" ]] && ok "P13: baseline recorded: the worker is running, StartedAt $P13_STARTED" || bad "P13: cannot read the worker container's state (running='$P13_RUNNING')"

# --- 2. CONTROL: with the Control up throughout, a job is sent and no recovery is claimed ---------------------------------------
if p13_send_and_wait "p13-before@example.com"; then ok "P13 control: with the Control up, a job is sent"; else bad "P13 control: a job did not reach sent with the Control up"; fi
if p13_wlog "$P13_S_RESTART" | grep -q 'reconnected after'; then
  bad "P13 control: the worker logged a recovery ('reconnected after') although the Control was never down"
else
  ok "P13 control: no recovery line before the outage (a connected worker claims no reconnect)"
fi

# --- 3. the outage: the Control is DOWN; the worker keeps trying past thirty attempts and is not restarted ------------------------
P13_T_DOWN="$(p13_now)"
$DC stop control >/dev/null 2>&1 || true
P13_SEEN=0
for _ in $(seq 1 150); do
  P13_SEEN="$(p13_max_attempts "$P13_T_DOWN")"
  [[ "${P13_SEEN:-0}" -ge 40 ]] && break
  sleep 1
done
[[ "${P13_SEEN:-0}" -ge 40 ]] && ok "P13: with the Control stopped the worker logged $P13_SEEN consecutive failed attempts (> 30: the old limit) and kept going" \
  || bad "P13: with the Control stopped the worker logged only $P13_SEEN consecutive failed attempts in 150 s (want >= 40)"
P13_RUNNING_DURING="$(p13_inspect '{{.State.Running}}')"
P13_STARTED_DURING="$(p13_inspect '{{.State.StartedAt}}')"
[[ "$P13_RUNNING_DURING" == "true" && "$P13_STARTED_DURING" == "$P13_STARTED" ]] \
  && ok "P13: after more than thirty failed attempts the worker is STILL RUNNING and was not restarted (same StartedAt)" \
  || bad "P13: the worker gave up or was restarted during the outage: running=$P13_RUNNING_DURING StartedAt=$P13_STARTED_DURING (was $P13_STARTED) exit=$(p13_inspect '{{.State.ExitCode}}') — a reconnect limit is back"
if p13_wlog "$P13_T_DOWN" | grep -qE 'could not \(re\)register|exiting for restart'; then bad "P13: the worker logged a give-up line during the outage"; else ok "P13: no give-up line in the worker's log"; fi

# --- 4. recovery: the Control returns; the worker re-registers by itself and sends, with no restart ------------------------------
P13_T_UP="$(PSQL1 "SELECT NOW()" 2>/dev/null || true)"
P13_S_UP="$(p13_now)"
$DC start control >/dev/null 2>&1 || true
P13_READY=0
for _ in $(seq 1 120); do
  [[ "$(curl -s -o /dev/null -w '%{http_code}' "$P13_API/readyz" 2>/dev/null || true)" == "200" ]] && { P13_READY=1; break; }
  sleep 1
done
[[ "$P13_READY" == "1" ]] && ok "P13: the Control is back (readyz 200)" || bad "P13: the Control did not become ready again"
# the DB clock taken before the Control returned: a heartbeat newer than it proves a fresh registration
[[ -n "$P13_T_UP" ]] || P13_T_UP="$(PSQL1 "SELECT NOW() - interval '1 second'" || true)"
if p13_wait_registered "$P13_T_UP" 90; then ok "P13: the worker re-registered with the returned Control on its own (fresh heartbeat on its box row)"; else bad "P13: the worker did not re-register after the Control returned"; fi
P13_REC="$(p13_wlog "$P13_T_DOWN" | grep -oE 'reconnected after [0-9]+ failed attempt' | grep -oE '[0-9]+' | tail -1 || true)"
[[ "${P13_REC:-0}" -gt 30 ]] && ok "P13: the worker logged 'reconnected after $P13_REC failed attempt(s)' (> 30)" || bad "P13: no recovery line with more than 30 failed attempts (saw '${P13_REC:-none}')"
P13_BEFORE_MAIL="$(p13_mail_count p13-after@example.com)"
if p13_send_and_wait "p13-after@example.com"; then ok "P13: after the outage a NEW job reaches the sent state"; else bad "P13: a job submitted after the outage did not reach sent"; fi
P13_AFTER_MAIL="$(p13_mail_count p13-after@example.com)"
[[ "${P13_AFTER_MAIL:-0}" -gt "${P13_BEFORE_MAIL:-0}" ]] && ok "P13: the message reached the SMTP sink (Mailpit) through the worker that rode out the outage" || bad "P13: the sink got no message after the outage ($P13_BEFORE_MAIL -> $P13_AFTER_MAIL)"
P13_STARTED_END="$(p13_inspect '{{.State.StartedAt}}')"
[[ "$P13_STARTED_END" == "$P13_STARTED" && "$(p13_inspect '{{.State.Running}}')" == "true" ]] \
  && ok "P13: the worker was never restarted: the same container start ($P13_STARTED) before, during and after the outage" \
  || bad "P13: the worker's StartedAt changed ($P13_STARTED -> $P13_STARTED_END) or it is not running"

# --- restore: the seeded defaults, and the Control left running -----------------------------------------------------------
p13_put worker_reconnect_base_ms 1000 >/dev/null; p13_put worker_reconnect_cap_ms 30000 >/dev/null; p13_put worker_reconnect_jitter_pct 20 >/dev/null
[[ "$(curl -s -o /dev/null -w '%{http_code}' "$P13_API/readyz" 2>/dev/null || true)" == "200" ]] || $DC start control >/dev/null 2>&1 || true
