#!/usr/bin/env bash
# phase12_text_part.sh — the optional PLAIN-TEXT PART, LIVE (Phase 3 close-out B3). Drives the running compose
# stack (real Control, real worker over real mTLS, real Postgres, real Mailpit as the SMTP sink), sourced by run.sh
# AFTER phase11_cancel.sh and reusing phase3_send.sh's $DC / $PSQL / $PSQL1 / $PLTOK / $ADMKEY / $SYSUSER / ok / bad
# and its pool + account fixtures. NEVER mocks (CLAUDE.md Rule 3).
#
# WHAT ONLY THE WHOLE PIPE CAN PROVE: the platform writes public.job_items.rendered_text, POST /v1/run copies it into
# send_managed.jobs.rendered_text, the dispatcher puts it on push_job.send.text, the worker builds the message, and
# the SMTP server (Mailpit) receives it. The assertions read the message Mailpit RECEIVED over SMTP (its /raw), not
# anything the module says about itself:
#
#   * a job_item with NO rendered_text (NULL)            -> the single-part text/html message, as before;
#   * a job_item with a whitespace-only rendered_text    -> the SAME single-part message (NULL, '' and blank are one meaning);
#   * a job_item WITH a rendered_text                    -> multipart/alternative, the text/plain part FIRST and the
#                                                           text/html part LAST, each with a charset and an encoding,
#                                                           a closing delimiter, the plain text and the HTML both there.
#
# Each present-case assertion has its absent-case twin in the same run, so a module that always sent multipart, or
# never did, or put the parts the wrong way round, is red here.
#
# Variation is left at its seeded defaults (all off) by phase8_variation.sh's restore; this suite asserts that
# precondition rather than assuming it.
#
# NOTE: sourced by run.sh (`set -euo pipefail`). Do not re-set shell options. Every command tolerates failure
# explicitly so `set -e` never kills the run mid-suite.

P12_API="http://localhost:8080"
P12_MAILPIT="http://localhost:8025"
P12_HTML_PRESENT='<p>html hello p12</p>'
P12_BOUNDARY_RE='----=_Alt_[0-9a-f]{24}'

# Precondition: every boolean variation toggle reads false (a toggle left ON would turn even the absent case into
# something other than the Phase 3 shape and make this suite assert nothing about the text part).
P12_TOGGLES_OK=1
for key in variation_html_minify variation_comment_injection variation_css_micro variation_unique_url_params \
           variation_zero_width variation_encoding_rotation variation_mime_boundary_rotation variation_dynamic_preheader \
           variation_user_agent_injection; do
  V="$(curl -s "$P12_API/v1/admin/config/$key" -H "Authorization: Bearer $ADMKEY" | grep -oE '"value":(true|false)' | cut -d: -f2 || true)"
  [[ "$V" == "false" ]] || P12_TOGGLES_OK=0
done
[[ "$P12_TOGGLES_OK" == "1" ]] && ok "P12: every variation toggle reads false before the plain-text suite (precondition)" || bad "P12: a variation toggle is ON — the plain-text suite's byte-shape assertions would be meaningless"

# p12_send <recipient> <rendered_text SQL expression>  -> echoes the job id once it is sent ("" on timeout)
p12_send() {
  local rcpt="$1" textexpr="$2" ji job st
  ji="$(PSQL1 "INSERT INTO public.job_items (campaign_id,user_id,recipient_email,rendered_subject,rendered_body,rendered_text,reply_to,status) VALUES ('00000000-0000-0000-0000-000000000000','00000000-0000-0000-0000-000000000000','${rcpt}','Phase 12 ${rcpt}','${P12_HTML_PRESENT}',${textexpr},'reply@ourdomain.com','pending') RETURNING id" || true)"
  job="$(curl -s -X POST "$P12_API/v1/run" -H "Authorization: Bearer $PLTOK" -H 'content-type: application/json' -d "{\"job_item_id\":\"$ji\",\"user_id\":\"$SYSUSER\"}" | grep -oE '"job_id":"[0-9a-f-]+"' | cut -d'"' -f4 || true)"
  for _ in $(seq 1 40); do
    st="$(PSQL "SELECT state FROM send_managed.jobs WHERE id='$job'" || true)"
    [[ "$st" == "sent" ]] && { echo "$job"; return 0; }
    sleep 1
  done
  echo ""
}

