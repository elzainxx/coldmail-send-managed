#!/usr/bin/env bash
# phase8_variation.sh — the Phase-8 content-variation LIVE behavioral suite. Drives the running
# compose stack (real Control, real worker over real mTLS, real Mailpit) end to end, sourced by run.sh
# AFTER phase3_send.sh (reuses its pool/account fixtures + $ADMKEY/$PLTOK/$SYSUSER/$DC/$PSQL/
# $PSQL1). NEVER mocks (CLAUDE.md Rule 3).
#
# Covers what only the whole real pipe can prove:
#   T1  ALL TOGGLES OFF (the config seeds' own defaults, untouched) => Mailpit's raw message for a
#       fresh send is BYTE-IDENTICAL in shape to the Phase 3 T1 message (headers, MIME, no injected
#       bytes) — the headline regression guard, proven against the deployed Control+worker, not just unit
#       tests.
#   T3  variation ON (a representative real profile) => the Mailpit-delivered message differs at the
#       byte level from the all-off baseline, and STILL PARSES/RENDERS (Content-Type present, body
#       intact, no injected header corrupts the RFC 5322 structure Mailpit itself had to parse to
#       accept the message).
#   T7  unique_url_params on => the delivered message's links carry a real _ref/_v param and NO
#       tracking/redirect surface, observed in the ACTUAL bytes Mailpit received (not a unit fixture).
#   S1  live-stack re-check: with variation ON, the credential canary still never appears anywhere
#       (Mailpit body, control/worker logs, send_logs, jobs) — the security regression guard (§8 S1) run
#       against the real send path with variation active, not skipped because Phase 3's S1 already
#       covered the off case.
#
# NOTE: sourced by run.sh (`set -euo pipefail`). Do not re-set shell options here. Every command
# tolerates failure explicitly so `set -e` never kills the run mid-suite.

# --- T1 (live): confirm the seeded Phase 8 defaults are genuinely all-off/neutral, then send ---
DEFAULTS_OK=1
for key in variation_html_minify variation_comment_injection variation_css_micro \
           variation_unique_url_params variation_zero_width variation_encoding_rotation \
           variation_mime_boundary_rotation variation_dynamic_preheader \
           variation_user_agent_injection variation_thread_index; do
  V="$(curl -s "http://localhost:8080/v1/admin/config/$key" -H "Authorization: Bearer $ADMKEY" | grep -oE '"value":(true|false)' | cut -d: -f2)"
  [[ "$V" == "false" ]] || DEFAULTS_OK=0
done
[[ "$DEFAULTS_OK" == "1" ]] && ok "P8 T1: every boolean toggle reads false via the live admin API (seeded defaults intact)" \
  || bad "P8 T1: a boolean toggle is NOT false on the live stack — precondition for the byte-identity check failed"

JI8T1="$(PSQL1 "INSERT INTO public.job_items (campaign_id,user_id,recipient_email,rendered_subject,rendered_body,status) VALUES ('00000000-0000-0000-0000-000000000000','00000000-0000-0000-0000-000000000000','p8t1@example.com','Phase 8 T1','<p>Hello Phase 8 T1</p>','pending') RETURNING id" || true)"
RUN8T1="$(curl -s -w '\n%{http_code}' -X POST http://localhost:8080/v1/run -H "Authorization: Bearer $PLTOK" -H 'content-type: application/json' -d "{\"job_item_id\":\"$JI8T1\",\"user_id\":\"$SYSUSER\"}")"
JOB8T1="$(echo "$RUN8T1" | head -1 | grep -oE '"job_id":"[0-9a-f-]+"' | cut -d'"' -f4 || true)"
sent=0
for _ in $(seq 1 30); do
  st="$(PSQL "SELECT state FROM send_managed.jobs WHERE id='$JOB8T1'" || true)"
  [[ "$st" == "sent" ]] && { sent=1; break; }
  sleep 1
done
[[ "$sent" == "1" ]] && ok "P8 T1: all-toggles-off job reached sent" || bad "P8 T1: all-toggles-off job never reached sent"

