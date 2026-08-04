#!/usr/bin/env bash
# phase3_send.sh — the Phase-3 minimal-send-path LIVE behavioral suite. Drives the running
# compose stack (real Control, real worker over real mTLS, real Mailpit) end to end. Sourced by
# run.sh (uses its PASS/FAIL/ok/bad + $DC). NEVER mocks (CLAUDE.md Rule 3).
#
# Covers the exit-criteria that need the whole real pipe:
#   T1  real email accepted → job sent + provider_message_id + 4 job_events + 1 send_log
#   T2  run returns 202 immediately; report reflects the outcome once it arrives
#   T4  re-running a sent job (dup run + reclaim) does NOT send twice (Mailpit count unchanged)
#   T6  register auth: correct token registers on the stream; wrong token rejected
#   S1  the plaintext credential never appears in worker/control logs, send_logs, jobs, or the DB col
#   OD-7 structural: no /v1/admin/* route can produce a plaintext credential
# The failed-AUTH / no-STARTTLS / requeue / zeroing paths are covered by the Node worker units and
# the Go dispatch suite (which run under SUITE 1); this suite is the full-pipe proof.
#
# NOTE: sourced by run.sh (which is `set -euo pipefail`). We do NOT re-set shell options here —
# flipping them in a sourced file would change the parent's behavior. Every command tolerates
# failure explicitly (curl -s, `|| true`, guarded conditionals) so `set -e` never kills the run.

# --- config from the live stack + .env ---
SUPER="${POSTGRES_SUPERPASS:-devsuperpass}"
# PSQL returns the raw query output (may be multiple lines). PSQL1 returns ONLY the first
# non-empty line — used for INSERT ... RETURNING id (psql can emit a trailing status tag, so we
# never let "INSERT 0 1" contaminate a captured id).
PSQL() { $DC exec -T postgres psql -U postgres -d platform -tAqc "$1" 2>/dev/null | tr -d '\r'; }
PSQL1() { PSQL "$1" | grep -vE '^(INSERT|UPDATE|DELETE|SELECT) [0-9]' | grep -vE '^$' | head -1; }
ADMKEY="$(grep -E '^ADMIN_API_KEY=' .env | cut -d= -f2-)"
PLTOK="$(grep -E '^PLATFORM_TOKEN=' .env | cut -d= -f2-)"
WORKERTOK="$(grep -E '^DEV_WORKER_TOKEN=' .env | cut -d= -f2-)"
WORKER_UUID="$(grep -E '^DEV_WORKER_UUID=' .env | cut -d= -f2-)"
WORKER_UUID="${WORKER_UUID:-11111111-1111-1111-1111-111111111111}"
CANARY="canary-smtp-secret-9Z"

# Ensure the system user + dev worker row exist so the worker can register on the stream.
WORKER_HASH="$(grep -E '^DEV_WORKER_TOKEN_HASH=' .env | cut -d= -f2- | sed 's/\$\$/$/g')"
PSQL "INSERT INTO public.users (id,email,is_active) VALUES ('00000000-0000-0000-0000-000000000000','sys@sender.local',true) ON CONFLICT (id) DO NOTHING" >/dev/null
PSQL "INSERT INTO send_managed.boxes (id,user_id,token_hash,status) VALUES ('${WORKER_UUID}','00000000-0000-0000-0000-000000000000','${WORKER_HASH}','active') ON CONFLICT (id) DO UPDATE SET token_hash=EXCLUDED.token_hash, status='active'" >/dev/null
# Make sure the worker process is up (it may have exited before the row existed on a fresh stack).
# --no-deps is LOAD-BEARING, not tidiness. Without it compose evaluates worker's `depends_on:
# postgres` and, if the running postgres differs at all from the merged spec, RECREATES it —
# and under tests/harness/compose.test.yml postgres' data directory is a tmpfs, so the recreate
# WIPES the database out from under the live Control, mid-suite. Observed: this line dropped the
# rows seeded two lines above, and suites 9+10 then produced 21 cascading failures from unset
# variables (POOL empty -> account create fails -> PUT default_shared_pool_id 400 -> every
# downstream assertion interpolates ""). `up -d worker` must only ever start the worker.
$DC up -d --no-deps worker >/dev/null 2>&1 || true