# p12_raw <recipient> -> the message Mailpit received for that recipient, CR stripped (CRLF on the wire)
p12_raw() {
  local id
  id="$(curl -s "$P12_MAILPIT/api/v1/messages?query=to:$1" | grep -oE '"ID":"[^"]+"' | head -1 | cut -d'"' -f4 || true)"
  [[ -n "$id" ]] && curl -s "$P12_MAILPIT/api/v1/message/$id/raw" | tr -d '\r'
}

# --- the absent twin: rendered_text IS NULL ------------------------------------------------------------------------
P12_JOB_NULL="$(p12_send p12-null@example.com "NULL")"
[[ -n "$P12_JOB_NULL" ]] && ok "P12: a job_item with rendered_text NULL was sent" || bad "P12: the NULL-text job never reached sent"
P12_RAW_NULL="$(p12_raw p12-null@example.com || true)"
if [[ -n "$P12_RAW_NULL" ]]; then
  N_CT="$(printf '%s\n' "$P12_RAW_NULL" | grep -c '^Content-Type:' || true)"
  if [[ "$N_CT" == "1" ]] && printf '%s\n' "$P12_RAW_NULL" | grep -q '^Content-Type: text/html; charset="utf-8"$' \
     && printf '%s\n' "$P12_RAW_NULL" | grep -q '^Content-Transfer-Encoding: 8bit$' \
     && ! printf '%s\n' "$P12_RAW_NULL" | grep -qiE 'multipart|text/plain'; then
    ok "P12: NULL rendered_text -> the delivered message is the single-part text/html message (no multipart, no text/plain)"
  else
    bad "P12: NULL rendered_text did not give the single-part text/html message (Content-Type lines: $N_CT) — RAW (cat -A): $(printf '%s\n' "$P12_RAW_NULL" | cat -A | head -30)"
  fi
else
  bad "P12: could not find the NULL-text message in Mailpit"
fi

# --- the other absent twin: whitespace-only (NULL, '' and blank are one meaning) ------------------------------------------
P12_JOB_BLANK="$(p12_send p12-blank@example.com "'   ' || chr(10) || '  '")"
[[ -n "$P12_JOB_BLANK" ]] && ok "P12: a job_item with a whitespace-only rendered_text was sent" || bad "P12: the blank-text job never reached sent"
P12_RAW_BLANK="$(p12_raw p12-blank@example.com || true)"
if [[ -n "$P12_RAW_BLANK" ]] && ! printf '%s\n' "$P12_RAW_BLANK" | grep -qiE 'multipart|text/plain' \
   && printf '%s\n' "$P12_RAW_BLANK" | grep -q '^Content-Type: text/html; charset="utf-8"$'; then
  ok "P12: a whitespace-only rendered_text -> the SAME single-part text/html message (no empty text/plain part)"
else
  bad "P12: a whitespace-only rendered_text produced a multipart or lost its Content-Type — RAW (cat -A): $(printf '%s\n' "$P12_RAW_BLANK" | cat -A | head -30)"
fi

# --- the present case ------------------------------------------------------------------------------------------------------
P12_JOB_TEXT="$(p12_send p12-text@example.com "'plain text hello p12' || chr(10) || 'second plain line'")"
[[ -n "$P12_JOB_TEXT" ]] && ok "P12: a job_item WITH rendered_text was sent" || bad "P12: the text-bearing job never reached sent"
P12_STORED="$(PSQL "SELECT rendered_text IS NOT NULL AND position('second plain line' in rendered_text) > 0 FROM send_managed.jobs WHERE id='$P12_JOB_TEXT'" || true)"
[[ "$P12_STORED" == "t" ]] && ok "P12: POST /v1/run copied rendered_text into send_managed.jobs" || bad "P12: send_managed.jobs.rendered_text was not populated by run (got '$P12_STORED')"
P12_RAW_TEXT="$(p12_raw p12-text@example.com || true)"
if [[ -z "$P12_RAW_TEXT" ]]; then
  bad "P12: could not find the text-bearing message in Mailpit"
else
  P12_B="$(printf '%s\n' "$P12_RAW_TEXT" | grep -oE "^Content-Type: multipart/alternative; boundary=\"${P12_BOUNDARY_RE}\"" | head -1 | sed -E 's/.*boundary="([^"]+)"/\1/' || true)"
  [[ -n "$P12_B" ]] && ok "P12: the delivered message is multipart/alternative with a deterministic-shaped boundary ($P12_B)" \
    || bad "P12: the delivered message is not multipart/alternative with a quoted ${P12_BOUNDARY_RE} boundary — RAW (cat -A): $(printf '%s\n' "$P12_RAW_TEXT" | cat -A | head -30)"
  if [[ -n "$P12_B" ]]; then
    L_TXT="$(printf '%s\n' "$P12_RAW_TEXT" | grep -n '^Content-Type: text/plain; charset="utf-8"$' | head -1 | cut -d: -f1 || true)"
    L_HTML="$(printf '%s\n' "$P12_RAW_TEXT" | grep -n '^Content-Type: text/html; charset="utf-8"$' | head -1 | cut -d: -f1 || true)"
    if [[ -n "$L_TXT" && -n "$L_HTML" && "$L_TXT" -lt "$L_HTML" ]]; then
      ok "P12: the text/plain part comes BEFORE the text/html part (RFC 2046: least to most faithful)"
    else
      bad "P12: part order wrong or a part missing: text/plain at line '${L_TXT:-none}', text/html at line '${L_HTML:-none}'"
    fi
    N_OPEN="$(printf '%s\n' "$P12_RAW_TEXT" | grep -cxF -- "--$P12_B" || true)"
    N_CLOSE="$(printf '%s\n' "$P12_RAW_TEXT" | grep -cxF -- "--$P12_B--" || true)"
    [[ "$N_OPEN" == "2" && "$N_CLOSE" == "1" ]] && ok "P12: two part delimiters and one closing delimiter" || bad "P12: delimiter lines: $N_OPEN opening (want 2), $N_CLOSE closing (want 1)"
    N_CTE="$(printf '%s\n' "$P12_RAW_TEXT" | grep -c '^Content-Transfer-Encoding: 8bit$' || true)"
    [[ "$N_CTE" == "2" ]] && ok "P12: each part states Content-Transfer-Encoding: 8bit (and the multipart itself states none)" || bad "P12: $N_CTE Content-Transfer-Encoding lines, want exactly 2 (one per part)"
    TXT_PART="$(printf '%s\n' "$P12_RAW_TEXT" | awk -v b="--$P12_B" 'BEGIN{n=0} $0==b{n++; next} n==1{print}')"
    HTML_PART="$(printf '%s\n' "$P12_RAW_TEXT" | awk -v b="--$P12_B" 'BEGIN{n=0} $0==b{n++; next} $0==b"--"{n=9} n==2{print}')"
    if printf '%s\n' "$TXT_PART" | grep -qx 'plain text hello p12' && printf '%s\n' "$TXT_PART" | grep -qx 'second plain line' && ! printf '%s\n' "$TXT_PART" | grep -q '<p>'; then
      ok "P12: the text/plain part carries the plain text (both lines) and no HTML"
    else
      bad "P12: the text/plain part is wrong: $(printf '%s\n' "$TXT_PART" | cat -A | head -12)"
    fi
    if printf '%s\n' "$HTML_PART" | grep -qxF "$P12_HTML_PRESENT" && ! printf '%s\n' "$HTML_PART" | grep -q 'plain text hello'; then
      ok "P12: the text/html part carries exactly the HTML body and none of the plain text"
    else
      bad "P12: the text/html part is wrong: $(printf '%s\n' "$HTML_PART" | cat -A | head -12)"
    fi
  fi
  # the message identity is untouched by the text part: the Control's Message-ID still carries the job id
  printf '%s\n' "$P12_RAW_TEXT" | grep -q "^Message-ID: <${P12_JOB_TEXT}@" && ok "P12: the Message-ID still carries the job id (the duplicate-guard identity is unchanged)" \
    || bad "P12: the Message-ID of the multipart message does not carry the job id"
fi

# --- the credential canary never appears in the multipart message either ---------------------------------------------
if printf '%s\n' "$P12_RAW_TEXT" | grep -qF "$CANARY"; then bad "P12: the SMTP credential canary appears in the delivered multipart message"; else ok "P12: the credential canary is absent from the delivered multipart message"; fi

# --- the platform reads back the same states, with no reference to text ---------------------------------------------------
P12_REP="$(curl -s "$P12_API/v1/report?job_id=$P12_JOB_TEXT&user_id=$SYSUSER" -H "Authorization: Bearer $PLTOK" || true)"
if echo "$P12_REP" | grep -q '"state":"sent"' && ! echo "$P12_REP" | grep -qiE 'rendered_text|plain text hello'; then
  ok "P12: the report of the multipart job is the ordinary sent report (no text content leaks into it)"
else
  bad "P12: the report of the multipart job is wrong: $P12_REP"
fi
P12_DESC="$(curl -s "$P12_API/v1/describe" -H "Authorization: Bearer $PLTOK" || true)"
echo "$P12_DESC" | grep -q '"plain_text_alternative":true' && ok "P12: GET /v1/describe advertises capabilities.plain_text_alternative=true" || bad "P12: describe does not advertise plain_text_alternative: $P12_DESC"
