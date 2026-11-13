# Real-provider smoke test

> **STATUS: WRITTEN, NOT RUN.** Deliberately. Every test in this repo up to now sends into
> [Mailpit](http://localhost:8025), a local sink — nothing has ever left a machine. This procedure is
> the system's first contact with a real SMTP provider and a real inworker, and it touches **real
> sending reputation**. Running it is **step one of the pilot**, not part of the build. Do not run it
> to "check the docs are right."

The reason for the care: a bad first impression with a provider is not a test failure you re-run. An
account that authenticates wrong repeatedly, sends to an address that hard-bounces, or trips a
volume heuristic on day one carries that history forward. This procedure is shaped to make the first
send **small, to yourself, and reversible**.

Config keys and request shapes are not restated here — [`config-reference.md`](./config-reference.md)
and [`api-contract.md`](./api-contract.md).

---

## 1. Prerequisites

Do not start until all of these are true.

- [ ] The install guide has been completed end to end and a **Mailpit** send has succeeded.
      [`install-and-deploy.md`](./install-and-deploy.md). If it has not sent to a sink, it is not
      ready to send to a provider.
- [ ] The master key is **backed up off-host and the recovery drill has been run**.
      [`disaster-recovery.md`](./disaster-recovery.md) §1. You are about to enter a real credential;
      do not enter one into a system whose key you cannot restore.
- [ ] `jq` is installed, and you can read `GET /v1/admin/health`.
      [`operator-runbook.md`](./operator-runbook.md).
- [ ] You have a **dedicated throwaway or low-stakes sending account** at the provider — not the
      company's primary mailbox.
- [ ] You have **two recipient addresses you control**, at *different* providers (e.g. one Gmail, one
      Outlook). Different providers is the point: it is how you learn whether you landed in inworker or
      spam.
- [ ] You know the provider's real per-mailbox daily and per-hour limits, and the account's SPF/DKIM
      state for the sending domain.
- [ ] Someone other than you knows you are doing this, and when.

**Never** point a first smoke test at a purchased list, a customer list, or any address you do not
personally control. One hard bounce from a guessed address is a worse outcome than any information
this test could give you.

---

## 2. Pre-flight configuration

Set these before the first send. They are the difference between a controlled test and an incident.

| Setting | Why, for this test |
|---|---|
| `daily_limit` on the test account: **set it low** (e.g. 10) | A bug that loops cannot become a hundred messages. This is the single most valuable guard here. |
| `dispatch_enabled` | Leave `true`, but know exactly how to set it `false`. That is your stop button; keep the command in the terminal ready to paste. |
| `autoquarantine_mode` | Leave at its **shadow** default. You want to see what it *would* do without it acting on a first-contact anomaly. |
| Pacing (`send_delay_min_sec`/`send_delay_max_sec`) | Do not zero them. The default delay is the reputation-friendly behavior; the load test zeroes them only because it is measuring the Control. |
| `alert_webhook_url` + a health poll | Wire [`monitoring.md`](./monitoring.md) *first*. Alerts that only fire when the endpoint is polled are worth having during a first run. |
| SMTP port | 587 (STARTTLS) or 465 (implicit TLS). AUTH never precedes TLS; on 587/25 the session **aborts before AUTH** if the server does not advertise STARTTLS, and the credential is never sent. |

Record the starting state so you can tell what the test changed:

```bash
export ADMIN=http://127.0.0.1:8080
read -rs -p 'operator admin key: ' KEY; echo; export KEY   # not an assignment: keeps it out of ~/.bash_history
ahealth() { curl -fsS "$ADMIN/v1/admin/health" -H "Authorization: Bearer $KEY"; }

# Two captures on purpose: a wide one to read now, and a narrow SORTED one to diff against in §5.
# The diff baseline must use the SAME filter as the diff itself or it will never come back clean.
ahealth | jq '{dispatch, accounts, pools, workers, errors_last_window}' | tee /tmp/smoke-before-full.json
ahealth | jq -S '{accounts, pools}' > /tmp/smoke-before.json
```

---

## 3. The procedure

### Step 1 — Create the account with the real credential

`POST /v1/admin/accounts` with the provider's host, port, username, and `smtp_secret`, a low
`daily_limit`, and the pool you will send from. Exact body:
[`api-contract.md`](./api-contract.md).

Verify **only** that the response reports `has_credential: true`. That is by design the most you can
ever read back — the admin surface encrypts on write and holds no Decrypt capability, so there is no
route that will show you the secret again ([`security-posture.md`](./security-posture.md)).

**Expected:** `201`, `has_credential: true`. Nothing has authenticated to the provider yet — a
credential is only exercised at send time, so a wrong password looks fine at this step.

### Step 2 — Confirm eligibility before queueing anything

```bash
ahealth | jq '{pools, accounts, workers}'
```

**Expected:** the pool present with `headroom` **not** `exhausted`, the account counted as eligible,
at least one worker connected, `dispatch_paused: false`. If any of that is wrong, stop and use the
runbook — do not queue a job hoping it resolves.

### Step 3 — ONE message, to yourself

Queue exactly one job (`POST /v1/run` with `{job_item_id, user_id}`) whose single recipient is the
first address you control.

Then watch it, and give it a **hard time bound**: if it has not reached a terminal state within a few
minutes, set `dispatch_enabled=false` and diagnose before trying again. An unattended retry loop
against a real provider is the thing to avoid.

```bash
# Domain A platform token here — NOT the operator key. The two never share a credential.
curl -fsS "$PLATFORM/v1/report?user_id=$USER_ID&job_id=$JOB_ID" \
  -H "Authorization: Bearer $PLATFORM_TOKEN" | jq
```

**Expected:** `queued` → (briefly `assigned`/`sending`) → `sent`, with a `provider_message_id`
present. Note that `provider_message_id` and `completed_at` are **omitted entirely** until set — check
for key presence, not for `null`.

**Then check the actual inworker.** This is the step the whole procedure exists for, and no field in any
API can substitute for it:

- Did it arrive at all?
- **Inworker or spam?**
- Are SPF/DKIM/DMARC passing in the received headers?
- Is the From/display name what you intended?

A `sent` state means the provider returned `250` and nothing more. **V1 has no bounce, complaint,
open, or click data** — if this message later bounces, is filtered, or is silently dropped, this
system will never know. Your eyes on the inworker are the only feedback loop that exists.

### Step 4 — The second provider

Repeat step 3 with the address at the *other* provider. Different receivers apply very different
filtering, and one inworker placement tells you much less than two.

**Expected:** delivered at both, ideally both to inworker. Inworker at one and spam at the other is normal
for a new sending domain and is a *domain reputation* signal, not a defect in this module — the levers
that exist are in the runbook's deliverability section.

### Step 5 — Prove failure handling on purpose

Reputation-safe, and worth far more than another successful send, because it exercises the paths you
will actually need at 3am.

1. **Wrong credential → quarantine.** Temporarily `PATCH` the account with an incorrect
   `smtp_secret`, queue one job to your own address, and watch. **Expected:** the job does not send;
   the account is **quarantined immediately** on `auth_failure` (regardless of score), with an
   `account_events` row recording reason `auth_failure`. This costs one failed AUTH at the provider —
   acceptable once, deliberately. Then restore the correct secret and unquarantine.
2. **Daily limit.** Set `daily_limit` to 1, send one message, then queue a second. **Expected:** the
   second sits in `waiting_for_capacity` and the pool reads `exhausted`. Confirm via `/v1/report` that
   it reads as clean pending `queued` — **never** `failed` — and that it resumes at the next daily
   reset. This is the single most misread state in the system; seeing it once here is worth the five
   minutes.
3. **Worker death.** Stop a worker process mid-run. **Expected:** the job is reclaimed after
   `job_max_processing_time_sec`, `attempt_count` increments (saturating), the job returns to clean
   `queued`, and it goes out on another worker. Nothing is marked `sent` that the worker did not confirm.

Do **not** test a bounce by sending to an invalid address. That is exactly the reputation damage this
procedure is designed to avoid, and V1 would not record the bounce anyway.

### Step 6 — A small real batch

Only after steps 1–5 pass. Ten to twenty messages to addresses you control, watching the rate and the
error counts:

```bash
ahealth | jq '{rate: .dispatch, errors: .errors_last_window, accounts, pools}'
```

**Expected:** every job terminal as `sent`, `errors_last_window` empty or benign, no unexpected pause
or quarantine, observed rate consistent with the configured pacing (not faster). A rate *higher* than
pacing should allow means something is misconfigured — investigate before scaling.

---

## 4. Stop conditions

Set `dispatch_enabled=false` immediately, and do not resume until you understand why, if:

- More than one account quarantines.
- Any message reaches an address you did not intend.
- The observed send rate exceeds what pacing should allow.
- The provider returns throttling responses (visible as `throttle`-class errors and self-clearing
  pauses).
- A job reaches `sent` without a `provider_message_id`, or a state transition you cannot explain.
- Anything at all surprises you. This is the one run where "I'll look at it after" is the wrong call.

`dispatch_enabled=false` is a clean halt: zero job loss, fully resumable, queued jobs simply wait.
Using it costs nothing.

---

## 5. After the test

- [ ] Restore `daily_limit` to its intended production value.
- [ ] Confirm the account is not left paused or quarantined from step 5:
      `ahealth | jq '{accounts, pools}'`.
- [ ] Confirm the correct credential is in place (step 5.1) — re-`PATCH` if there is any doubt, and
      prove it with one more successful send.
- [ ] Diff against the recorded baseline — same filter as the capture in §2, or the diff is noise:
      `diff /tmp/smoke-before.json <(ahealth | jq -S '{accounts, pools}')` (expect only the intended
      new account and its pool).
- [ ] Review `send_managed.account_events` for the account — every lifecycle change during the test is
      recorded there, and it is the honest log of what happened.
- [ ] Record the result: date, provider, ports used, inworker-vs-spam per receiver, anything surprising.
      Append it here or alongside [`load-test-results.md`](./load-test-results.md); a first-contact
      result that nobody wrote down has to be re-earned.
- [ ] Work through [`go-live-checklist.md`](./go-live-checklist.md) before widening beyond addresses
      you control.

## 6. Related

| Doc | For |
|---|---|
| [`install-and-deploy.md`](./install-and-deploy.md) | Everything that must work before this. |
| [`operator-runbook.md`](./operator-runbook.md) | Diagnosing anything that goes wrong here. |
| [`capacity-planning.md`](./capacity-planning.md) | Sizing after the smoke test passes. |
| [`disaster-recovery.md`](./disaster-recovery.md) | Key backup — a prerequisite, not a follow-up. |
| [`go-live-checklist.md`](./go-live-checklist.md) | The gate between this test and real volume. |