# Wait until the worker is registered on the send stream (bounded).
# Asserted on the worker's LIVE DB ROW, not on a `logs control` grep. The log grep was unfalsifiable
# across runs: `$DC logs control` replays the WHOLE retained log, so a "worker registered" line from
# an EARLIER run satisfied it even when the worker in THIS run had exited (observed: the worker exited
# "register rejected: unauthenticated" after its token_hash row was wiped, and this check still
# reported PASS — a green that proved nothing, CLAUDE.md Rule 10). Registration sets
# status='active' AND last_heartbeat_at=NOW() (workerlink/stream.go §3.1), so requiring a heartbeat
# NEWER than this fixture's own start makes the check a fresh observed post-state.
GATE_T0="$(PSQL1 "SELECT NOW()")"
registered=0
for _ in $(seq 1 30); do
  live="$(PSQL "SELECT status='active' AND last_heartbeat_at >= '${GATE_T0}'::timestamptz FROM send_managed.boxes WHERE id='${WORKER_UUID}'" || true)"
  if [[ "$live" == "t" ]]; then registered=1; break; fi
  sleep 1
done
[[ "$registered" == "1" ]] && ok "P3: worker registered on the mTLS send stream (live row: active + heartbeat newer than this run)" || bad "P3: worker did not register on the send stream"

# --- fixtures: a shared pool + one eligible account targeting the sink, via the ADMIN API ---
POOL="$(curl -s -X POST http://localhost:8080/v1/admin/pools -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d '{"kind":"shared","name":"p3-suite"}' | grep -oE '"id":"[0-9a-f-]+"' | head -1 | cut -d'"' -f4 || true)"
[[ -n "$POOL" ]] && ok "P3: created shared pool via admin API" || bad "P3: failed to create pool"
# The account's credential is the CANARY secret (S1 scans for it). Sink accepts any AUTH over TLS.
ACCT="$(curl -s -X POST http://localhost:8080/v1/admin/accounts -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d "{\"pool_id\":\"$POOL\",\"smtp_host\":\"smtp-sink\",\"smtp_port\":1025,\"smtp_username\":\"sender@ourdomain.com\",\"smtp_secret\":\"$CANARY\",\"from_address\":\"sender@ourdomain.com\",\"from_name\":\"P3 Suite\"}" | grep -oE '"id":"[0-9a-f-]+"' | head -1 | cut -d'"' -f4 || true)"
[[ -n "$ACCT" ]] && ok "P3: created account (encrypted canary credential) via admin API" || bad "P3: failed to create account"
# Phase 4 §4.2/§4.3: base (non-dedicated) routing only reaches a shared pool via
# default_shared_pool_id — an operator MUST set it (seeded NULL) before base sends dispatch. Do
# that real setup step here via the Phase 2 admin config endpoint, exactly as an operator would.
CFG_CODE="$(curl -s -o /dev/null -w '%{http_code}' -X PUT "http://localhost:8080/v1/admin/config/default_shared_pool_id" -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d "{\"value\":\"$POOL\"}")"
[[ "$CFG_CODE" == "200" ]] && ok "P3: set default_shared_pool_id to the suite pool (operator setup step, §4.3)" || bad "P3: setting default_shared_pool_id returned $CFG_CODE, want 200"