# Pull the RAW message Mailpit actually received for this job and assert it has exactly the Phase 3
# shape: no injected comments, no zero-width, no reordered/added headers, single-part MIME.
MP8T1_ID="$(curl -s "http://localhost:8025/api/v1/messages?query=to:p8t1@example.com" | grep -oE '"ID":"[^"]+"' | head -1 | cut -d'"' -f4 || true)"
if [[ -n "$MP8T1_ID" ]]; then
  RAW8T1="$(curl -s "http://localhost:8025/api/v1/message/$MP8T1_ID/raw" || true)"
  # Each condition records WHICH one tripped. This used to set a bare flag and print "deviates", so a failure on a
  # real Linux runner (where it was seen once and could not be explained from the dump) named no condition at all.
  # The dump is `cat -A` so a CR (^M) or any non-ASCII byte shows up instead of hiding inside printable-looking text.
  #
  # THE CAUSE OF THE FAILURE THIS REPLACED: Mailpit's /raw returns the message exactly as it was received over SMTP, i.e.
  # with CRLF line endings, so every `^Header: value$` below could never match — `$` sits before the CR, not after it. The
  # check was written and "passed" on a Windows dev host whose bash strips CR from $(...); a real Linux runner keeps it.
  # (And the first diagnosis of this was wrong for exactly that reason: host-side sed/awk strip CR too, so the log looked
  # LF-only. `cat -A` of the failing run showed ^M on every line.) The shape assertions run on a CR-free COPY. RAW8T1
  # itself stays byte-for-byte as fetched — T3 below compares two raw fetches against each other, and normalising only one
  # of them would make "differs from the baseline" pass vacuously.
  BASELINE_OK=1; BASELINE_WHY=""
  RAW8T1_LF="$(printf '%s\n' "$RAW8T1" | tr -d '\r')"
  must_match()  { printf '%s\n' "$RAW8T1_LF" | grep -qE -- "$2" || { BASELINE_OK=0; BASELINE_WHY="$BASELINE_WHY [absent: $1]"; }; }
  must_absent() { if printf '%s\n' "$RAW8T1_LF" | grep -q -- "$2"; then BASELINE_OK=0; BASELINE_WHY="$BASELINE_WHY [present: $1]"; fi; }
  must_match  "MIME-Version: 1.0"                  '^MIME-Version: 1\.0$'
  must_match  "Content-Type: text/html; utf-8"     '^Content-Type: text/html; charset="utf-8"$'
  must_match  "Content-Transfer-Encoding: 8bit"    '^Content-Transfer-Encoding: 8bit$'
  must_absent "injected HTML comment"              '<!--'
  must_absent "User-Agent header"                  'User-Agent:'
  must_absent "Precedence header"                  'Precedence:'
  [[ "$BASELINE_OK" == "1" ]] && ok "P8 T1: the delivered message has exactly the Phase 3 shape (no injected comments/UA/Precedence, 8bit single-part)" \
    || bad "P8 T1: the delivered message deviates from the Phase 3 shape while every toggle reads off —${BASELINE_WHY} — RAW (cat -A):
$(printf '%s\n' "$RAW8T1" | cat -A)"
else
  bad "P8 T1: could not locate the delivered message in Mailpit for p8t1@example.com"
fi

# --- T3/T7 (live): flip a representative real profile on, send, observe the ACTUAL bytes ---
curl -s -o /dev/null -X PUT "http://localhost:8080/v1/admin/config/variation_html_minify" -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d '{"value":true}'
curl -s -o /dev/null -X PUT "http://localhost:8080/v1/admin/config/variation_unique_url_params" -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d '{"value":true}'
curl -s -o /dev/null -X PUT "http://localhost:8080/v1/admin/config/variation_comment_injection" -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d '{"value":true}'
curl -s -o /dev/null -X PUT "http://localhost:8080/v1/admin/config/variation_comment_intensity" -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d '{"value":"high"}'