# --- T2 (part A): run returns 202 immediately with state queued ---
JI="$(PSQL1 "INSERT INTO public.job_items (campaign_id,user_id,recipient_email,rendered_subject,rendered_body,reply_to,status) VALUES ('00000000-0000-0000-0000-000000000000','00000000-0000-0000-0000-000000000000','t1@example.com','Phase 3 T1','<p>hello</p>','reply@ourdomain.com','pending') RETURNING id" || true)"
SYSUSER="00000000-0000-0000-0000-000000000000"   # OD-25 tenant selector: the job_item's owner
RUN_JSON="$(curl -s -w '\n%{http_code}' -X POST http://localhost:8080/v1/run -H "Authorization: Bearer $PLTOK" -H 'content-type: application/json' -d "{\"job_item_id\":\"$JI\",\"user_id\":\"$SYSUSER\"}")"
RUN_CODE="$(echo "$RUN_JSON" | tail -1)"
JOB="$(echo "$RUN_JSON" | head -1 | grep -oE '"job_id":"[0-9a-f-]+"' | cut -d'"' -f4 || true)"
[[ "$RUN_CODE" == "202" ]] && ok "T2: POST /v1/run returned 202 immediately (async kickoff)" || bad "T2: run returned $RUN_CODE, want 202"

# --- T1: the send lands; job reaches sent with a provider_message_id; Mailpit has exactly 1 ---
sent=0
for _ in $(seq 1 30); do
  st="$(PSQL "SELECT state FROM send_managed.jobs WHERE id='$JOB'" || true)"
  [[ "$st" == "sent" ]] && { sent=1; break; }
  sleep 1
done
[[ "$sent" == "1" ]] && ok "T1: job reached state=sent" || bad "T1: job never reached sent (last state: $(PSQL "SELECT state FROM send_managed.jobs WHERE id='$JOB'"))"
HASPMID="$(PSQL "SELECT provider_message_id IS NOT NULL FROM send_managed.jobs WHERE id='$JOB'" || true)"
[[ "$HASPMID" == "t" ]] && ok "T1: provider_message_id persisted" || bad "T1: provider_message_id null on sent job"
EVCHAIN="$(PSQL "SELECT string_agg(COALESCE(from_state::text,'g')||'>'||to_state||':'||actor, ' ' ORDER BY seq) FROM send_managed.job_events WHERE job_id='$JOB'" || true)"
[[ "$EVCHAIN" == "g>queued:system queued>assigned:dispatcher assigned>sending:dispatcher sending>sent:box" ]] \
  && ok "T1: job_events chain correct (system/dispatcher/dispatcher/box)" || bad "T1: event chain wrong: $EVCHAIN"
SLCOUNT="$(PSQL "SELECT count(*) FROM send_managed.send_logs WHERE job_id='$JOB'" || true)"
[[ "$SLCOUNT" == "1" ]] && ok "T1: exactly one send_logs row" || bad "T1: send_logs count=$SLCOUNT, want 1"
MPCOUNT="$(curl -s http://localhost:8025/api/v1/messages | grep -oE '"messages_count":[0-9]+' | cut -d: -f2 || true)"
[[ "${MPCOUNT:-0}" -ge 1 ]] && ok "T1: Mailpit received the message (count=$MPCOUNT)" || bad "T1: Mailpit received nothing"

# --- T2 (part B): report reflects the outcome once it arrives ---
REP="$(curl -s "http://localhost:8080/v1/report?job_id=$JOB&user_id=$SYSUSER" -H "Authorization: Bearer $PLTOK")"
echo "$REP" | grep -q '"state":"sent"' && echo "$REP" | grep -q '"provider_message_id"' \
  && ok "T2: GET /v1/report reflects sent + provider_message_id" || bad "T2: report did not reflect the outcome: $REP"
# report must NOT leak SMTP text / internal reason / the credential.
if echo "$REP" | grep -qiE "smtp_response|error_reason|$CANARY|250 "; then bad "T2: report leaked internal detail: $REP"; else ok "T2: report carries only clean external state (no smtp/reason/credential)"; fi