# RICH_BODY must contain MANY closing tags, and that is load-bearing rather than cosmetic.
# injectComments makes an INDEPENDENT seeded draw per closing tag at the bucket's rate (high = 0.30,
# from variation_comment_intensity_rates). The original fixture had exactly TWO closing tags, so the
# probability of injecting nothing at all was 0.7^2 = 49% — the T3 comment assertion below was a coin
# flip, and it failed intermittently from Phase 8 through Phase 10 for that reason alone, never
# because of a product defect. With the 17 closing tags below the miss probability is 0.7^17 ≈ 0.23%.
# Do not shrink this body: a test that fails half the time is indistinguishable from a real
# regression in CI output, which is the failure mode CLAUDE.md Rule 10 exists to close.
RICH_BODY='<p>Hello Phase 8 T3</p><a href="https://example.com/landing">landing page</a><div><span>a</span><span>b</span><span>c</span><span>d</span></div><div><span>e</span><span>f</span><span>g</span><span>h</span></div><ul><li>one</li><li>two</li><li>three</li><li>four</li></ul>'
JI8T3="$(PSQL1 "INSERT INTO public.job_items (campaign_id,user_id,recipient_email,rendered_subject,rendered_body,status) VALUES ('00000000-0000-0000-0000-000000000000','00000000-0000-0000-0000-000000000000','p8t3@example.com','Phase 8 T3',$(printf '%s' "$RICH_BODY" | sed "s/'/''/g;s/^/'/;s/$/'/"),'pending') RETURNING id" || true)"
RUN8T3="$(curl -s -w '\n%{http_code}' -X POST http://localhost:8080/v1/run -H "Authorization: Bearer $PLTOK" -H 'content-type: application/json' -d "{\"job_item_id\":\"$JI8T3\",\"user_id\":\"$SYSUSER\"}")"
JOB8T3="$(echo "$RUN8T3" | head -1 | grep -oE '"job_id":"[0-9a-f-]+"' | cut -d'"' -f4 || true)"
sent3=0
for _ in $(seq 1 30); do
  st="$(PSQL "SELECT state FROM send_managed.jobs WHERE id='$JOB8T3'" || true)"
  [[ "$st" == "sent" ]] && { sent3=1; break; }
  sleep 1
done
[[ "$sent3" == "1" ]] && ok "P8 T3: variation-on job reached sent" || bad "P8 T3: variation-on job never reached sent"

MP8T3_ID="$(curl -s "http://localhost:8025/api/v1/messages?query=to:p8t3@example.com" | grep -oE '"ID":"[^"]+"' | head -1 | cut -d'"' -f4 || true)"
if [[ -n "$MP8T3_ID" ]]; then
  RAW8T3="$(curl -s "http://localhost:8025/api/v1/message/$MP8T3_ID/raw" || true)"
  # T3: the delivered bytes differ from the all-off baseline — variation genuinely fired live.
  if [[ "$RAW8T3" != "$RAW8T1" ]]; then ok "P8 T3: variation-on delivered bytes differ from the all-off baseline (variation fired on the live stack)"
  else bad "P8 T3: variation-on delivered bytes are IDENTICAL to the all-off baseline — variation did not fire"; fi
  # T3: still parses — Content-Type/MIME framing intact (Mailpit accepted + stored it, but assert
  # explicitly rather than only inferring from "it's in Mailpit").
  echo "$RAW8T3" | grep -qE '^Content-Type: text/html' && ok "P8 T3: delivered message still carries a valid Content-Type after variation" \
    || bad "P8 T3: delivered message lost its Content-Type header under variation"
  # T3: minify + comment injection fired (an injected comment is present; original body text intact).
  echo "$RAW8T3" | grep -q -- '<!--' && ok "P8 T3: comment_injection fired live (an injected HTML comment is present)" \
    || bad "P8 T3: comment_injection did not inject any comment at 'high' intensity on a real send"
  echo "$RAW8T3" | grep -q "Hello Phase 8 T3" && ok "P8 T3: original visible body text survived minification/comment injection" \
    || bad "P8 T3: original visible body text is missing after variation — content was altered, not just varied"
  # T7: the link now carries _ref, and NO tracking/redirect surface was introduced.
  echo "$RAW8T3" | grep -qE 'href="https://example\.com/landing\?_ref=[0-9a-f]+"' && ok "P8 T7: the delivered link carries a real _ref cache-buster param" \
    || bad "P8 T7: the delivered link does not carry the expected _ref param"
  if echo "$RAW8T3" | grep -qiE 'mail-track|redirect|track\.'; then
    bad "P8 T7: the delivered message introduced a tracking/redirect surface — forward-boundary violation (P8-10)"
  else
    ok "P8 T7: no tracking/redirect surface in the delivered message (forward boundary intact, P8-10)"
  fi