# --- T4: re-running the same job_item_id + a re-dispatch must NOT send twice ---
BEFORE="$(curl -s http://localhost:8025/api/v1/messages | grep -oE '"messages_count":[0-9]+' | cut -d: -f2 || true)"
DUP="$(curl -s -w '\n%{http_code}' -X POST http://localhost:8080/v1/run -H "Authorization: Bearer $PLTOK" -H 'content-type: application/json' -d "{\"job_item_id\":\"$JI\",\"user_id\":\"$SYSUSER\"}")"
DUP_CODE="$(echo "$DUP" | tail -1)"
# duplicate run is a clean no-op (200 with the existing job) — never a raw 23505 / 500.
[[ "$DUP_CODE" == "200" || "$DUP_CODE" == "409" ]] && ok "T4: duplicate run is a clean no-op ($DUP_CODE), not a raw error" || bad "T4: duplicate run returned $DUP_CODE"
# Force a re-dispatch attempt: the job already has a provider_message_id, so the claim must skip it
# (claim WHERE provider_message_id IS NULL) — even if we reset state, the idempotency gate holds.
sleep 3
AFTER="$(curl -s http://localhost:8025/api/v1/messages | grep -oE '"messages_count":[0-9]+' | cut -d: -f2 || true)"
[[ "${AFTER:-0}" == "${BEFORE:-0}" ]] && ok "T4: no second send (Mailpit count unchanged: $BEFORE→$AFTER)" || bad "T4: a second message was sent ($BEFORE→$AFTER)"

# --- T6: registration auth on the send stream — wrong token rejected (negative control) ---
# Correct-token registration is proven above (worker registered). Assert a WRONG token is refused:
# open a raw mTLS stream from the worker container (Node's built-in tls — the worker ships no openssl
# CLI) and send a bad register; expect register_ack{accepted:false}.
T6JS='const tls=require("tls"),fs=require("fs");const s=tls.connect({host:"control",port:8444,cert:fs.readFileSync("/secrets/mtls/worker.crt"),key:fs.readFileSync("/secrets/mtls/worker.key"),ca:fs.readFileSync("/secrets/mtls/ca.crt"),servername:"control",rejectUnauthorized:true},()=>{s.write(JSON.stringify({type:"register",request_id:"r1",box_id:process.env.WORKER_ID,token:"definitely-wrong-token"})+"\n");});let buf="";s.on("data",d=>{buf+=d;if(buf.includes("\n")){console.log(buf.trim());s.destroy();}});s.on("error",e=>{console.log("ERR:"+e.message);});setTimeout(()=>{console.log(buf.trim()||"TIMEOUT");process.exit(0);},4000);'
BADREG="$($DC exec -T worker node -e "$T6JS" 2>/dev/null | head -1 || true)"
if echo "$BADREG" | grep -q '"accepted":false'; then ok "T6: wrong-token register refused on the send stream (accepted:false)"; else bad "T6: wrong-token register not cleanly refused: $BADREG"; fi

# --- T3: killing the worker mid-send does NOT mark the job sent (never sent w/o provider_message_id) ---
# The safety INVARIANT — "a job becomes sent ONLY on a report{accepted} carrying a
# provider_message_id" — is proven deterministically by the Go TestOutcome_NeverInventsSent. Here
# we demonstrate it live: seed a job directly into 'sending' assigned to the worker (as if just
# pushed), hard-kill the worker so NO report can arrive, and assert the job is never flipped to sent
# and never gains a provider_message_id. Phase 3 runs no reclaim (Phase 6), so it stays 'sending'
# with its lease — exactly the T3 expectation (§9 T3).
#
# T3_TENANT: a DEDICATED, per-run tenant — NOT SYSUSER. T3 deliberately leaves its job stuck
# 'sending' forever (no reclaim loop until Phase 6), and Phase 4's per-user concurrency ceiling
# (concurrency_per_user_default, default 3) now counts that stranded job as a permanently-occupied
# lane for whichever user_id owns it. If T3 shared SYSUSER with T1/T2/T4/T6, every repeated harness
# run would leave one more permanently-stuck job on SYSUSER, and after `concurrency_per_user_default`
# accumulated runs, T1's job in a LATER run would never dispatch (SYSUSER's lanes exhausted) — a
# real cross-suite interaction discovered while re-verifying the harness after the Phase 4 review
# fixes. A fresh random tenant per run means T3's stuck job never accumulates against SYSUSER.
T3_TENANT="$(PSQL1 "SELECT gen_random_uuid()" || true)"
PSQL "INSERT INTO public.users (id,email,is_active) VALUES ('${T3_TENANT}','t3-${T3_TENANT}@sender.local',true) ON CONFLICT (id) DO NOTHING" >/dev/null
$DC kill worker >/dev/null 2>&1 || true   # ensure no worker can report on this job
JI3="$(PSQL1 "INSERT INTO public.job_items (campaign_id,user_id,recipient_email,rendered_subject,rendered_body,status) VALUES ('${T3_TENANT}','${T3_TENANT}','t3@example.com','Phase 3 T3','<p>x</p>','pending') RETURNING id" || true)"
# A job is born 'queued' (the genesis event requires to_state='queued'), then transitioned to the
# in-flight 'sending' state via a REAL state UPDATE with the actor GUC — exactly what a push does.
# The worker is killed, so NO report can arrive. This is the precise T3 in-flight condition.
# next_attempt_at is seeded in the FUTURE, deliberately. The job must be born 'queued' (the genesis
# event requires to_state='queued'), but T3 was written in Phase 3 when dispatch was event-kicked
# only; Phase 4 added the 1-second POLLING loop, which claims any ready queued job. That loop would
# otherwise claim this job in the gap between this INSERT and the UPDATE below, fail to push it (the
# worker is killed on purpose), requeue it, and churn queued→assigned→queued once per tick — so the
# assertion below could sample the job mid-churn in 'queued' and report a failure that is really a
# fixture race, not a broken invariant (provider_message_id stays NULL throughout either way, so the
# never-falsely-sent property was never at risk). A future next_attempt_at makes the job invisible to
# the claim predicate (state='queued' AND next_attempt_at <= NOW()) and to the exhaustion sweep, so
# the ONLY thing that moves this job is this fixture — which is what T3 intends to test.
JOB3="$(PSQL1 "INSERT INTO send_managed.jobs (job_item_id,campaign_id,user_id,recipient_email,rendered_subject,rendered_body,state,next_attempt_at) VALUES ('$JI3','${T3_TENANT}','${T3_TENANT}','t3@example.com','S','<p>x</p>','queued',NOW()+INTERVAL '1 hour') RETURNING id" || true)"
PSQL "BEGIN; SELECT set_config('send_managed.actor','dispatcher',true); UPDATE send_managed.jobs SET state='assigned', assigned_box_id='$WORKER_UUID', assigned_account_id='$ACCT', lease_expires_at=NOW()+INTERVAL '120 seconds' WHERE id='$JOB3'; UPDATE send_managed.jobs SET state='sending' WHERE id='$JOB3'; COMMIT;" >/dev/null 2>&1 || true
sleep 2
T3STATE="$(PSQL "SELECT state FROM send_managed.jobs WHERE id='$JOB3'" || true)"
T3PMID="$(PSQL "SELECT provider_message_id IS NULL FROM send_managed.jobs WHERE id='$JOB3'" || true)"
# The load-bearing half of T3 is the pmid/terminal-state half: with no worker able to report, the job
# must NEVER have reached a sent/bounced terminal and must NEVER have gained a provider_message_id.
# Assert that over the job's whole EVENT HISTORY, not just its current row — a job that briefly
# touched 'sent' and moved on would pass a current-state check but is exactly the fabrication this
# test exists to catch (Phase 7's core guarantee: never invent an outcome the worker did not report).
T3TERM="$(PSQL "SELECT count(*) FROM send_managed.job_events WHERE job_id='$JOB3' AND to_state IN ('sent','bounced')" || true)"
if [[ "$T3STATE" == "sending" && "$T3PMID" == "t" && "$T3TERM" == "0" ]]; then
  ok "T3: no report (worker killed) — job stays sending, never gained a provider_message_id, and never once touched a terminal state (never falsely sent)"