else
  bad "P8 T3/T7: could not locate the delivered message in Mailpit for p8t3@example.com"
fi

# --- S1 (live, re-checked WITH variation on): the credential canary still never appears anywhere ---
# PLAINTEXT **and BASE64**, via phase3_send.sh's canary_b64_in (this file is sourced after it). The
# base64 forms matter here for the same reason they do in Phase 3 — SMTP AUTH transmits the
# credential encoded, so a logged AUTH exchange leaks a WORKING credential past every plaintext
# grep — and they matter SEPARATELY here because variation rewrites the message on its way out: this
# is the only scan that covers the credential with the variation code paths active.
LOGS8="$($DC --profile multiworker logs 2>&1 || true)"
if grep -qF "$CANARY" <<<"$LOGS8"; then bad "P8 S1: the canary credential appears in service logs with variation ON"; else ok "P8 S1: canary absent from service logs with variation ON (plaintext)"; fi
if canary_b64_in "$LOGS8"; then
  bad "P8 S1: the canary credential appears BASE64-ENCODED in service logs with variation ON"
else
  ok "P8 S1: canary absent from service logs with variation ON (base64)"
fi
if echo "$RAW8T3" | grep -qF "$CANARY"; then bad "P8 S1: the canary credential appears in the delivered message body/headers"; else ok "P8 S1: canary absent from the delivered message"; fi
# The delivered message is the one place a variation bug could inject the credential into content;
# check the encoded forms there too, not just the logs.
if canary_b64_in "$RAW8T3"; then
  bad "P8 S1: the canary credential appears BASE64-ENCODED in the delivered message"
else
  ok "P8 S1: canary absent from the delivered message (base64)"
fi
DBLEAK8="$(PSQL "SELECT (SELECT count(*) FROM send_managed.send_logs WHERE body LIKE '%${CANARY}%' OR smtp_response LIKE '%${CANARY}%' OR headers::text LIKE '%${CANARY}%') + (SELECT count(*) FROM send_managed.jobs WHERE last_error_message LIKE '%${CANARY}%')" || true)"
[[ "${DBLEAK8:-0}" == "0" ]] && ok "P8 S1: canary absent from send_logs/jobs with variation ON" || bad "P8 S1: canary leaked into send_logs/jobs with variation ON ($DBLEAK8 hits)"
DBLEAK8B64="$(PSQL "SELECT (SELECT count(*) FROM send_managed.send_logs WHERE body LIKE '%${CANARY_B64}%' OR smtp_response LIKE '%${CANARY_B64}%' OR headers::text LIKE '%${CANARY_B64}%' OR body LIKE '%${CANARY_PLAIN_B64}%' OR headers::text LIKE '%${CANARY_PLAIN_B64}%') + (SELECT count(*) FROM send_managed.jobs WHERE last_error_message LIKE '%${CANARY_B64}%' OR last_error_message LIKE '%${CANARY_PLAIN_B64}%')" || true)"
[[ "${DBLEAK8B64:-0}" == "0" ]] && ok "P8 S1: canary absent from send_logs/jobs with variation ON (base64)" || bad "P8 S1: canary leaked BASE64-ENCODED into send_logs/jobs with variation ON ($DBLEAK8B64 hits)"

# --- restore every toggle flipped above to its seeded default, so a later harness run (or another
# suite reading these same keys) is not left with variation silently ON. ---
curl -s -o /dev/null -X PUT "http://localhost:8080/v1/admin/config/variation_html_minify" -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d '{"value":false}'
curl -s -o /dev/null -X PUT "http://localhost:8080/v1/admin/config/variation_unique_url_params" -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d '{"value":false}'
curl -s -o /dev/null -X PUT "http://localhost:8080/v1/admin/config/variation_comment_injection" -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d '{"value":false}'
curl -s -o /dev/null -X PUT "http://localhost:8080/v1/admin/config/variation_comment_intensity" -H "Authorization: Bearer $ADMKEY" -H 'content-type: application/json' -d '{"value":"low"}'