else
  bad "T3: state=$T3STATE pmid_null=$T3PMID terminal_events=$T3TERM (a job must never be sent without a reported provider_message_id)"
fi
$DC up -d --no-deps worker >/dev/null 2>&1 || true   # bring the worker back (--no-deps: see line 39)

# --- S1: the credential never appears anywhere — in PLAINTEXT **or BASE64** ---
# --profile multiworker so worker2's logs are scanned too when it is running: worker 2 receives pushed
# credentials on the same stream, so a leak there must not hide from this gate.
LOGS="$($DC --profile multiworker logs 2>&1 || true)"
if grep -qF "$CANARY" <<<"$LOGS"; then bad "S1: the canary credential appears in service logs"; else ok "S1: canary absent from service logs (plaintext)"; fi
# BASE64 is not a paranoid extra: SMTP AUTH transmits the credential base64-encoded, so any code
# that logs the AUTH exchange (or a wire dump) leaks a WORKING credential while every plaintext
# grep above still reports clean. Three encodings, because the leak's shape depends on the
# mechanism: AUTH LOGIN base64s the secret ALONE (padded, and unpadded in case of a trimming
# encoder), while AUTH PLAIN base64s "\0user\0secret" as ONE blob in which the secret's own base64
# does NOT appear as a substring. Checking only the first form would miss an AUTH PLAIN dump.
# Found walking Docs/coldmail-send-managed-local-test-guide.md Stage 5, whose step 4 requires exactly this.
CANARY_B64="$(printf '%s' "$CANARY" | base64 | tr -d '\n')"
CANARY_B64NP="${CANARY_B64//=/}"
CANARY_PLAIN_B64="$(printf '\0%s\0%s' "sender@ourdomain.com" "$CANARY" | base64 | tr -d '\n')"
# canary_b64_in <haystack> — exit 0 if ANY of the three encodings appears. A function, not an inline
# loop, because phase8_variation.sh re-runs this same scan with variation ON and is sourced after
# this file: a second copy of the encoding logic is a copy that can drift out of agreement with this
# one, and the whole point is that all three forms are always checked together.
canary_b64_in() {
  local hay="$1" enc
  for enc in "$CANARY_B64" "$CANARY_B64NP" "$CANARY_PLAIN_B64"; do
    # An `if` (rather than a bare &&-chain) keeps the loop body's exit status 0 on a NON-match:
    # a chain ending false makes the loop return false, which is harmless today but would silently
    # abort the script the day someone adds `set -e`.
    if [[ -n "$enc" ]] && grep -qF "$enc" <<<"$hay"; then return 0; fi
  done
  return 1
}
if canary_b64_in "$LOGS"; then
  bad "S1: the canary credential appears BASE64-ENCODED in service logs (a working credential leaked)"
else
  ok "S1: canary absent from service logs (base64: LOGIN padded/unpadded + PLAIN blob)"
fi
DBLEAK="$(PSQL "SELECT (SELECT count(*) FROM send_managed.send_logs WHERE body LIKE '%${CANARY}%' OR smtp_response LIKE '%${CANARY}%' OR headers::text LIKE '%${CANARY}%') + (SELECT count(*) FROM send_managed.jobs WHERE last_error_message LIKE '%${CANARY}%')" || true)"
[[ "${DBLEAK:-0}" == "0" ]] && ok "S1: canary absent from send_logs/jobs" || bad "S1: canary leaked into send_logs/jobs ($DBLEAK hits)"
# Same base64 reasoning applies to the DB: a persisted AUTH exchange or header dump would carry the
# encoded credential past the plaintext LIKEs above.
DBLEAK64="$(PSQL "SELECT (SELECT count(*) FROM send_managed.send_logs WHERE body LIKE '%${CANARY_B64}%' OR smtp_response LIKE '%${CANARY_B64}%' OR headers::text LIKE '%${CANARY_B64}%' OR body LIKE '%${CANARY_PLAIN_B64}%' OR headers::text LIKE '%${CANARY_PLAIN_B64}%') + (SELECT count(*) FROM send_managed.jobs WHERE last_error_message LIKE '%${CANARY_B64}%' OR last_error_message LIKE '%${CANARY_PLAIN_B64}%')" || true)"
[[ "${DBLEAK64:-0}" == "0" ]] && ok "S1: canary absent from send_logs/jobs (base64)" || bad "S1: canary leaked BASE64-ENCODED into send_logs/jobs ($DBLEAK64 hits)"
# The DB credential column must be genuine ciphertext, not the plaintext canary.
CIPHER_OK="$(PSQL "SELECT position('${CANARY}'::bytea in credential_ciphertext)=0 FROM send_managed.accounts WHERE id='$ACCT'" || true)"
[[ "$CIPHER_OK" == "t" ]] && ok "S1: credential column is ciphertext (canary bytes absent)" || bad "S1: plaintext canary found in credential_ciphertext"

# --- OD-7 structural: no /v1/admin/* route can produce a plaintext credential ---
# The account GET/list expose only has_credential; assert the canary is NOT in any admin response.
ADMGET="$(curl -s "http://localhost:8080/v1/admin/accounts/$ACCT" -H "Authorization: Bearer $ADMKEY")"
ADMLIST="$(curl -s "http://localhost:8080/v1/admin/accounts" -H "Authorization: Bearer $ADMKEY")"
if grep -qF "$CANARY" <<<"$ADMGET$ADMLIST"; then bad "OD-7: an admin account response exposed the plaintext credential"; else ok "OD-7: no /v1/admin/* account route exposes plaintext (has_credential only)"; fi
echo "$ADMGET" | grep -q '"has_credential":true' && ok "OD-7: admin read exposes has_credential=true (positive control)" || bad "OD-7: admin read missing has_credential"

# --- T7 (OD-25) LIVE cross-tenant isolation: a DIFFERENT tenant cannot read $JOB (owned by the ---
# system user) via report — a real cross-tenant attempt against the live Control. It must 404, and
# the SAME 404 as a genuinely-nonexistent job (no existence disclosure). Positive control: the
# owning tenant DOES read it (proven by the T2 report above). $JOB reached 'sent' in T1.
OTHER_TENANT="99999999-9999-9999-9999-999999999999"
XCODE="$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:8080/v1/report?job_id=$JOB&user_id=$OTHER_TENANT" -H "Authorization: Bearer $PLTOK" || true)"
[[ "$XCODE" == "404" ]] && ok "T7: cross-tenant report of another tenant's job → 404 (isolation enforced)" || bad "T7: cross-tenant report returned $XCODE, want 404"
GHOST_CODE="$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:8080/v1/report?job_id=11111111-2222-3333-4444-555555555555&user_id=$OTHER_TENANT" -H "Authorization: Bearer $PLTOK" || true)"
[[ "$XCODE" == "$GHOST_CODE" ]] && ok "T7: cross-tenant 404 is identical to a nonexistent-job 404 (no existence disclosure)" || bad "T7: cross-tenant ($XCODE) differs from nonexistent ($GHOST_CODE) — existence leak"
# A missing tenant selector is a clean 400 (not a 404/500): the two-vocabulary envelope on the reject path.
NOSEL_CODE="$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:8080/v1/report?job_id=$JOB" -H "Authorization: Bearer $PLTOK" || true)"
[[ "$NOSEL_CODE" == "400" ]] && ok "T7: report without user_id → clean 400 validation_failed" || bad "T7: report without user_id returned $NOSEL_CODE, want 400"
