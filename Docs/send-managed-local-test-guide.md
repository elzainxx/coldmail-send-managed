# SendManaged module — local pre-deploy test guide

Everything here runs on your PC against the docker stack. No VPS, no real SMTP provider, no reputation at risk.

**The good news:** almost the entire system is verifiable locally. Routing, pacing, the full error taxonomy, worker death, Control restart, database outage, pool exhaustion, the panic switch, shadow mode, credential security, key rotation — all of it. What local testing *cannot* prove is a short list, and it is spelled out in Part 4.

Treat this as the gate: nothing deploys until every stage below is green.

---

## Part 1 — What you need on the PC

### Software
- **Docker Desktop** (with Compose) — the whole stack runs in containers
- **Git Bash or WSL** — `scripts/dev.sh` is a shell script; `make` isn't on the Windows host, which is why the Makefile delegates
- **Go toolchain** — to run the Go suites directly (dispatch, adminapi, platformapi, workerproto)
- **Node** — to run the worker unit suite directly
- **`psql` client** — you will be inspecting the database constantly. The container's psql works if you'd rather not install one.
- **`curl`** or an HTTP client (Postman, Insomnia, `httpie`) for the admin and platform APIs
- **`jq`** — optional, but makes reading API responses far less painful

### Resources
- **~4 GB free RAM** — Postgres, Control, 2 worker containers, Mailpit
- **~10 GB free disk** — images plus volumes across repeated clean-slate resets
- Ports free: Postgres, Control API, Mailpit UI + SMTP (check your compose for the exact set)

### Setup
- A **local webhook receiver** for alert testing — a container running a request-logger, or [webhook.site](https://webhook.site) if the container can reach the internet
- **A scratch copy of the local KMS key**, stored outside the repo — you need this for the restore drill in Stage 11
- **A second worker.** Multi-worker routing, worker death, and reclaim all need one to mean anything. `scripts/dev.sh certs` provisions it and `scripts/dev.sh reset` seeds it; start it with **`scripts/dev.sh up2`**. It is a distinct `worker2` compose service behind the `multiworker` profile, so it stays off until you ask for it.
  - **Do not use `docker compose up --scale worker=2`.** Worker identity is the authorization subject: scaled replicas share one `WORKER_ID`/`WORKER_TOKEN`, and the Control closes the previous connection whenever an id re-registers (the correct reconnect rule). Two same-id replicas evict each other in a permanent register/disconnect loop, `send_managed.boxes` still shows one row, and no work is distributed. The two workers intentionally **share** the dev mTLS cert — the cert is transport, the token is the authority.

---

## Part 2 — Local gotchas

Read these first; they will each cost you an hour otherwise.

- **Host antivirus intercepts host→localhost TLS.** This is a known environment quirk, not a defect. mTLS must be probed **in-network** (container-to-container), not from the Windows host. If an mTLS check fails from the host, re-run it from inside a container before believing it.
- **`dev.sh reset` means reset.** Clean-slate tests need a genuinely fresh volume — a stale volume will silently skip migrations and give you a false pass on Stage 2.
- **A reset drops the worker rows, and `--wait` will lie to you about it.** `reset` is `down -v`, which destroys the volume holding `send_managed.boxes` — so a worker has nothing to authorize against, is refused with `unauthenticated`, burns its retry budget and **exits 1**. `up --wait` reports it *healthy* seconds before it dies, so a scripted run sees a green light on a dead worker. `reset` now re-seeds the worker rows itself (postgres → migrate → seed → the rest); if you drive compose by hand, run `scripts/dev.sh seed` after migrations and before the worker. Always confirm with `docker compose ps -a` (**`-a`** — a plain `ps` hides an exited container).
- **Dev and CI use the same compose** (base + harness override). If something passes locally but fails in CI, suspect the override, not the code.
- **Always drive compose through `scripts/dev.sh`, or pass BOTH `-f` files — and never restart postgres mid-stage.** Two separate traps with the same symptom (your fixtures silently vanish, and `migrate` logs `applied 22 migration(s)` on what you thought was a warm database):
  - The harness override backs postgres's data directory with **`tmpfs`** (RAM). It is not a volume — the data does not survive the container being recreated *at all*, and there is nothing to inspect afterwards.
  - `scripts/dev.sh` always passes `-f docker-compose.yml -f tests/harness/compose.test.yml`. A bare `docker compose ...` typed by hand uses only the base file, where postgres is **volume**-backed with different flags. The two invocations describe *different* containers, so alternating between them **recreates postgres and discards its data**.
  - So: a bare `docker compose up -d --force-recreate --wait control` to pick up a rebuilt Control will take postgres with it if the previous `up` came from `dev.sh`. Rebuild through `dev.sh`, or add `--no-deps`, and re-check your fixtures (`SELECT count(*) FROM send_managed.jobs`) before trusting a result. Anything you must keep across a restart belongs in a re-runnable seed step, not in the database only.
- **Timing tests need a quiet machine.** Stage 8 measures actual throughput. Don't run it while a build is churning — you'll chase a pacing bug that's really CPU contention.
- **One host, one log stream.** Since the Control and both workers run on the same machine, tail them together during Stage 9. Watching a heartbeat miss, the reclaim, and the re-dispatch scroll past in one place makes failure tests dramatically easier to read.

---

## Part 3 — The test sequence

### Stage 0 — Environment sanity

1. `scripts/dev.sh reset` — tear down completely (including volumes), then bring the stack back up. This re-seeds the worker rows in the correct order (postgres → migrate → seed → rest); without that, the worker cannot register.
2. `scripts/dev.sh up2` — start the second worker (`multiworker` profile; needed from Stage 8 onward)
3. `docker compose ... ps -a` — confirm every container is up and **nothing has exited**. Use `-a`: a plain `ps` omits exited containers, which is exactly how a dead worker hides.
4. Confirm both workers registered: `SELECT id, status FROM send_managed.boxes` shows **two** `active` rows, and the Control logged `worker registered id=...` exactly twice with no `stream closed` churn
5. Open the Mailpit UI; confirm it is reachable and empty
6. Connect with `psql`; confirm the `send_managed` schema exists (owner `send_managed_owner`) and `send_managed.config` holds **53** rows

**Pass:** every container up and stable, both workers `active`, Mailpit empty, schema present.

---

### Stage 1 — Automated suite

> **READ THIS BEFORE STEP 1 — run the harness from a clean slate, and read the failure COUNT sceptically.**
>
> Two traps, both hit on the real walkthrough of this stage, both now fixed in the harness itself —
> but the reading habit they teach is still yours to keep.
>
> **1. A large failure count is usually ONE root cause.** The first run here reported
> `HARNESS RESULT: 35 passed, 23 failed`. It was not 23 defects; it was **two**. Suites 9 and 10
> contributed 21 cascading failures from a single empty variable: `POOL` came back empty, so the
> account create failed, so `PUT default_shared_pool_id` returned 400, and every downstream
> assertion interpolated `""`. The tell is in the output — `job never reached sent (last state: )`
> and `send_logs count=, want 1` have nothing between the `:`/`=` and the next token. Two more
> tells: **self-contradicting pairs** (`FAIL: S1: plaintext canary found in credential_ciphertext`
> printed right beside a PASS of the same property). When you see either, stop counting and find
> the first failure in file order; fix that and re-run before triaging anything below it.
>
> **2. Starting one container can destroy the database.** The root cause above was
> `$DC up -d worker` **without `--no-deps`** in `tests/harness/phase3_send.sh`. Compose then evaluates
> worker's `depends_on: postgres`, and if the running postgres differs at all from the merged spec it
> **recreates** it — and under `tests/harness/compose.test.yml` postgres' data directory is a
> **tmpfs**, so the recreate wipes the DB out from under the live Control, mid-suite. It dropped the
> fixture rows the script had seeded two lines earlier. Any `up`/`run` against the harness compose
> files needs `--no-deps` unless you actually intend to touch the stack.
>
> That wipe was survivable for a long time because of trap 2b: the "worker registered" gate greped
> `$DC logs control`, which replays the **whole retained log**, so a registration line from an
> *earlier* run satisfied it while the worker in *this* run had already exited
> `register rejected: unauthenticated` (its `token_hash` row having just been wiped). It reported
> **PASS while the worker was dead** — a green that could not go red (Rule 10). The gate now asserts a
> live DB post-state: `status='active' AND last_heartbeat_at >=` the fixture's own start time.
>
> **Start from `scripts/dev.sh reset`**, and confirm postgres' age (`docker ps`) afterwards. A
> postgres container younger than the harness run means something recreated it mid-run.

1. `scripts/dev.sh test` — the full harness (**58** checks; the count is `ok()`+`bad()` in
   `tests/harness/run.sh`, printed as the `HARNESS RESULT` line. This said 53 until 2026-07-29 —
   don't take the number on faith, read the line the run prints)
2. Run the Go suites directly. `cd services/control && go test ./...` covers all of them at once — dispatch, adminapi, platformapi, credential, workerlink, workerproto — which is safer than naming them, since a named list silently misses a new package.
3. Run the worker Node suite (`cd services/worker && node --test "test/*.test.mjs"` — **78** tests)
4. Run all 6 CI gates locally: `migration_lint`, `config_drift`, `no_secret_leak`, `contract_drift`, `config_consumption`, `drain_grace`
5. `gofmt` / `go vet` clean

**Pass:** every suite green, every gate green, **zero skipped tests**. A skip here is a test you think you have and don't.

---

### Stage 2 — Clean-slate reproduction (the docs test)

This is the one stage where you deliberately ignore what you know.

1. `scripts/dev.sh reset`
2. Follow the **written install doc**, top to bottom, using only what it says
3. Get to a real send landing in Mailpit
4. **Time it.** Write the number down.

**Pass:** you reach a delivered message without deviating from the doc once. Every place you had to rely on memory or improvise is a docs bug — fix the doc, reset, repeat until it runs clean.

**Observed (2026-07-29, PASS on the fourth pass).** Four resets, because the loop in the pass condition is the point: passes 1–3 each hit a place where the doc went silent and I improvised. The fourth ran verbatim with **zero** deviations.

*Timing (pass 4, the clean one).* `scripts/dev.sh reset` **46 s** wall clock (postgres up `07:42:51` → stack healthy `07:43:37`, of which the Control waits for a healthy DB and the worker for a healthy Control). Steps 9–10, pool → account → default pool → health → `quote`/`run`/`report` → confirmed in Mailpit: **10 s**. **Total ≈ 56 s** unattended — but that number is only honest for a reader who does not have to stop and think, which is exactly what the three earlier passes did not have.

*Terminal state.* `has_credential: true` (no credential echoed), health `headroom: ok` / `dispatch_paused: false` / `boxes.active: 2` / `accounts.healthy: 1`, `quote` → `{"cost":1,"tier":"shared"}`, `run` → `202 queued`, `report` → `{"state":"sent","provider_message_id":"6hLItpFlPk1CFuscrMQHFr","attempt_count":0}`, Mailpit `total: 1` — `Local smoke test -> you@example.com`. The `provider_message_id` is the proof the worker reported a real accepted send; `attempt_count: 0` means it landed first try.

**Four docs bugs found, all fixed in `install-and-deploy.md` in the same change.** Every one of them is a *silence*, not an error — the product answered correctly each time and the doc simply did not say enough:

| # | Where | What a first-time local reader hits |
|---|---|---|
| 1 | Step 9, the account body | `smtp.example.com` does not resolve and nothing named the bundled sink. Now: `smtp-sink:1025`, Mailpit UI at `localhost:8025`, and why any credential works (STARTTLS still exercised). |
| 2 | Step 10, the three calls | The step said "run one real campaign through `quote` → `run` → `report`" with **no bodies** and — unlike Step 9 — **no contract link**. All three guesses were wrong: `quote` takes `recipient_count` only (`user_id` is a strict-decode `400`), `run` is singular `job_item_id` with no `campaign_id`, `report` uses query params (a path segment is a router `404`). |
| 3 | Step 10, `UID=` | `UID` is **readonly in bash** — the assignment fails, `run` posts an empty `user_id`. Renamed to `OWNER`. Shell-dependent (zsh permits it), which is how it survived review. |
| 4 | Step 10, `$ITEM` | `public.job_items` is **empty after a reset** and nothing in this repo seeds it (the module holds `SELECT` only), so the doc named a value the reader could not obtain. Now carries the dev-only `INSERT`, explicitly marked not-for-production. |

Bug 4 has a sharp edge worth its own line, because its symptom lies: `psql -tA` on a `RETURNING` insert prints **two** lines — the uuid, then the command tag `INSERT 0 1`. Capture both and `$ITEM` becomes `08a8404b-…-a46b98d47392INSERT 0 1`; the JSON body is malformed, and `run` answers `400 validation_failed` naming **`job_item_id: required`** — which reads as a wrong field name and is not one. `head -1 | tr -d '\r\n'`, then `echo "[$ITEM]"` and expect 36 characters. On the first three passes this cost more time than the other three bugs combined.

**What passed on its own merits:** the two-secret discipline in Step 9 (`read -rs`, `--data @file`, `shred -u`) works exactly as written; Step 10's stated health expectation matched the observed response field for field; and both auth domains stayed separate throughout. That last one was checked rather than assumed, with paired positive controls (a cross-domain `401` proves nothing if the credential is simply broken):

| Credential | → `/v1/quote` (Domain A) | → `/v1/admin/health` (Domain B) |
|---|---|---|
| operator admin key | **401** `unauthenticated` — "Missing or invalid platform service token." | **200** |
| platform service token | **200** | **401** `unauthenticated` — "Missing or invalid admin API key." |

Both refusals carry only `{code, message, request_id}` — the message names the *expected* credential class, never the presented one, and leaks no hash, no key id, no internal reason.

---

### Stage 3 — First-run empty state

From a fresh reset, before creating anything, look at the system as a new operator would.

1. Query the health endpoint
2. Attempt to run a campaign with no accounts, no pools, and `default_shared_pool_id` unset

**Pass:** the system tells you clearly what is missing. Jobs sit queued in a clean *not configured* state — no crash, no cryptic error, no silence, no event churn.

**Observed (2026-07-29, both steps PASS).** Precondition asserted first, not assumed: `pools=0`, `accounts=0`, `jobs=0`, and `default_shared_pool_id` = `{"v": null}` (a real JSON `null`, checked as `value->>'v' IS NULL` — an empty string would be a different state).

*Step 1 — health on a virgin system.* `200`, every counter zero, `pools: []`, `accounts` all-zero, `boxes.active: 2`. Nothing invented, nothing crashed.

*Step 2 — run a campaign into the void.* `POST /v1/run` → **`202 {"state":"queued"}`**, which is correct per the contract (async kickoff, never blocks on the send). Then the load-bearing assertion, checked at ~12 s and again at ~60 s:

| What | Observed | Why it matters |
|---|---|---|
| `send_managed.jobs.state` | `queued` (unchanged) | not failed, not stuck |
| `attempt_count` | `0` | dispatch never burned an attempt on an unservable job |
| `job_events` rows | **exactly 1** (`system`, `→ queued`, no error class) | **no event churn** — the genesis row and nothing else |
| `/v1/report` (tenant) | `{"state":"queued","attempt_count":0}` | reads as clean pending; no internal reason leaks |
| `/readyz` | `200 ready`, `db: true`, `singleton_lock: true` | no crash |

Zero churn is the half of this stage that could plausibly have failed and didn't: a 1-second polling dispatcher facing an unservable job is exactly the shape that spins `queued→assigned→queued` forever, inflating `job_events` and `attempt_count`. It doesn't — the pool chain is resolved *before* a claim.

*What the operator is told.* Three signals, in increasing usefulness:
- `queue.waiting_for_capacity: 1` in `/v1/admin/health` — the machine-readable "something is waiting".
- The Control log names the condition, the exact key, and the fix: `dispatch: shared_pool_not_configured — default_shared_pool_id is unset or does not resolve to an enabled shared pool; affected base/fallback campaigns stay queued cleanly until an operator sets it (§4.3)`. **Rate-limited across the process:** 3 lines in ~4 minutes, not one per tick.
- `operator-runbook.md`'s triage table routes the observed symptom (`waiting_for_capacity > 0`) to a section that names this exact case in words: *"A chain that is merely empty — for example `default_shared_pool_id` unset — is treated as a configuration gap and **waits**; it never fails jobs. So a config mistake costs you delay, not data."* The runbook path was walked from the symptom, not assumed to exist.

> **`active_conditions[]` stays `[]` here, and that is BY DESIGN — do not read it as the empty-state signal.**
> It carries exactly three conditions (`pool_exhausted`, `account_quarantined`, `worker_death`, `health_alerts.go`),
> and `pool_exhausted` iterates the pools in the snapshot — with **zero pools** there is nothing to
> report as exhausted. A brand-new never-configured system is deliberately not an *alert*; it is a
> *setup* state, and the runbook's own triage table has a separate row for it
> (`waiting_for_capacity > 0`). Alerting on it would page an operator during install.
>
> **Two traps for whoever repeats this stage.** `POST /v1/run` takes `{job_item_id, user_id}` —
> **singular**, and there is no `campaign_id` field (that lives on the `job_items` row). Second: my
> first attempt returned `400 validation_failed` and the field names were NOT the cause — the log
> showed `reason=malformed_json: invalid character '\n' in string literal`, i.e. a captured psql id
> had carried a trailing newline into the JSON, so the body never parsed and validation was never
> reached. Incidentally a clean live check of Rule 5: the client got only
> `{code, message, request_id}` while the internal reason went to the log **keyed by the same
> `request_id`** — correlatable by an operator, invisible to the caller.

---

### Stage 4 — Admin surface walkthrough

Do this by hand through the API, not through a script. You are testing whether the surface is usable.

1. `POST /v1/admin/pools` — create a shared pool
2. `POST /v1/admin/accounts` — create an account pointed at Mailpit
3. `PUT` the config key `default_shared_pool_id` to your pool
4. `GET` accounts and pools — verify listings
5. Exercise every lifecycle transition: quarantine → unquarantine, pause → unpause, disable → enable
6. Verify each transition wrote exactly one `account_events` row with `actor='admin'`
7. Try an illegal transition

**Pass:** every operation works with clean responses; the audit trail is complete; illegal transitions are rejected with a usable error.

---

### Stage 5 — Security verification

1. Create an account with a secret. `GET` it, list it, and grep the Control and worker logs.
   - **Pass:** the secret appears nowhere. Only `has_credential` is exposed.
2. Update the account *without* sending the secret. Check the ciphertext in the database.
   - **Pass:** byte-unchanged.
3. Update *with* a new secret.
   - **Pass:** ciphertext changed, `credential_key_id` recorded.
4. Grep logs for the secret in **base64**, not just plaintext — SMTP AUTH transmits it encoded.
   - **Pass:** absent in both forms.
5. Attempt to read tenant B's job with tenant A's selector.
   - **Pass:** not-found (no existence disclosure); the correctly scoped request succeeds.
6. Attempt an admin call with no key and with a wrong key.
   - **Pass:** both rejected.
7. Confirm no `/v1/admin/*` route can produce a plaintext credential.
   - **Pass:** none can.

---

### Stage 6 — Config surface and invariants

1. Read all **53** config keys
2. Change one with an observable effect (the pacing delay is easiest to see)
   - **Pass:** takes effect without a redeploy
3. `PUT` `smtp_operation_timeout_sec` **higher** than `job_max_processing_time_sec`
   - **Pass:** rejected at the boundary, naming the conflicting key and the valid range; **nothing lands in the DB**
4. `PUT` `box_outcome_buffer_ttl_sec` **lower** than `startup_reconnect_window_sec`
   - **Pass:** same clean rejection
5. Perform the ordered two-step widening (raise the lease first, then the timeout)
   - **Pass:** succeeds
6. Try to set an invalid type — a quoted numeric string where a number belongs
   - **Pass:** rejected cleanly; the Control does **not** wedge
   - **Then push past the API.** The `PUT` rejection is the easy half — the value never reaches the
     DB, so nothing has to survive it. Write the bad value **directly in SQL** (the one path by which
     a type fault really arrives: a hand-run `UPDATE`, a restore, a bad migration) and watch the
     Control load it:
     ```bash
     docker compose exec -T postgres psql -U postgres -d platform -c \
       "UPDATE send_managed.config SET value='{\"v\":\"thirty\"}' WHERE key='smtp_operation_timeout_sec'"
     ```
     **Pass:** the loops halt fail-closed (jobs stay `queued`, nothing marked sent/failed), the
     container does **not** restart, the log names the offending key at `level=error`, there is **no**
     `db outage DETECTED` line, and repairing the value resumes dispatch within a tick with no
     restart. Doing this found a real defect — the fault was reported as `db outage DETECTED … the DB
     is unreachable` at ~2 lines/second against a perfectly healthy DB, pointing the operator at
     Postgres instead of the config row and burying the one useful line. Fixed in `a617fda`.

---

### Stage 7 — The send path

1. `GET /v1/describe` — tiers and pricing
2. `POST /v1/quote` for N recipients
   - **Pass:** `N × unit_price_credits`
   - Do it for several N including the edges — `0`, `1`, `250`, `10000` — and then retune
     `unit_price_credits` and re-quote. **Pass:** `0` is a valid count quoting `cost: 0` (not a
     rejection), and the retuned price takes effect on the very next `quote` AND on `describe` with
     **no redeploy** (config-not-code; the two read the same live key so they cannot disagree).
   - Then push past the happy path and quote the values a caller will actually send by mistake:
     omitted, `null`, `-5`, `"7"` (a string), `2.5`, and an unknown extra field. Read the
     `details[]` array in each envelope, not just the status code. **Pass:** all rejected `400
     validation_failed`; **and each rejection is distinguishable** — absent/`null` →
     `recipient_count:required`, `-5` → `recipient_count:out_of_range`, a wrong type →
     `recipient_count:malformed`, an unknown field → empty `details[]` deliberately (its name is
     caller-supplied text and must never be echoed into a field a UI renders). This found two real
     defects: `-5` reported `required` — telling the caller a field it had plainly supplied was
     missing, while `run`/`report` on the same server already distinguished the two — and a
     wrong-type value returned an empty `details[]`, naming no field for a UI to attach the error
     to. Both fixed in `fe27588`. The status code alone would have looked green for all six.
3. `POST /v1/run` with a small campaign
   - **Pass:** returns immediately; does not block on sending
4. Poll `report` through the run
   - **Pass:** counts and progress track reality
5. Check Mailpit
   - **Pass:** every message arrived; headers present and correct — `From`, `To`, `Subject`, `Reply-To`, `Message-ID`, `Date`
6. Check the database
   - **Pass:** jobs `sent` with non-null `provider_message_id`; `job_events` chain complete with correct actors; `send_logs` written

---

### Stage 8 — Routing and pacing

1. Add a second account. Set the pool strategy to `round_robin`, run a campaign.
   - **Pass:** sends distribute across both accounts (check `assigned_account_id` spread)
2. Repeat with `random`, then `one_by_one`
   - **Pass:** each distributes as specified. **Distinguish them by STRUCTURE, not just by count** — a
     13/7 split could be produced by any of the three. What separates them: `round_robin` gives a
     strictly alternating `ABABAB…` (a run of 2 is already a failure); `random` shows *runs* (a run of
     5 identical picks is structurally impossible under round-robin); `one_by_one` is a single long run.
   - **`one_by_one` needs EQUAL `daily_limit`s to be sticky.** With unequal limits it hands the lead
     to a larger-limit peer after as little as ONE send — the score's `w_prox` term (weight 15)
     penalizes `daily_sent_count ÷ daily_limit`, so a small-limit account sheds score fastest. If you
     cap one account to force a handover, cap the one that is *actually leading* (`score DESC`), not
     whichever you seeded first. See the `one_by_one` note in [`operator-runbook.md`](./operator-runbook.md).
3. Pause one account mid-campaign
   - **Pass:** it is skipped; sends continue on the other. Pause it with jobs still `queued` (slow the
     pacing so the campaign outlives the pause) or the test is vacuous — check the paused account's
     last `completed_at` really precedes the pause, and that the peer absorbed every remaining job.
     Then `/unpause` it: a **manual** pause lifts cleanly (200), which is the contrast to the
     `daily_limit` pause below.
4. Measure throughput against `lanes ÷ average delay`
   - **Pass:** observed rate matches within tolerance. Expect it slightly UNDER the ideal — the SMTP
     round trip is real work the delay doesn't account for. Observed: 1 lane at delay 4–5s gave
     11.17 sends/min against a ~13.3 ideal (≈0.87s/send of overhead). Measure `max(completed_at) −
     min(completed_at)` over `sent` jobs and divide by `count−1`, not by `count`.
5. Double the concurrency, measure again
   - **Pass:** roughly double the throughput — this proves per-lane pacing rather than a single global rate
   - Observed across a 4× range: 1 lane → 11.17/min, 2 → 24.69/min (2.21×), 4 → 50.47/min (2.04×).
     Near-linear in lanes is the actual proof; a flat or sub-linear curve would mean a global rate.
     Set `concurrency_per_user_default` through `PUT /v1/admin/config/{key}`, not direct SQL, so the
     write serializer and validation run.
6. Create a dedicated pool for one user, exhaust it, and watch fallback
   - **Pass:** falls back to shared **only** when the dedicated pool is genuinely unavailable, not before
   - **Assert BOTH halves — "not before" is the one that catches a bug.** Give the dedicated account a
     small `daily_limit` and queue more than that: the first N must land on the dedicated pool and only
     the overflow on shared (observed 4 dedicated then 4 shared at `daily_limit=4`, 8 jobs). Then raise
     the limit, age `daily_reset_day`, and re-run: **zero** jobs may touch shared while the dedicated
     pool still has headroom (observed 5/5 dedicated).
   - **Also check dedicated→dedicated is refused (P4-3).** Point the dedicated pool's `fallback_pool_id`
     at *another tenant's* dedicated pool and exhaust: that fallback must be **ignored** (it resolves
     only to an enabled *shared* pool) and routing must fall through to `default_shared_pool_id`.
     Observed: all 3 jobs on shared, zero on the other tenant's accounts. One tenant's overflow must
     never land on another tenant's identities.
7. Start a second campaign for a user who already has one active
   - **Pass:** rejected outright, cleanly; no job rows created
   - `409 conflict` per attempt, and `SELECT count(*) FROM send_managed.jobs WHERE campaign_id = <B>` must be
     **0** — the rejection is what's under test, but the *zero rows* is what makes it "outright" rather
     than "queued and ignored". Confirm the envelope carries only `{code, message, request_id}` — the
     internal reason `user_campaign_active` must not appear (Rule 5).
   - **Two controls, or this proves nothing.** (a) A repeat `run` within the SAME campaign must still
     be accepted while that campaign has non-terminal jobs — the gate is per-*campaign*, not
     per-in-flight-job (already exercised by running all N items of campaign A). (b) Once A drains to
     terminal, B must be accepted (observed 3 × 202) — otherwise a blanket "always reject" would pass.

---

### Stage 9 — Failure injection

The most valuable stage.

> **Read this before starting: steps 1–4 have no live-stack mechanism, by omission not by design.**
> This stage used to open with "use whatever failing-SMTP mechanism your Phase 5 error-taxonomy tests
> already use." **There isn't one on the live stack.** Walking the stage found three separate things:
> - The compose sink is **Mailpit** with `MP_SMTP_AUTH_ACCEPT_ANY: "true"`. It accepts *any*
>   credential and cannot be made to emit a 535, a 550, a 4xx, or a throttle. So it can prove the
>   success path and nothing else.
> - `docker-compose.yml`'s own comment on that service claims "the failed-AUTH / no-STARTTLS /
>   requeue paths use a controllable Go sink (`tests/harness`)." **That Go sink was never built.**
>   The comment describes an intention, not a file.
> - The only controllable sink that exists is `services/worker/test/sink.mjs` — in-process Node, reached
>   only by the worker's own unit tests, not reachable from a running container.
>
> Where the coverage for steps 1–4 actually lives, and it is real coverage — all of it in
> `services/control/internal/dispatch/`, driven through the real `TryDispatch` + `OutcomeHandler`
> against real Postgres, with the SMTP text fed in as the worker would report it:
>
> | Step | Test | What it pins |
> |---|---|---|
> | 1 auth failure | `policy_test.go::TestPolicy_AuthFailureQuarantines` | 535 → `auth_failure`, job requeued, account quarantined, `paused`/`disabled` untouched, exactly one `account_events(system, quarantine, auth_failure)` |
> | 1 shadow half | `shadowmode_test.go::TestShadowMode_*` | the same condition under `shadow` **decides without acting** (flag not set, would-quarantine row logged), under `enforce` acts, under `off` no decision at all |
> | 1 re-route half | `exitcriteria_test.go::TestExit_AuthFailureThenSendsViaDifferentAccount` | after the 535 the job sends via a **different** account |
> | 2 hard bounce | `dispatch_test.go::TestOutcome_BouncedTerminal` + `score_test.go::TestScore_BounceNeverPenalizes` | 550 → `bounced` (terminal, lease cleared, `completed_at` set); score, `consecutive_failures`, `last_failure_at` **byte-unchanged** and zero `account_events` — with a paired `transport_4xx` positive control proving the scorer isn't just dead |
> | 3 transient 4xx | `classify_test.go` + `policy_test.go` | a bare 4xx classifies `transport_4xx` (**not** `throttle`) and requeues without quarantine |
> | 4 throttle | `policy_test.go::TestPolicy_ThrottlePauses` + `statemachine_test.go::TestStateMachine_ThrottleAutoClears` | 421 → paused with `reason='throttle'`, `throttle_paused_at` stamped, **not** quarantined; the pause self-clears at the timer on the next consume, and a `manual` pause in the same pool is **not** swept up |
>
> Note `policy_test.go` reaches its assertions because the harness pins `autoquarantine_mode=enforce`
> (`harness_test.go`); the seeded default is `shadow`. That is why step 1 has two rows above — the
> enforce behaviour and the shadow-ladder behaviour are separate tests, and the guide's own wording
> ("in shadow … the flag is *not* set") is the `shadowmode_test.go` half.
>
> **One live failure mechanism DOES exist, and it is worth knowing about even though it cannot close
> steps 1–4.** Setting an account's `smtp_port` to a dead port (e.g. 2599, nothing listening) makes the
> worker report `connect_refused`, which classifies as `timeout` — a genuine worker-reported penalizing
> failure on the live stack, no hand-written rows. It drives the score, `consecutive_failures`,
> `last_failure_at`, the backoff ladder, and the auto-quarantine ladder (this is how Stage 10 steps 3–4
> are closed live). What it **cannot** do is choose a *class*: a dead port is always `timeout`, never
> `auth_failure`, `bounce_permanent`, `transport_4xx`, or `throttle` — which is exactly what steps 1–4
> each name. So the gap above is narrower than "no failure injection at all": it is specifically the
> inability to control the SMTP *reply*, which is what the missing controllable sink would give.
>
> **So:** treat steps 1–4 as *verified by those suites*, and steps 5–10 as live-stack work. Do not
> mark 1–4 green by hand-writing a `job_events` row or hand-setting a `last_error_class` — that
> proves the column is writable, not that the classifier and policy work. Closing this properly means
> building the controllable sink the compose comment already promises; that is registered in
> [`future-work.md`](./future-work.md), not left implicit here.

1. **Auth failure** — point an account at a credential the sink rejects
   - **Pass:** classified `auth_failure`; in shadow, a would-quarantine decision is logged and the flag is *not* set; the job requeues and sends via the other account
   - *No live mechanism (see the note above). Covered by `classify_test.go` + `policy_test.go`'s `TestPolicy_AuthFailureQuarantines`.*
2. **Hard bounce (550)** — force a 550 for one recipient
   - **Pass:** job is `bounced` (not `failed`), no retry, and the account's score and `consecutive_failures` are **unchanged**
   - *No live mechanism. Covered by `carveout_test.go` — the "bounce never penalizes" carve-out is asserted byte-for-byte there.*
3. **Transient 4xx** — force a 4xx
   - **Pass:** retried, then requeued; no account quarantine
   - *No live mechanism. Covered by `classify_test.go` (a bare 4xx → `transport_4xx`, not `throttle`) + `policy_test.go`.*
4. **Throttle** — force a throttle response
   - **Pass:** account paused, job requeued, pause auto-clears without intervention
   - *No live mechanism. Covered by `policy_test.go`'s `TestPolicy_ThrottlePauses` + the P5-13 expired-throttle self-clear in `eligibility_test.go`.*
5. **Daily limit** — set an account's `daily_limit` low enough to hit mid-campaign
   - **Pass:** pauses, requeues to the other account, and `report` shows *waiting for capacity* — never failed
   - **Two traps.** (a) `waiting_for_capacity` requires **zero** eligible accounts across *all* the
     tenant's reachable pools — a capped account sitting beside a healthy peer correctly reports `0`,
     so cap **every** account or the assertion is vacuous. (b) To age `daily_reset_day` you must use
     the **container's** clock, not the host's: `docker compose exec postgres date -u`. Setting it to
     the host's "yesterday" was a no-op here because the container had already rolled over to the
     next UTC day.
   - Also assert the *recovery* is the same rows: after aging the reset day, the **same job ids** go
     `queued → sent`, total job rows unchanged (nothing re-created), and `attempt_count` did not move
     while they were waiting — waiting must not burn retry budget.
6. **Worker death** — `docker kill` a worker container mid-send (abrupt, not a graceful stop)
   - **Pass:** the reclaim loop recovers the job, it re-dispatches to the surviving worker, and Mailpit shows **exactly one** copy
   - **You cannot win this race by polling.** Against a local Mailpit the SMTP hop is sub-100ms, so
     the `sending` window is shorter than any practical poll — a 50ms in-database polling loop over
     60s never observed a single `sending` row across 10 jobs. `send_delay_{min,max}_sec` will not
     help: that delay is **inter-send** pacing, not intra-send. Raising it to 20–25s changed nothing
     about the window.
   - **Use `docker pause`, not timing luck** (Rule 10 applies to hand-run tests too — a test that
     cannot fail is not a test). A paused worker keeps its mTLS socket open but never processes the
     pushed job, so the job pins in `sending` indefinitely and you can act deliberately:
     ```bash
     docker compose pause worker2                    # freeze it
     tests/manual/launch-campaign.sh s9dead 10    # peers drain; one job pins on the frozen worker
     docker kill --signal=SIGKILL coldmail-send-managed-worker2-1   # kill it WHILE STILL FROZEN
     ```
   - **Kill it while frozen, and check Mailpit *before* the kill.** Unpausing first invalidates the
     run: the first attempt here did that, worker2 completed the SMTP hop and reported `sent` before
     SIGKILL landed, and the test proved nothing about reclaim. Assert **0 copies pre-kill** so you
     know the send genuinely never happened and no outcome can ever arrive.
   - **Mailpit's search is fuzzy — count per exact subject.** `?query=...message 1` also matches
     "message 10", which reads as a phantom duplicate. Group by exact `Subject` and assert 1 each:
     ```bash
     curl -sS "http://localhost:8025/api/v1/search?query=s9dead" | python -c "
     import json,sys,collections
     c=collections.Counter(m['Subject'] for m in json.load(sys.stdin)['messages'])
     print('dupes:', [s for s,n in c.items() if n>1] or 'NONE')"
     ```
   - **Observed (live, 2026-07-29).** Job pinned on frozen worker2 with `attempt_token=1`, 0 Mailpit
     copies, SIGKILL while frozen. The lease (`job_max_processing_time_sec`=120) expired and
     `reclaim_loop` requeued it with `error_class='timeout'`; the surviving worker re-sent it. Final row:
     `sent`, `assigned_box_id` = the *other* worker, `attempt_token=2`, `attempt_count=1`. Event chain
     was exactly 7 rows, `sending→queued` attributed to `reclaim_loop` (not `worker` — the worker reported
     nothing, and the system did not invent an outcome for it). **Exactly one** Mailpit copy and
     **exactly one** `send_logs` row — one real SMTP attempt for one reclaimed job.
   - This run also confirmed [`send_managed_0022`](../db/migrations/send_managed_0022_job_events_error_context.up.sql)
     in the wild: `send_managed.jobs.last_error_class` correctly *retained* `timeout` on the now-`sent` row
     (a running fact about a mutable row), while the `sending→sent` **event** carried no error class.
     Pre-0022 that success would have been tagged `timeout` and counted as a failure.
7. **Control restart** — restart the Control mid-campaign
   - **Pass:** orphans recover, the campaign resumes, nothing lost or duplicated; a worker that was mid-send reconnects and replays its buffered outcome
   - **Use the same `docker pause` trick as step 6** to have a job genuinely in-flight when the Control
     dies — otherwise there is no orphan and no buffered outcome, so the interesting half is untested.
     Freeze `worker2`, launch, let the peers drain, then `docker compose restart control` while one job sits
     `sending` on the frozen worker. **Do not unpause until after the Control is back** — the replay is the
     point.
   - **Read the shutdown log, not just the restart.** Three distinct lines each prove a different
     property, and all three should appear:
     ```
     shutdown: DRAINING — dispatch stops claiming ... the worker link and outcome handler stay up
     shutdown: draining 1 in-flight job(s), waiting up to 30s for their workers to report (§4.1)
     startup rescan: extended 1 orphaned in-flight lease(s) + refreshed worker heartbeats
     ```
     The third is the load-bearing one: the rescan **extends** the orphan's lease, it does not reclaim
     it. That is what opens `startup_reconnect_window_sec` for the worker to come back and report the
     send it really did, instead of the Control assuming the worst and re-sending.
   - **Assert the orphan is still `sending` immediately after the restart.** This is the honesty check
     and it is easy to skip: `state='sending'`, `attempt_token` **unchanged**, `attempt_count` still
     `0`, `last_error_class` NULL, `lease_expires_at` pushed further out. A Control that marked it
     `failed` (or `sent`) on the way down would be inventing an outcome no worker reported (Rule 5).
   - **Observed (live, 2026-07-29).** 14 jobs; 13 sent by `worker`, 1 pinned `sending` on frozen `worker2`
     with 0 Mailpit copies. Restart: drained gracefully (waited its 30s for the frozen worker, which by
     construction could not answer), came back, rescan extended that one lease. Orphan still
     `sending`/token 1/attempts 0 — **nothing invented**. Then unpausing `worker2` produced
     `control stream closed; reconnecting (outcome buffer retained)` → re-register → the buffered
     outcome replayed and the job went `sent` **on the original `attempt_token=1`**, with a
     `provider_message_id`. Event chain exactly 4 rows (`→queued`, `queued→assigned`,
     `assigned→sending`, `sending→sent` by **`worker`**) — no requeue, no reclaim, no second dispatch.
     One `send_logs` row; Mailpit 14/14 subjects, **zero** duplicates.
   - Note the contrast with step 6, which is what makes both meaningful: same starting position, but
     there the worker never came back so `reclaim_loop` requeued it (token 1→2, one re-send); here the worker
     *did* come back inside its window so the original attempt was honoured. Exactly one delivery
     either way.
8. **Database outage** — `docker stop` Postgres for a few minutes, then start it
   - **Pass:** the Control halts cleanly, does not crash-loop, and **invents nothing** — no job is marked sent or failed during the outage. On recovery it resumes with no double-processing.
   - *This is the single most important assertion in the guide.*
   - **Snapshot before you pull the plug, or "invented nothing" is unprovable.** Counting rows only
     afterwards cannot distinguish "nothing was written" from "something was written and looks
     plausible". Capture totals **and** `max(seq)` — the sequence is the strongest evidence, because a
     single fabricated transition anywhere in the system advances it:
     ```bash
     docker compose exec -T postgres psql -U postgres -d platform -tAc "
       SELECT (SELECT count(*) FROM send_managed.jobs), (SELECT count(*) FROM send_managed.job_events),
              (SELECT count(*) FROM send_managed.send_logs), (SELECT max(seq) FROM send_managed.job_events)"
     ```
     Freeze `worker2` first (step 6's trick) so a job is genuinely in-flight when the DB dies — an outage
     with nothing in flight is the easy case and skips the whole question.
   - **Four things to check while it is down**, each a different failure mode:
     1. `docker compose ps control` says **running**, and `docker inspect --format '{{.RestartCount}}'`
        is **0**. A crash-loop would "recover" by luck and lose the drain guarantees.
     2. The log says **`loops remain PAUSED, no state invented`** on a backoff, not a flood. Probes
        should space out (seconds → capped re-probe), not spin.
     3. `GET /v1/admin/health` and `POST /v1/run` **fail closed** — and their envelopes carry only
        `{code, message, request_id}`. `run` must **refuse**, never return a `queued` job it could not
        persist.
     4. The internal reason appears **in the log keyed by that same `request_id`** and nowhere else.
        This is Rule 5 in both directions; grep the response body for `SQLSTATE`, `5432`,
        `send_managed_app`, `postgres`, `select` and expect **nothing**.
   - **Observed (live, 2026-07-29).** 16 jobs, 15 sent, 1 pinned `sending` on frozen `worker2`; snapshot
     266 jobs / 1088 events / 265 send_logs / `max(seq)`=1088. Postgres stopped for **1m48s**.
     - Control stayed `running (healthy)` with **0 restarts**. Backoff went 0→3→4→9→14→27→46→59→78→91s
       with the re-probe capping at 30s, every line ending `loops remain PAUSED, no state invented`.
       The singleton lock connection was correctly noticed as lost (`57P01`) and **marked not-held**
       rather than presumed still held.
     - `health` → `500 internal_error`, `run` → `500 internal_error`, both with only
       `{code, message, request_id}`. The reasons `health_window_read_failed` and `begin_tx_failed`
       appeared in the log against the exact matching `request_id`s. The leak grep found **nothing**.
     - Recovery: `db outage RESOLVED after 1m48s — RESUMING loops from stored state`, then
       `reclaim: tick reclaimed 1 job(s)`.
     - **The decisive comparison:** `jobs_total` **266 → 266** (nothing fabricated), `send_logs`
       **265 → 266** — exactly **one** new send, for the one job that genuinely needed re-sending.
       `job_events` 1088 → 1092, and the four new rows are seq **1089–1092**, i.e. the next four after
       the snapshot: **zero events were written during the outage**. The orphan's chain shows the
       honest story — `sending→queued` by `reclaim_loop` with `error_class='timeout'` (the worker never
       reported, and no outcome was invented for it), then a clean re-dispatch to `sent` by `worker`.
       One `send_logs` row for it; Mailpit **16/16** subjects, **zero** duplicates.
9. **Pool exhaustion** — exhaust every account mid-campaign
   - **Pass:** jobs **wait** (not failed) with a clean waiting-for-capacity status; re-open one and the same jobs complete
   - > **This step as originally written was wrong, and following it literally reports a false
     > failure.** It said to "quarantine or disable every account" and expect the jobs to WAIT. Doing
     > exactly that produced 8 jobs in state `failed` with
     > `pool_exhausted: all eligible accounts terminal_blocked (§5.1)`. **The code is correct and the
     > guide was not.** `internal/dispatch/exhaustion.go` implements a locked three-way verdict, first
     > match wins: (1) any eligible account → CLAIM; (2) empty/unresolvable chain → WAIT (a config gap
     > is never an exhaustion); (3) any **self_recovering** account → WAIT; (4) otherwise all
     > **terminal_blocked** → FAIL. Quarantined / disabled / **manually**-paused is
     > `terminal_blocked` — it needs a human, so failing the job is the honest outcome rather than
     > queueing forever behind a block nothing will lift. `daily_limit`-paused, at-limit-this-cycle,
     > and throttle-paused-within-window are `self_recovering` — a timer reopens them, so those WAIT.
     > **To test the WAIT path you must exhaust accounts self-recoveringly, not block them.**
   - **The WAIT setup** — put every account in the tenant's chain at its daily limit on the current
     cycle (not disabled, not quarantined, not manually paused):
     ```bash
     docker compose exec -T postgres psql -U postgres -d platform -c \
       "UPDATE send_managed.accounts SET daily_sent_count=daily_limit, daily_reset_day=CURRENT_DATE,
              paused=TRUE, pause_reason='daily_limit' WHERE user_id='<tenant>'"
     ```
     Remember Stage 5's trap: it must be **every** account the tenant can reach, or a healthy peer
     absorbs the work and `waiting_for_capacity` correctly reads `0`.
   - **Capture the job ids before re-opening capacity.** "The same jobs complete" is the actual claim,
     and it is unfalsifiable if you only count rows — 6 sent could be 6 *new* jobs. Diff the id list.
   - **You cannot re-open a `daily_limit` pause with `unpause` — and shouldn't be able to.** That
     returns **422** `unprocessable`: *"This pause was not set manually and clears automatically; only
     a manual pause can be lifted here."* Correct: the operator surface only lifts what an operator
     set. Re-open capacity the way production does, by letting the automatic path fire — age **one**
     account's `daily_reset_day` and leave the rest at the limit. Use the **container's** clock
     (`docker compose exec postgres date -u`), not the host's.
   - **Observed (live, 2026-07-29).** Both halves.
     - *FAIL half (terminal):* both shared-pool accounts disabled through the real admin API → 8 jobs
       `failed`, `attempt_count=0`, message `pool_exhausted: all eligible accounts terminal_blocked
       (§5.1)`. Per §5.1 this is the specified behaviour.
     - *WAIT half (self-recovering):* both accounts at limit on the current cycle → 6 jobs sat
       `queued` with `attempt_count=0` and **zero writes** (no churn), and `GET /v1/admin/health`
       reported `queued: 6, waiting_for_capacity: 6` — waiting, not failed.
     - *Recovery:* aged **one** account's reset day; the lazy daily reset re-opened it and the
       **byte-identical six ids** (diffed against the pre-capture list) all reached `sent`, all on
       that one account, with `attempt_count` still `0` for four of them — **waiting did not burn
       retry budget**. `waiting_for_capacity` returned to `0`, and Mailpit showed 1 copy each.
   - *Mailpit's search tokenizes:* a `s9wait2` query also returned an unrelated `S9 s9wait 2` subject
     left over from an earlier run. Match exact subjects before calling a count a duplicate.
10. **Attempt token** — inspect `attempt_token` across a re-dispatch
    - **Pass:** a distinct token per dispatch
    - **Verified by step 6's run — no separate setup needed.** A re-dispatch is exactly what a reclaim
      produces, so read the token on the job step 6 killed a worker under: it went `1` (first dispatch,
      event seq 909) → `2` (post-reclaim dispatch, seq 956), with `attempt_count` `0` → `1`.
    - The teeth are the pairing: a distinct token **and** exactly one `send_logs` row and one Mailpit
      copy. A token that advanced while two sends happened would be a duplicate-send bug wearing a
      correct-looking token; a single send under a *reused* token would mean a stale outcome from the
      dead worker could still be accepted for the new attempt. Assert both halves.

---

### Stage 10 — Operability

1. **Panic switch** — set `dispatch_enabled = false` mid-campaign
   - **Pass:** dispatch stops; nothing fails; jobs stay queued; `report` shows a clean operator-paused state
   - **Flipping it mid-campaign is the weaker test; launching a campaign *into* the halt is the
     stronger one.** A local campaign can drain in seconds, so "dispatch stopped" may just mean "there
     was nothing left to dispatch" — a vacuous pass (Rule 10). Do both: flip it mid-flight, then
     `run` a fresh campaign while it is still `false` and confirm those jobs never move.
   - **Prove the halt by the event log, not by polling states.** A state snapshot can read a
     transaction that committed *before* the flip and look like a violation. The real question is
     whether any NEW transition was written after it:
     ```bash
     docker compose exec -T postgres psql -U postgres -d platform -c \
       "SELECT max(created_at) FROM send_managed.job_events"   # must predate the flip
     ```
   - **Observed (live, 2026-07-29).** Flipped mid-campaign: the last `job_events` row system-wide was
     **3s before** the flip and **zero** events followed it, so dispatch genuinely stopped claiming.
     Launched 10 jobs into the halt: all sat `queued`, `assigned_account_id` NULL, `attempt_count=0`,
     **nothing failed**, for as long as it was left off. `/v1/report` read clean
     `{"state":"queued","attempt_count":0}` — no internal reason, no error (two-vocabulary rule), and
     `/v1/run` kept **accepting** work (the switch halts dispatch, it does not reject callers).
     `/v1/admin/health` showed `dispatch.dispatch_paused: true` with `waiting_for_capacity: 0`, which
     is the distinction that matters to an operator: an intentional halt is not a capacity fault.
   - **A halt does not block outcome persistence, and should not.** A job already in-flight when the
     switch flipped still had its outcome accepted and committed when its worker reported. That is
     correct — refusing it would strand a real send and force a duplicate later. The switch stops the
     Control from *claiming and pushing*, not from telling the truth about work already out the door.
   - *One-campaign-per-user still applies during a halt:* a second `run` is refused `409 conflict`
     while the first campaign has non-terminal jobs — which it will, because they are frozen. Let the
     first drain (or clear it) before launching the "into the halt" case.
2. Re-enable
   - **Pass:** the **same** job rows resume and complete — nothing lost, nothing re-created
   - **Diff the id list, and check the event chains.** "The same rows" is the claim; counting 10 sent
     cannot distinguish resumed rows from 10 new ones.
   - **Observed (live, 2026-07-29).** Set back to `true`: all 10 reached `sent` within ~45s, the id
     list **byte-identical** to the pre-flip capture (nothing re-created), and `attempt_count` still
     **0** on every one — an operator halt burns no retry budget. Event chains were exactly 4 rows per
     job (`→queued`, `queued→assigned`, `assigned→sending`, `sending→sent` by `worker`) — **40 rows
     total, zero requeues and zero reclaims**, so the halt cost nothing but time. Mailpit: 10
     subjects, 1 copy each.
3. **Shadow mode** — drive an account's score below the quarantine threshold
   - **Pass:** a would-quarantine decision is logged; the flag stays unset; the account keeps sending
   - **This one DOES have a live mechanism, unlike Stage 9 steps 1–4 — point an account at a dead
     port.** The score only moves on a *penalizing* failure reported by a worker, and Mailpit cannot be
     made to reject anything. But `smtp_port: 2599` (nothing listening) makes the worker report
     `connect_refused`, which classifies as `timeout` — and `timeout` **is** penalizing. Read
     `classify.go` carefully here: its row-6 comment says "no penalty", which means no *flag* action
     (no quarantine, no pause). It still routes through `applyClassifiedFailure` →
     `applyAccountEffect`, so `consecutive_failures` climbs and the score is recomputed. Those two
     senses of "penalty" are easy to conflate and cost a detour.
   - **Use a FRESH account on its OWN tenant, or the score will not go low enough.** The formula
     (`account_score.go`) is
     `100 − 60·min(1, cf/5) − 15·daily_prox − 25·recency`. An account with a recent success carries a
     recency penalty near 0, so even a saturated `cf=5` bottoms out around 40 — above the threshold of
     30, and the decision never fires. An account that has **never succeeded** takes the full 25
     recency penalty, so `cf=4` → **27** and it trips. A fresh isolated tenant (own user, own
     dedicated pool, one account) also keeps every earlier stage's fixture accounts untouched, and
     sidesteps the one-campaign-per-user guard. `CAMPAIGN_USER_ID=<uuid> tests/manual/launch-campaign.sh`
     targets it.
   - **The marker to look for** (`account_effect.go`, deliberately schema-neutral — no new column, no
     new `action` enum value): `account_events` with `actor='system'`, `action='updated'`,
     `from_flags = to_flags` (the equality IS the "nothing was mutated" signal), and
     `reason = 'shadow_would_quarantine: score=<X> threshold=<T> trigger=score|auth_failure'`.
   - **Observed (live, 2026-07-29).** Fresh tenant + fresh account on port 2599, mode at its seeded
     `shadow`. Score fell **100 → 27 → 15** and stayed at 15; `consecutive_failures` reached **10**.
     Seven `shadow_would_quarantine` rows, every one with `from_flags = to_flags`
     (`score=27 threshold=30 trigger=score`, then `score=15 …`), and `quarantined` **false** the whole
     time — decided, never acted. Jobs requeued normally (`queued`, `attempt_count=2`,
     `last_error_class='timeout'`), so the *job* policy is untouched by the account-effect mode.
   - **Then prove the third leg — "the account keeps sending" — don't just assert the flag is unset.**
     A score of 15 is only an *ordering* term in `eligibility.go`
     (`ORDER BY account_status_score DESC, id ASC`), never a filter, so a low score must not block a
     claim. Restored the port to 1025: all **6 jobs reached `sent`, 1 Mailpit copy each by exact
     subject**, and **4 of the 6 went out on the score-15 account itself** (the other 2 landed on the
     shared-fallback pool — see the caveat below). So the low-score account did keep sending, by
     attribution and not by inference. Its score then recovered to 100 and `cf` to 0 on those
     successes, which is why step 4 needs a *second* fresh account rather than reusing this one.
   - **Check `assigned_account_id`, not just the job count — `default_shared_pool_id` is set on this
     stack.** `resolvePoolChain` appends the config default shared pool after the tenant's dedicated
     pool, so an "isolated" tenant is only isolated for the *dedicated* hop; once its own account is
     the low-scoring one, `ORDER BY account_status_score DESC` prefers the shared pool's score-100
     accounts. That is correct routing, but it means "6 jobs sent" alone does not tell you *which*
     account sent them:
     ```bash
     docker compose exec -T postgres psql -U postgres -d platform -c \
       "SELECT a.smtp_username, j.state, count(*) FROM send_managed.jobs j
          LEFT JOIN send_managed.accounts a ON a.id = j.assigned_account_id
         WHERE j.user_id = '<tenant>' GROUP BY 1,2"
     ```
     It also explains a side effect worth expecting: the Stage 7 shared accounts (`step7@`, `s7b@`)
     pick up traffic from these runs, so their `daily_sent_count` moves even though the campaign
     belongs to a different tenant.
4. Set `enforce` and repeat
   - **Pass:** the flag *is* set on the identical condition. **Return to `shadow` afterwards.**
   - **Observed (live, 2026-07-29).** `PUT /v1/admin/config/autoquarantine_mode` → `enforce` (the
     response confirms `default_value` still `shadow` — Rule 1: defaults are never mutated). A second
     fresh tenant + fresh never-succeeded account on port 2599 — the *identical* condition — reached
     score **27** at `cf=4` and this time `quarantined` **= true**, with a **real** event:
     `action='quarantine'` (not `'updated'`), `reason='score_below_threshold'`, and
     `from_flags ≠ to_flags`. **Exactly one** such row, not one per failing outcome — the decision is
     idempotent once the flag is set.
   - **The two runs also differ in a way worth noticing:** enforce stopped the bleeding at `cf=4`,
     where shadow let the identical account run on to `cf=10`. That gap is precisely what the shadow
     ladder is for — you see the decision you *would* have taken before granting it the power to act.
   - **And the quarantine actually took effect on routing, which is the half a flag check misses.**
     All **6** of the enforce tenant's jobs completed on the *shared fallback* accounts (`step7@`,
     `s7b@`, 3 each) and **zero** on the quarantined account — its `daily_sent_count` stayed at 4, the
     four attempts from before the flag was set. Enforce did not merely write a boolean; the
     eligibility predicate stopped selecting the account, and the jobs re-routed instead of failing.
     (This is the same live-stack behaviour `exitcriteria_test.go::TestExit_AuthFailureThenSendsViaDifferentAccount`
     pins for the auth-failure class.)
   - **Returned to `shadow` and verified it** (`value` = `{"v": "shadow"}` = `default_value`), then
     restored the fixture account (port 1025, `unquarantine` → 200; its jobs had already drained via
     the fallback pool, leaving zero non-terminal work).
5. **Kill switches** — disable auto-pause, then content variation
   - **Pass:** each disables only its own behaviour
   - **The two halves are not equally testable live, and the asymmetry is worth knowing before you
     start.** `variation_enabled` closes fully on the live stack (below). `autopause_enabled` does not:
     it has exactly ONE call site (`account_effect.go:99`, `cfg.AutopauseEnabled && pauseDecision(...)`)
     and `pauseDecision` returns false for every class except `throttle` — which requires a 4xx
     carrying a rate signal, the one thing Mailpit cannot emit. The dead-port trick that closes steps
     3–4 yields `timeout`, never `throttle`. So this half is the same missing-controllable-sink gap as
     Stage 9 steps 1–4, and it is covered by
     `shadowmode_test.go::TestKillSwitch_AutopauseDisablesOnlyAutopause` — which is a genuine
     scoped-switch test, not just an off-switch test: with the switch off a 450 rate-limit reply leaves
     the account unpaused *and* confirms it did not quarantine either (a different subsystem,
     unaffected), then a **paired positive control** on a fresh account with the switch back on DOES
     pause with `reason='throttle'`. Re-run green here (1.89s, real Postgres).
   - **For the variation half, first turn some toggles ON — otherwise the test is vacuous.** Every
     `variation_*` sub-toggle ships `false` by default, so on a stock stack "variation off" and
     "variation on" produce identical output and flipping the master switch proves nothing (Rule 10).
     Enable a few *observable* ones first: `variation_comment_injection`,
     `variation_mime_boundary_rotation`, `variation_dynamic_preheader`, plus
     `variation_comment_intensity=high` and `variation_header_style=thunderbird`.
   - **Observed (live, 2026-07-29).** With those five on, 4 messages came out **1160/1160/1148/1159
     bytes** — variable, and each carrying all three artifacts visibly in the raw source: a rotated
     `_NextPart_17822414ddfd9798383a_` MIME boundary, a hidden preheader
     `<span style="display:none;…">A brief note for you…</span>`, and an injected `<!--35066c06-->`
     comment. Then `variation_enabled=false` **alone**, leaving all five sub-toggles at `true`: the
     next 4 messages were **614 bytes each, all four identical**, with **zero** `_NextPart_`, zero
     `display:none`, zero `<!--…-->` across all four. The header *order* also reverted to baseline even
     though `variation_header_style` was still `"thunderbird"` — which is the real claim: the master
     switch acts at the snapshot level (`variationSnapshot` returns `nil`, so the worker's own
     identity-function default applies), it does not walk around flipping sub-toggles.
   - **And "only its own behaviour":** those 4 jobs went `sent` with `attempt_count=0` and
     `last_error_class` NULL — disabling variation does not touch dispatch, routing, or the send path.
     Diffed pairwise, the only remaining differences between the four are `Message-ID`, the timestamp,
     and the body's own message number. Every *structural* difference was variation, and it is gone.
   - **Restore all of it, and verify rather than assume** — one query catches any key left drifted:
     ```bash
     docker compose exec -T postgres psql -U postgres -d platform -c \
       "SELECT key, value, default_value FROM send_managed.config
         WHERE key LIKE 'variation%' AND value <> default_value"   # must return 0 rows
     ```
     Returned 0 rows after restoring, so all 21 `variation_*` keys are back at their defaults.
6. **Health endpoint** — seed a known state and read it
   - **Pass:** the numbers match reality, not just a 200
   - **"Matches reality" is not enough on its own — PERTURB the state and confirm the numbers move.**
     A snapshot that agrees with the DB proves the queries run; it does not prove they are wired to
     anything. Read it once, change something via the admin API, read it again (Rule 10).
   - **Observed (live, 2026-07-29), every field cross-checked against its own query.** `queue` 328
     sent / 8 failed, `accounts` 6 healthy / 0 paused / 0 quarantined / 0 disabled, `workers` 2 active /
     0 stale, `shadow.would_quarantine_count` 9 with the nine `recent[]` entries carrying the real
     score/threshold pairs (27 then 15×8), `errors_last_window` `timeout: 18` + `unknown: 15`, and all
     five pools' `eligible_accounts` (1/1/1/1/2) — every one an exact match.
   - **Then the perturbation:** `pause` one account and `disable` another → `healthy` 6→**4**,
     `paused` 0→**1**, `disabled` 0→**1**, and the two affected dedicated pools flipped to
     `eligible_accounts: 0` with `headroom: "exhausted"`. The endpoint tracks live state, not a cache.
   - **`sends_last_window` and `send_logs` legitimately DISAGREE — don't file it as a bug.** The
     snapshot read `sends_last_window: 136` while `SELECT count(*) FROM send_managed.send_logs` over the same
     window returned **152**. `queryTerminalCountInWindow` counts *jobs that reached `sent`*
     (`state='sent' AND completed_at >= …`), whereas `send_logs` gets a row **per attempt**. Reconciled
     exactly: `count(DISTINCT job_id)` over those 152 rows is **136** — the 16 extra rows are the
     failed dead-port attempts from steps 3–4. Two correct numbers measuring different things.
   - **`accounts.quarantined: 0` alongside an `account_quarantined` active condition is also correct.**
     `queryAccountQuarantinedConditions` is **window-scoped by design** (`health_alerts.go`): a
     quarantine is a point-in-time `account_events` row, not a re-derivable current row state, so
     "active" means "fired within `health_metrics_window_sec`". An account quarantined and then
     un-quarantined inside the window still shows the condition while the counter reads 0. The other
     two conditions *are* live-state derived (`pool_exhausted` from the snapshot, `worker_death` from
     `workers.status='stale'`), so only this one behaves that way.
7. **Alerts** — trigger pool exhaustion, an account quarantine, and a worker death
   - **Pass:** each appears in the endpoint's active conditions and fires the webhook; with no webhook configured, conditions still surface and nothing errors
   - **You need an HTTP receiver ON THE COMPOSE NETWORK.** `localhost` from the host is not reachable
     from inside the Control container, so a host-side listener never sees a delivery.
     [`tests/manual/webhook-receiver.py`](../tests/manual/webhook-receiver.py) exists for this — it
     logs each POST body **verbatim**, which is the point: the payload crosses an untrusted egress
     boundary (Rule 5 names `alert_webhook_url` explicitly), so the test has to read exactly what went
     out. Run it and point the config at it:
     ```bash
     docker run -d --name s10hook --network coldmail-send-managed_default \
       -v "$(pwd -W)/tests/manual:/rcv:ro" python:3.12-alpine python /rcv/webhook-receiver.py
     # then: PUT /v1/admin/config/alert_webhook_url  {"value":"http://s10hook:9099/hook"}
     docker logs s10hook
     ```
     *(On Windows/Git Bash use `$(pwd -W)` and prefix `MSYS_NO_PATHCONV=1`, or the container-side mount
     path gets rewritten into a Windows path and the script "isn't found". A `/tmp` file also mounts as
     an empty directory under Docker Desktop — mount from the project directory, which is shared.)*
   - **The webhook fires on each `/v1/admin/health` poll, on RISING EDGES only** (`health_handler.go`
     calls `go fireWebhooks(...)`). So nothing is delivered until you read the endpoint — and reading
     it four times still delivers once per subject.
   - **Observed (live, 2026-07-29) — all three conditions, all three webhooks.**
     - **`account_quarantined`** — already true from step 4's enforce run. One delivery; then **4 polls
       total produced still exactly 1 delivery**, proving the `(condition, subject_id)` dedup.
     - **`pool_exhausted`** — this one takes real setup, and the first attempt FAILED to trigger it.
       Two dedicated pools were already `headroom: "exhausted"` from step 6's perturbation, yet no
       condition fired: `queryPoolExhaustedConditions` requires **≥1 job waiting on the pool**, so a
       newly-exhausted pool nobody has routed to correctly stays silent. Launching a campaign didn't
       help either — the shared fallback still had capacity, so the jobs just sent. Then pausing the
       shared accounts *manually* was also wrong: a manual pause is `terminal_blocked` (Stage 9 step
       9), so the 3 jobs went **`failed`**, not `queued`, and again nothing waited. What works is a
       **self_recovering** block — drive every account to its daily limit (`daily_limit =
       daily_sent_count`; note the `accounts_daily_limit_check` floor of 1). Then: `waiting_for_capacity: 3`,
       and **two** `pool_exhausted` conditions — one per pool in that job's resolved chain — each
       carrying `waiting_jobs: 3`, and **two separate deliveries** keyed by their own `pool_id`.
     - **`worker_death`** — `docker compose stop worker2`, then wait `heartbeat_interval_sec` ×
       `heartbeat_missed_threshold` = 120 × 3 = **6 minutes**. Flipped to `status='stale'` at 6m51s;
       `workers` read `active: 1, stale: 1`, and the condition carried `box_id` + `last_heartbeat_at`.
       **It auto-cleared on restart** — worker2 reconnected and the condition vanished on the very next
       poll, because that query reads live `workers.status`, not an event log.
   - **Every payload is two-vocabulary clean, which is the security half of this step.** Verbatim:
     `{"condition":"worker_death","severity":"critical","subject":{"type":"box","id":"2222…"},
     "detail":{"box_id":"2222…","last_heartbeat_at":"…"},"fired_at":"…","module":"send_managed"}` — condition
     code, subject id, timestamps. No credential, no ciphertext, no SQL, no SQLSTATE, no internal
     hostname, no internal reason string.
   - **The no-webhook half needs a FRESH rising edge, or it proves nothing.** Clearing
     `alert_webhook_url` does not reset the dedup set, so re-polling an already-fired condition would
     be skipped by dedup rather than by the empty URL — a vacuous pass. Do it properly: worker2 had
     reconnected (so `clearStale` dropped its `worker_death` key), so stopping it again produced a genuine
     new edge with the URL unset. Result: **HTTP 200**, all four conditions present in
     `active_conditions[]` including the fresh `worker_death`, the receiver's delivery count **still 4**
     (no attempt made — `fireWebhooks` returns before any HTTP on a blank URL), and **zero** webhook
     errors in the Control log. Conditions stay visible; alerting is simply off.
   - **Restore:** clear `alert_webhook_url`, `docker rm -f s10hook`, restart worker2, put `daily_limit`
     back to 500, and re-enable/unpause the fixture accounts. Verified after: queue fully drained (the
     3 waiting jobs completed), 6 healthy accounts, 2 active workers, and `value <> default_value` showing
     only the 3 deliberate dev-stack keys.

---

### Stage 11 — Operational drills

1. **KMS key restore** — back up the local key, destroy the working copy, restore from backup alone, then decrypt an existing credential
   - **Pass:** decryption succeeds from the backup
   - Practise it locally where it costs nothing. In production, losing this key means every stored SMTP credential is permanently unrecoverable.
   - The procedure is `Docs/disaster-recovery.md` §1 — follow it rather than improvising, since the
     point of the drill is to test **the documented route**. Its backup/restore steps work verbatim on
     the dev stack (`gpg --symmetric --cipher-algo AES256`, then `--decrypt`, confirm 32 bytes).
   - **This drill is worthless without the wrong-key negative control, and the reason is a property
     the DR doc states explicitly: the failure is SILENT until send time.** A Control that loads a
     32-byte key of the *wrong* value starts perfectly cleanly, so "the Control came up" proves only that
     the file is the right length. Confirmed live: with a random 32-byte key installed, startup logged
     `kms dev key loaded (sole decryptor; 32 bytes …)`, `readyz` returned
     `{"db":true,"singleton_lock":true,"status":"ready"}`, and the container went `healthy` — nothing
     anywhere said the key was wrong. **Only a completed send proves a key.** So: baseline send → destroy
     the key → install a wrong one → prove the send FAILS → restore → prove the SAME jobs now succeed.
   - **Observed (live, 2026-07-29). PASS.** Baseline: 2 jobs `sent` on the live key. The working copy
     was then `shred -u`'d, leaving the gpg backup as the **only** copy (verified: the path did not
     exist).
     - **Negative control — a random 32-byte key.** Clean start as described above. Two fresh jobs then
       failed at dispatch, repeatedly and honestly:
       `dispatch: decrypt failed for account <id> (job <id>): credential decrypt failed (authentication) — requeued`.
       Jobs sat `queued` with `attempt_count` climbing and **`assigned_account_id` NULL** — requeued, never
       claimed as sent, so Rule 5 held (no invented outcome). **Mailpit `count: 0`** for that tag: zero
       mail escaped, so no plaintext went anywhere. The client `report` showed only
       `state: "queued"` / `pending: 2` — grepping its body for `decrypt|authentication|kms|key|cipher|nonce|SQLSTATE|send_managed_app`
       found **0 matches**, and the internal reason stayed in the Control log (Rule 5, both directions).
       The log itself leaked no key material either: **0 matches** for either key's hash, `cred-dek`,
       `ciphertext`, or `nonce`.
     - **Restore, from the backup alone.** `gpg --decrypt` → **32 bytes**, SHA-256 **identical** to the
       pre-drill original (`4c9ca879…`). Note the DR doc's `mode 0400` step does **not** take effect on
       Windows — `chmod 0400` lands as `-r--r--r--` (0444), because the Git-Bash/NTFS layer does not carry
       the group/other bits. Harmless here (the container mounts it `:ro` and the real control is the
       host filesystem ACL), but do not read a dev-stack `ls` as evidence that the production
       permissions step works; verify that one on the Control host.
     - **The proof that counts.** After the restart, the **same two job ids** that had just failed
       authentication went `sent` with a `provider_message_id`, **exactly one** Mailpit copy each and
       **one** `send_logs` row each — no duplicate from the failed attempts. Then all six accounts'
       credentials were exercised, not just the two the shared pool happened to pick: 12 shared-pool
       jobs (`s7b@` 6, `step7@` 6) plus one job per dedicated tenant via `CAMPAIGN_USER_ID`
       (`s8ded@`, `s8other@`, `s10shadow@`, `s10enf@`) — **all `sent`**, every credential written
       *before* the drill. **Zero** decrypt failures after the restore.
   - **Trap when counting the "after" state:** `docker logs --since <ts>` on a container that has been
     **restarted** returns the whole stream, not just the tail — it reported 93 decrypt failures that
     were all pre-restart. Use `docker logs -t` and filter on the real timestamp
     (`awk '$1 > "<restart ts>"'`), which showed the true count of **0**, with the last failure 2s
     *before* the restart.
2. **Credential rotation** — rotate the key and re-encrypt existing credentials
   - **Pass:** sending continues; `credential_key_id` reflects the new key; old ciphertexts migrate
   - The procedure is `Docs/disaster-recovery.md` §3 ("Rotating the master key"), and its **ordering is
     load-bearing, not stylistic**: freeze dispatch BEFORE cutting the key over, because a Control
     resolves exactly one `credential_key_id` — there is no keyring, so between the cutover and the last
     re-entry **every** existing row is unreadable. "Re-encrypt" is **re-entry**, not a server-side
     transform: the admin surface is Encrypt-only (Rule 6), so you must have every plaintext password.
   - **"Sending continues" cannot mean an empty queue.** Park real jobs in the freeze first, or the
     step passes vacuously — a drained queue proves nothing about jobs that span the rotation. Queue
     them *after* setting `dispatch_enabled=false` and assert they are still `queued` at each stage.
   - **Observed (live, 2026-07-29). PASS, both directions.** New 32-byte key generated and gpg-backed-up
     **before** use (§3 step 1 — a key you rotate to and then lose is the §1 disaster with extra steps).
     `dispatch_enabled=false` → `dispatch_paused: true` confirmed before continuing; **4 jobs launched
     into the freeze** and held `queued`.
     - **Cutover.** v2 key installed, `CREDENTIAL_KEY_ID=cred-dek-v2`, Control recreated. It came up
       `ready`/`healthy` with all 6 rows still labelled `cred-dek-v1` — the "everything is unreadable"
       window, entered deliberately.
     - **The DR doc's claim about the error is exact, and this is the sharper of the two failure
       modes.** Briefly unfreezing produced
       `cannot resolve credential_key_id "cred-dek-v1" (this Control resolves "cred-dek-v2") — requeued`
       — an **explicit rotation error naming both labels**, provably distinct from the generic
       `credential decrypt failed (authentication)` that a wrong key of the *right* label gives (step 1's
       negative control). `credential.go:57` short-circuits on the label before it ever touches AES, so a
       rotation mistake is never a silent wrong-key attempt. Jobs **requeued**, all 4 still `queued`,
       **Mailpit `count: 0`** — nothing failed, nothing sent, nothing invented.
     - **Re-entry.** `PATCH /v1/admin/accounts/{id}` with `smtp_secret` × 6 → all `200`. §3 step 4's
       tracking query showed `cred-dek-v2|6`; §3 step 5's "rows not on the new label" query returned
       **empty**. Fresh nonce per write confirmed: **6 distinct nonces and 6 distinct ciphertexts** for 6
       accounts, even though every plaintext was identical.
     - **The proof (§3 step 6).** Unfrozen, the **same 4 job ids** parked since before the new key
       existed went `sent` — 4 Mailpit copies and **exactly one `send_logs` row each** despite
       `attempt_count = 3`, so the failed pre-rotation attempts left no duplicate. Then all six accounts
       were exercised under v2 (the 2 shared from the parked jobs + one job per dedicated tenant via
       `CAMPAIGN_USER_ID`) — all `sent`, **zero** decrypt failures. Audit trail: **6** `account_events`
       `updated`/`admin` rows with **6 distinct `request_id`s**. Leak grep for the plaintext and both key
       hashes: **0 matches**.
     - **Negative control that the retired key is genuinely dead (§3 step 7).** Restoring the **v1** key
       while leaving the **v2** label gave `credential decrypt failed (authentication)` — the *generic*
       GCM failure this time, since the label matched and only the key was wrong. 47 failures, jobs
       requeued, **Mailpit `count: 0`**. Both halves of "label vs key" therefore fail closed, and by
       *different* errors.
     - **Rollback (§3's abandon path) exercised, and it is symmetric to step 4 as documented.** Froze,
       restored the v1 key + label, re-entered all 6 credentials (now the v2 rows are the unreadable
       ones), unfroze → a fresh campaign `sent`. This is also the correct END STATE for the dev stack:
       `scripts/dev.sh:63` rewrites `.env` to `cred-dek-v1`, so leaving the stack on v2 would silently
       strand every credential the next time that script runs.
   - **Trap:** `chmod 0400` from step 1 makes the key file read-only, so step 2's `cp` of the new key
     fails `Permission denied` — and if you chain it with `&&` the whole rotation silently does not
     happen (the `sed` on `.env` never runs either, so the label stays v1 and nothing looks wrong).
     `chmod 0600` first, and verify with `sha256sum` that the file you think you installed is installed.
   - **Trap:** `docker compose restart control` does **not** pick up a changed `.env` — env vars are fixed
     at container creation. `CREDENTIAL_KEY_ID` needs
     `docker compose up -d --no-deps --force-recreate control`, then confirm with
     `docker exec <control> printenv CREDENTIAL_KEY_ID`. A `restart` leaves the old label live and the
     rotation appears to do nothing.
3. **Add a worker** — provision a third identity (`tools/provision-worker/bin/provision-worker new --id dev-worker-3`), seed its row into `send_managed.boxes`, and start a container with that `WORKER_ID` + `WORKER_TOKEN`. It can reuse the dev cert: the cert is transport, the token is the authority.
   - **Pass:** it registers, appears in health, and receives work with **no Control reconfiguration**. This is your scaling path — confirm it is genuinely this simple.
   - A worker needs its **own identity**, not just another container. Adding a replica that reuses an existing `WORKER_ID` does not add capacity — the two connections evict each other.
   - **"No Control reconfiguration" is the whole claim, so pin the Control before you start.** Record its
     `StartedAt` and `RestartCount` and re-check both at the end; if the Control never restarted, the
     token gate is provably DB-driven and not env-driven. Confirmed live: `StartedAt`
     `2026-07-29T05:09:30`, `restarts=0`, **unchanged across the entire step**, while its env held only
     worker-1 material — so worker 3 working is not explained by anything the Control was told.
   - **Observed (live, 2026-07-29). PASS.** `provision-worker new --id 33333333-3333-3333-3333-333333333333`
     printed `WORKER_ID` / `WORKER_TOKEN` (once) / `DEV_WORKER_TOKEN_HASH`; the hash was seeded into
     `send_managed.boxes`; the container started with the dev cert **reused**. It registered, health went to
     `active: 3`, and it received real work — 15 jobs split 4/9/2 across the three workers, worker 3's sends
     genuine (`provider_message_id` present, one `send_logs` row each, `sending→sent` written by actor
     **`worker`**, not by the dispatcher).
   - **Re-verify the worker's security envelope on the NEW worker, not just the original** — a hand-started
     container is exactly where a mount slips in. Confirmed: no DB or KMS variable in its env, only
     `worker.crt` / `worker.key` / `ca.crt` mounted, **no `/secrets/kms/`**, no `control.key` or `ca.key`, and
     **0** credential matches in its log (Rule 6).
   - **Trap — the duplicate-`WORKER_ID` eviction loop is loud but easy to misread as capacity.** A replica
     reusing worker 1's id drove **410 registrations in ~40 s** while health stayed at `active: 3`, and it
     returned to 0/s the moment the replica was removed. Watch the registration *rate*, not the count.
   - **Traps (Windows / Git Bash):** `MSYS_NO_PATHCONV=1` is needed on `docker exec <container> ls /path`
     too, not only on `docker run -v` — without it Git Bash rewrites the **container-side** path and you
     get `C:/Program Files/Git/secrets/mtls/: No such file or directory`, which reads like a missing
     mount. And `grep -P` is unavailable in this locale (`-P supports only unibyte and UTF-8 locales`),
     so parse the provisioner output with `sed`/`awk`.
4. **Remove a worker** — stop it cleanly
   - **Pass:** in-flight work recovers; the others absorb the load
   - **You cannot catch in-flight work by polling — force the hold.** Three attempts failed vacuously
     before this was accepted: 30 jobs at `send_delay` 0–1 s all completed before the first poll; at 6–8 s
     jobs wait `queued` *between* sends rather than sitting in-flight; and even with the other two workers
     stopped, a `docker compose exec` poll is a ~1 s round-trip and still lost the race. Per Rule 10 a
     test that never observes the state it claims to test is not a test. What works is a **blackhole
     listener** — accepts TCP, never speaks SMTP — pointed at from the account:
     ```bash
     MSYS_NO_PATHCONV=1 docker run -d --name s11hole --network coldmail-send-managed_default \
       alpine sh -c 'while true; do nc -l -p 2525 -e sleep 300; done'
     # then: UPDATE send_managed.accounts SET smtp_host='s11hole', smtp_port=2525 WHERE …
     ```
     A **dead** port or the Control's own mTLS port both fail *fast* instead (class `timeout`) and hold
     nothing. Assert the hold before stopping anything: state `sending`, a future `lease_expires_at`,
     and Mailpit `count: 0` so no outcome could already have arrived.
   - **Observed (live, 2026-07-29). PASS, both halves.** Precondition captured immediately before the
     stop: job `347281de…` `sending` on worker 3, `attempt_count=1`, `attempt_token=2`,
     `lease_expires_at 05:33:42+00`, Mailpit `count: 0`.
     - **Clean stop.** `docker compose stop` → **exit 0**, `oomkilled=false`. Immediately after, the job
       was still `sending` with `attempt_token` unchanged — the vanished worker's work was **not** resolved
       either way. Rule 5 held: no outcome was invented for a worker that is no longer there to report one.
     - **Recovery is by lease expiry, attributed honestly.** The `job_events` trail is the evidence, and
       the actor column is the point:
       ```
       05:31:42  assigned→sending  dispatcher  worker=3333…
       05:33:47  sending→queued    reclaim_loop worker=3333…  timeout  "lease expired"
       05:35:47  queued→assigned   dispatcher
       05:35:47  assigned→sending  dispatcher  worker=1111…
       05:35:47  sending→sent      worker         worker=1111…
       ```
       The requeue is written by **`reclaim_loop`**, never by `worker`, and it lands 5 s after the lease it
       was waiting on. The job then re-dispatched to a **surviving** worker and sent.
     - **Exactly-once, and the row count that looks wrong but is not.** The reclaimed job has **2
       `send_logs` rows** — that is per-**attempt** logging by design (`outcome.go` writes one from each
       of the accepted / bounced / failed paths), and the two rows are distinguishable: the blackhole
       attempt has a **NULL `smtp_response`**, the successful one carries `250 2.0.0 Ok: queued as …`.
       The exactly-once evidence is elsewhere and is unambiguous: **one** Mailpit copy and **one**
       `provider_message_id`. Do not assert `count(send_logs)=1` on a retried job.
     - **Nothing stranded.** Every one of the 13 jobs worker 3 had touched ended `sent` — 12 on worker 3 before
       the stop, the one in-flight job on worker 1 — and a cluster-wide sweep for non-terminal jobs
       (`state NOT IN ('sent','failed','bounced')`) returned **empty**.
     - **The others absorb the load.** With worker 3 gone, a fresh 12-job campaign went **12/12 `sent`**,
       split 10/2 across workers 1 and 2, **0** assigned to the removed worker, every job with a
       `provider_message_id`, **12** Mailpit copies and **12** `send_logs` rows for 12 distinct jobs — no
       duplicate, no shortfall.
     - **Deregistration needs no admin action.** Worker 3 aged from `active` to **`stale`** on its own
       (`heartbeat_interval_sec` 120 × `heartbeat_missed_threshold` 3) with no operator step, and while
       it was still nominally registered the dispatcher simply never picked it.
   - **Restoring the stack:** point the accounts back at `smtp-sink:1025`, remove the blackhole and the
     worker-3 container, `docker compose start worker worker2`, and return any `send_delay_*` /
     `smtp_operation_timeout_sec` you widened to their `default_value` (compare `value = default_value`
     in `send_managed.config` rather than trusting memory — Rule 1 keeps the default alongside the live value
     precisely so this check is one query).
5. **Graceful shutdown** — `SIGTERM` the Control mid-campaign
   - **Pass:** stops claiming, drains, leaves in-flight work recoverable; next boot recovers everything; no job stranded
   - **This step has TWO branches and only one of them is reachable by accident.** With nothing in
     flight the drain finishes in **1 ms** and logs `nothing in flight — drain complete` — honest, but
     it proves nothing about draining. Force the in-flight branch with the blackhole listener from
     step 4 (a job genuinely `sending`, Mailpit `count: 0`) or the step is half-tested. Keep some work
     `queued` too, or stop-claim has nothing to prove either.
   - **Observed (live, 2026-07-29). PASS — and forcing the second branch found a real dev-stack
     defect, fixed in the same change (see below).**
     - **Stop-claim (16 jobs, 7 still `queued` at the signal).** The log sequence is exactly §4.1:
       `shutdown signal received` → `DRAINING — dispatch stops claiming … the worker link and outcome
       handler stay up` → `drain complete` → `shutdown complete (no job was requeued or marked on the
       way out — §4.2 leave-in-place)`. With the Control down the queued remainder **stayed `queued`**;
       nothing was `assigned` or `sending` anywhere. The workers logged
       `connect attempt N not ready (getaddrinfo ENOTFOUND control); retrying` — they wait, they do not
       invent. On restart **all 16** reached `sent`, with **16** Mailpit copies and **16** `send_logs`
       rows for 16 distinct jobs. Nothing stranded, nothing duplicated.
     - **Leave-in-place with real in-flight work, and it holds even under SIGKILL.** With one job
       `sending` on the blackhole, the shutdown reached
       `draining 1 in-flight job(s), waiting up to 30s` and was then killed mid-drain. The job was left
       **exactly as stored** — still `sending`, `attempt_token` unchanged, original `lease_expires_at`,
       `provider_message_id` NULL, and its `job_events` trail ending at `assigned→sending` with **no
       terminal event**. §4.2 is real: a job that may already have hit `250` is never eagerly requeued.
     - **The next boot recovers it, and the reconnect window beats reclaim to it.** The restart logged
       `startup rescan: extended 1 orphaned in-flight lease(s)` — exactly one, matching what was left.
       Recovery then came from the **worker replaying its own buffered outcome** (`sending→queued`
       attributed to actor **`worker`**, `error_class` `timeout`), not from `reclaim_loop` — which is the
       better path of the two §4.3 offers, and the reason the rescan extends leases instead of
       reclaiming immediately. That job later went `sent` with **exactly one** Mailpit copy.
     - **Final state:** every job across all three campaigns terminal, the cluster-wide
       `state NOT IN ('sent','failed','bounced')` sweep **empty**, and one Mailpit copy per job.
   - **DEFECT FOUND AND FIXED — `docker-compose.yml` could never complete a drain.**
     `shutdown_drain_timeout_sec` is seeded at **30 s**, but Docker's default stop grace is **10 s** and
     the dev compose file declared no `stop_grace_period`. So `docker compose stop control` with work in
     flight exited **137** (SIGKILL) after ~11 s, truncating a drain that was proceeding correctly. The
     invariant was already documented in three places — `config-reference.md` ("Keep below deploy
     SIGTERM→SIGKILL grace"), the `install-and-deploy.md` systemd unit (`TimeoutStopSec=120`), and the
     `operator-runbook.md` row that names this exact failure — the dev stack simply contradicted all
     three. Fixed by adding `stop_grace_period: 90s` to the `control` service (the worker exits promptly, so
     10 s is fine there). **Proven by a paired control, per Rule 10:** the identical in-flight scenario
     went from `exitcode=137`, drain truncated, to `exit 0` with
     `drain complete in 18.076s — every in-flight job reported and resolved normally; NOTHING was
     requeued or invented` — and the resolving event was written by actor **`worker`** during the drain,
     proving the outcome handler really does stay up. Confirm the setting landed with
     `docker inspect … Config.StopTimeout` (expect `90`); `docker inspect -f '{{.HostConfig.StopTimeout}}'`
     errors with `map has no entry for key`, which is a template mistake, not an unset value.
   - **A truncated drain was never *unsafe*, only wasteful** — leave-in-place meant nothing was
     requeued or marked, and the next boot recovered it. That is precisely why the gap survived: the
     symptom is exit code 137 and some avoidable reclaim work, not lost or duplicated mail.
6. **Singleton guard** — start a second Control against the same database
   - **Pass:** it exits cleanly; the first keeps running; no double-dispatch
   - > **Do this step FIRST if the stack has been through Stage 9 step 8, and check the incumbent
     > before you start anything.** Walking Stage 11 in order found the dev stack *already* in a
     > split-control-capable state, left there by the outage drill 56 minutes earlier — and the evidence
     > is destroyed by the Control restart that Stage 11 step 1 requires. Before starting a second Control,
     > read the incumbent's `readyz` and the lock itself:
     > ```bash
     > B1=$(docker compose ps -q control | head -1)
     > docker exec $B1 curl -s http://127.0.0.1:8080/readyz     # singleton_lock must be true
     > docker compose exec -T postgres psql -U postgres -d platform -At \
     >   -c "SELECT count(*) FROM pg_locks WHERE locktype='advisory'
     >        AND ((classid::bigint<<32)|objid::bigint)=9200000001;"   # expect 1
     > ```
   - **Trap that produces a false PASS:** `docker compose exec control …` also matches a
     `docker compose run` container of the same service, and it selects the **newcomer** — whose
     `readyz` says `singleton_lock: true`. The split-control then hides behind the healthy one. Address
     the incumbent by container **id** (`docker compose ps -q control`), never by service name, once two
     containers share the label. Likewise `docker … | tail -3; echo $?` reports **tail's** exit code:
     redirect to a file and read `$?` off the `docker` command itself, or the clean `exit 3` is
     invisible.
   - **Observed (live, 2026-07-29) — the step PASSES on its own terms.** Against a healthy incumbent,
     a second Control (`docker compose run --no-deps --rm control`) waited the full bounded `1m0s`, logged
     `singleton lock not acquired within 1m0s: another Control is active; exiting cleanly`, and exited
     **code 3**. The incumbent stayed `{"db":true,"singleton_lock":true,"status":"ready"}` and
     `healthy` throughout. Zero double-dispatch.
   - **But this step as written does not cover the failure that matters, and walking it found a real
     HIGH defect.** A startup **contention** is not a mid-life lock **loss**, and only the former was
     ever tested. `internal/singleton/singleton.go`'s keeper correctly notices a dead lock connection,
     logs `marking not-held`, and exits without re-acquiring — but that flag is read by **`readyz`
     only** (`cmd/control/main.go:205` hands `guard` to the health server and nothing else; no
     dispatch/reclaim/retention path consults `guard.Healthy()`). So: Stage 9 step 8's outage dropped
     the lock at 00:46:55 (`57P01`), `pg_locks` then held **zero** advisory locks on key `9200000001`,
     and that same process — `RestartCount` **0**, one continuous Control — **kept dispatching for 56
     minutes** (`dispatch: tick pushed 1 job(s)` 01:26:33, `pushed 3 job(s)` 01:43:03). Driven to its
     consequence rather than argued: a second Control started against that database **acquired the lock
     instantly** and came fully up to `control ready`, so two Controls were live on one database with the
     *newcomer* holding the lock — exactly the second dispatch path Rule 7 forbids. Nothing was
     corrupted only because the compose alias `control` still resolved to the incumbent, so both workers
     stayed on it — luck about DNS, not a property of the design. **Filed as `G11-OD-1` + ADR-0011
     (needs-user: exit / pause-dispatch / re-acquire all differ operationally, so per Rule 2 the
     ruling is the user's).** The state was *visible* the whole time — `readyz` 503, container
     `unhealthy`, `FailingStreak` 1189 — just not acted on; an orchestrator whose liveness probe is
     wired to `readyz` would already have restarted into safety.
   - **RULED AND FIXED, 2026-07-29 — option (B), "pause dispatch, bounded re-acquire."** The three
     loops now consult a leadership predicate wired to `guard.Healthy` and pause while the lock is
     unheld (no claim, no push, no reclaim, no sweep), and the keeper re-attempts
     `pg_try_advisory_lock` every `brain_singleton_reacquire_interval_sec` (default 5, seeded by the new
     `send_managed_0023`). The **outcome path stays UP** on purpose — a worker's report on a send this Control
     pushed while it *was* leader must still be persisted (idempotent via `provider_message_id`
     UNIQUE), because refusing it would invent a non-outcome (Rule 5) or force a duplicate. Seven
     behavioral tests in `services/control/internal/dispatch/leadership_test.go` cover it on real
     Postgres, each confirmed to FAIL against a deliberately disabled gate. Details in ADR-0011
     (accepted) and `history/open-decisions.md`.
   - **Restoring the stack (as of the fix):** stop and remove the second Control and confirm `pg_locks`
     shows **1** row for the incumbent. A lockless Control now **recovers on its own** within
     ~`brain_singleton_reacquire_interval_sec` once the lock is free, so the restart is no longer
     required — but it remains the fastest way to force a clean re-acquire, and it is still required if
     that key is `0` (re-acquisition disabled), since the cadence is read once at startup.
     *(Before the fix this read "a lockless Control does not recover on its own — it needs the restart,"
     which was true of the tree as observed.)*
7. **Migration down/up** — roll the latest migration down and back up
   - **Pass:** reversible, no data loss on the down path
   - **Read the down file before you run it, and know what the latest migration touches.** "No data
     loss" is a claim about *your* migration, not a property of the runner: a down path that drops a
     column drops its data no matter how careful the runner is. Here the latest is
     `send_managed_0022_job_events_error_context`, which is **trigger-bodies-only** — two
     `CREATE OR REPLACE FUNCTION`s (same signatures, so `send_managed_0008`'s triggers keep pointing at
     them) plus one helper predicate. No table, column, constraint, enum, index, grant, or row is
     touched, so no fixture data from Stages 7–10 is at risk. A migration that *did* drop a column
     would need the fixtures rebuilt afterwards, or the whole stack reset.
   - **Down is dev-gated and fails closed.** `send-managed-migrate down <NNNN>` requires
     `SEND_MANAGED_MIGRATE_ALLOW_DOWN=true` or it refuses with
     `down is forbidden unless SEND_MANAGED_MIGRATE_ALLOW_DOWN=true (forward-only in prod)`. It rolls back
     **exactly one** version, and needs the version argument.
     ```bash
     MSYS_NO_PATHCONV=1 docker compose run --rm --no-deps \
       -e SEND_MANAGED_MIGRATE_ALLOW_DOWN=true -e MIGRATE_SKIP_PREFLIGHT=true \
       migrate down 0022
     ```
     Use `--no-deps` — see Part 2 on how a bare `docker compose run` can recreate `postgres` and wipe
     the fixtures out from under you.
   - **"No data loss" has to be MEASURED, not eyeballed.** Take a row-count fingerprint across every
     table before the down, and diff it after. Anything less passes vacuously.
   - **Observed (live, 2026-07-29). PASS.** `rolled back 0022`, exit 0.
     - **Fingerprint before and after the down was IDENTICAL** — `accounts` 6, `jobs` 487,
       `job_events` 2535, `send_logs` 500, `pools` 5, `workers` 3, `config` 53, `account_events` 62. The
       only differences anywhere: `schema_migrations` 23 → 22 (the `0022` row deleted, as intended)
       and the helper function dropped. **Zero rows of real data lost.**
     - **The down reverted BEHAVIOR, not just objects** — which for a trigger migration is the part
       that matters, since a `CREATE OR REPLACE` that silently no-ops leaves a green run and an
       unchanged database. Both trigger bodies were confirmed back to their `send_managed_0008` form
       (`prosrc LIKE '%v_is_error%'` → **false** for both), i.e. the down file **deliberately
       reinstates the defect 0022 fixed**, which is what "reversible" must mean: a down that keeps
       half the up's behavior is not a reverse.
     - **The Control kept running on the downgraded schema** — `readyz` `ready`, and a fresh 2-job
       campaign went `sent`. This also confirms the down file's own compatibility claim: a **post-0022
       Control on a pre-0022 schema still reports a correct `errors_last_window`**, because
       `queryErrorsInWindow` filters on transition shape in Go independently of the trigger. Verified
       rather than trusted — every error-tagged row inside `health_metrics_window_sec` was genuinely
       error-shaped (`to_state IN ('failed','bounced')` or a requeue): `timeout` 9 and `unknown` 151,
       **none** of them a success or a forward transition.
     - **Up restored everything.** `applied 0022_job_events_error_context`, back to version `0022`,
       helper function present, both trigger bodies gated again, all functions still owned by
       **`send_managed_owner`** (Rule 6 — a down/up must not quietly reassign ownership to the migrating
       superuser, or `FORCE ROW LEVEL SECURITY` stops enforcing). The helper answers all six
       transition shapes correctly (genesis `f`, `queued→assigned` `f`, `sending→sent` `f`, requeue
       `t`, `failed` `t`, `bounced` `t`), and a fresh campaign's events came back **fully untagged** —
       `sending→sent` carrying `NULL` error context, the exact row whose spurious tag 0022 exists to
       remove. `up` is idempotent: a second run printed `no pending migrations (already current)`, and
       `status` showed `23 applied, 0 pending`. All 6 CI gates green afterwards.
   - **Note on a reporting anomaly seen earlier in this walkthrough and NOT reproduced here:** `up`
     once printed `no pending migrations (already current)` while actually applying work. On this run
     it reported honestly in both directions (`applied 0022…` when it applied, `no pending` only when
     genuinely current). Left unfiled for want of a reproduction — if you see it, capture the exact
     invocation and `status` output before anything else.

---

### Stage 12 — Content variation

> **READ THIS BEFORE STEP 1 — every step in this stage is vacuous on the default body.** Most
> variation is **structural**: comment injection fires only after a closing `</tag>`
> (`body.replace(/(<\/\w+>)/g, …)` in `services/worker/src/variation-content.js`), CSS micro-variation
> only on `padding`/`margin` px declarations, URL params only on `href`/`src`. The default fixture
> body — `Body for <tag> message 1.` — contains **none** of those, so every one of those toggles is
> a **no-op** on it. A byte-comparison then "proves" all-toggles-off equals toggles-**on**, and the
> whole stage passes without testing anything. That is not hypothetical: it is what happened on the
> first attempt at step 1, and only the negative control caught it. Use an HTML body:
>
> ```bash
> export HTMLBODY='<html><body><div style="padding: 12px; margin: 8px"><p>Hello.</p><p>Visit <a href="https://example.test/landing?ref=abc">our page</a> or see <img src="https://example.test/img/logo.png"></p></div></body></html>'
> CAMPAIGN_BODY="$HTMLBODY" bash tests/manual/launch-campaign.sh s12off 2
> ```
>
> `CAMPAIGN_BODY` exists in `launch-campaign.sh` for exactly this reason. Before trusting any PASS
> here, run the negative control: turn **one** toggle on and confirm the bytes now **differ**. If
> they do not, your harness is blind and the PASS is worthless.
>
> **Comparing raw messages needs a binary normalizer.** Mailpit stamps `Return-Path`, `Received`,
> `Message-ID` and `Date` per delivery, so raw bytes never match on those lines alone — strip them.
> Do it in **binary** mode (`open(...,'rb')`, `rb''` patterns, `sys.stdout.buffer.write`): Python
> text mode rewrites `\r\n` and silently corrupts the very bytes under test.

1. All toggles off; capture the raw message
   - **Pass:** byte-identical to the non-variation send path
   - **Observed: PASS (second attempt — the first was vacuous, see the worker above).** Two ways of
     saying "off" must agree, and they do. `variation_enabled=false` (master kill-switch — the Control
     sends **no** variation block at all: `variationSnapshot` returns `nil`, `variation.go`) produced
     a message byte-identical after normalization to the one sent with `variation_enabled=true` and
     **all sub-toggles at `default_value`** — 454 bytes, sha256 `9db8dbd593c8…`, on the HTML body.
     That the two paths converge is the point: the stack's shipped default **already is**
     all-toggles-off, so "off" is not a special mode an operator has to find.
   - **Negative control (the whole reason this PASS means anything).** With
     `variation_comment_injection=true` + `variation_comment_intensity=high` on the **same** body, the
     message went to 499 bytes, sha256 `bbc7439d0ab9…`, differing in exactly three places — an
     HTML comment after `</p>`, after `</a>`, and after `</html>`:
     ```
     > <!--49e3923c-->
     > <p>Visit <a href="…">our page</a>
     > <!--f7ced431--> or see <img src="…">
     > </html>
     > <!--91dd5ab0-->
     ```
     On the **default** body the same toggle produced a byte-identical message. Same config change,
     same assertion, opposite verdicts — which is precisely why the first PASS was thrown out.
2. Enable variation; send identical content twice
   - **Pass:** different bytes, identical rendering
   - **Observed: PASS.** Three toggles on (`variation_comment_injection`, `variation_css_micro`,
     `variation_unique_url_params`), two recipients, and — deliberately — a body with **no `%d`**, so
     the two rendered bodies are literally the same string and any difference is variation alone.
     475 vs 466 bytes, different hashes. Every delta is variation-attributable and
     rendering-neutral:
     ```
     < <div style="padding: 11px; margin: 7px">      > <div style="padding: 13px; margin: 8px">
     < …/landing?ref=abc&_ref=388aba64               > …/landing?ref=abc&_ref=166f7362
     < …/logo.png?_v=f52dfa                          > …/logo.png?_v=0e67d5
     ```
     ±1px on padding/margin (`variation_css_magnitude_px` = 1) is invisible to a reader; the added
     params are inert (step 6). **Trap:** the diff also showed different `From:`/`To:` — that is
     **not** variation. The two jobs routed to different accounts (`step7@` vs `s7b@`), which is
     correct pool behaviour. Read a variation diff with the routing in hand or you will chase a
     phantom.
3. Send the same `job_id` twice
   - **Pass:** byte-identical (deterministic seeding)
   - **Observed: PASS.** Re-queued the **same** `send_managed.jobs` row (id preserved ⇒ same variation
     seed: `prngFor(jobId, "comments")`), let it dispatch again, and compared the two Mailpit copies:
     475 bytes each, sha256 `ff52b48ed290…` both — **byte-identical**, under two different
     `provider_message_id`s (`0BAlDhnw…`, then `0NCrGb64…`).
   - **Why this is not vacuous:** step 2 already proved a *different* `job_id` on the *same* body
     yields *different* variation bytes. So the seed demonstrably depends on the id, and identical
     output for an identical id is determinism rather than an inert code path.
   - **Trap — the schema fights a hand-rolled re-send, correctly.** Forcing a second send by
     `UPDATE`ing a `sent` job back to `queued` trips three guards in a row, and each is a feature:
     `attempt_token` is `NOT NULL` (do not null it — the dispatcher mints a fresh one on claim);
     `jobs_completed_at_valid_check` requires `completed_at IS NULL` for any non-terminal state; and
     the state-change trigger aborts with `send_managed.actor GUC not set: cannot record state transition`.
     So the update must be wrapped:
     ```sql
     BEGIN;
     SELECT set_config('send_managed.actor', 'admin', true);
     UPDATE send_managed.jobs SET state='queued', lease_expires_at=NULL,
            provider_message_id=NULL, completed_at=NULL WHERE id='<job>';
     COMMIT;
     ```
     `send_managed.event_actor` is `dispatcher | worker | reclaim_loop | admin | system` — there is **no**
     `operator` value; a manual edit is `admin`. This is a test-only manoeuvre: nothing in the
     product ever re-queues a `sent` job (§4.2 leave-in-place exists precisely so it cannot).
4. Confirm zero-width insertion is **off** by default; enable and verify the rate
   - **Observed: PASS, both halves.** Off by default is asserted at the widest scope available:
     swept **every one of the 497 messages** Mailpit had ever held across all twelve stages for
     `U+200D` (`e2 80 8d`, and its quoted-printable form `=E2=80=8D`) — **zero**. Then enabled it
     with `variation_zero_width_rate = 0.5` on a body of 60 words (59 word-boundary spaces × 4
     recipients = **236 independent draws**): **112 inserted, observed 0.4746** against a configured
     0.500 — within noise (σ ≈ 0.033 over 236 Bernoulli draws, so 0.8σ). Structural check too: every
     ZWJ sat immediately **before** a space, never inside a tag, attribute, or URL.
   - **The dial genuinely governs (not merely read).** Same body, same 236 draws, rate returned to
     the default `0.05` → **10 inserted, 0.0424**. 112 vs 10 is not a rounding difference; the
     parameter is load-bearing. Verifying a *rate* needs enough draws to distinguish it from its
     neighbours — 2 recipients cannot tell 0.05 from 0.15, which is why this uses a
     deliberately word-heavy body rather than the HTML fixture.
   - **Trap:** `variation_zero_width_rate` (0–1) and `variation_zero_width_rate_pct` (percent) are
     documented duplicates of one parameter under different units (P9.5-OD-3, RULED). Only
     `..._rate` is wired to the worker; setting `..._pct` alone changes nothing.
5. Confirm Thread-Index emits nothing
   - **Observed: PASS.** `variation_thread_index=true`, then inspected the delivered headers: no
     `Thread-Index`, `Thread-Topic`, `References`, or `In-Reply-To` on either message. The header
     set was exactly `From, To, Subject, Reply-To, Message-ID, Date, MIME-Version, Content-Type,
     Content-Transfer-Encoding` (plus Mailpit's own `Return-Path`/`Received`). Consistent with the
     code: toggle 12 deliberately registers **no stage at all** (`variation-headers.js` — "trivially
     inert in BOTH directions and can never affect `isNeutral`"), because V1 has no sequences and so
     no thread to index.
   - **Positive control — otherwise "NONE" only proves the sniffer is blind.** With
     `variation_user_agent_injection=true` and `variation_user_agent_rate=1.0`, the *same* sniffer on
     the *same* run reported a `User-Agent` header on both messages. So it can see an added header;
     Thread-Index really adds none.
6. Inspect a link/image URL parameter
   - **Pass:** unique per recipient, and contains **no** tracking or callback
   - **Observed: PASS.** 6 recipients, 4 URLs each (2 `href`, 2 `img src`): **6 distinct `_ref` and
     6 distinct `_v`**, and within each message a *single* value shared by every link — matching the
     per-**recipient** (not per-link) hash the spec calls for, since one job = one recipient.
   - **"No tracking or callback" asserted mechanically, not by eyeballing.** All 24 URLs were parsed
     and compared field-by-field against the four the author wrote: every one keeps the author's
     **scheme, host, and path** and retains its own query params (`?ref=abc` survives, joined with
     `&`); the **only** addition is `_ref`/`_v`; no scheme downgrade; and no host appears anywhere
     that the author did not write — no redirector, no tracking domain, no new endpoint. That is the
     permanent forward boundary in `variation-content.js`: an inert cache-buster on the author's own
     destination URL, never a tracker.
   - Fragment and existing-param handling are visible in the output: `?` vs `&` chosen correctly, and
     a URL already carrying the param is skipped rather than doubled.
7. Re-run the Stage 5 security checks with **every toggle on**
   - **Pass:** all still pass — variation must never touch the credential path
   - **Observed: PASS, all seven.** Every one of the **14** variation dials was driven to maximum
     activity — all 11 booleans `true`, `comment_intensity=high`, `header_style=random`,
     `precedence=marketing`, `user_agent_rate=1.0` — and a throwaway account with a fresh secret
     **actually SMTP-authenticated 3 sends** through that fully-varied path before any assertion
     ran. Checking a quiet system proves nothing here.
     1. `GET` + `LIST`: `has_credential: true`, secret absent, **no** credential-ish key at all.
     2. `PATCH` without `smtp_secret`: `credential_ciphertext`, `credential_nonce`, **and**
        `credential_key_id` all byte-unchanged — while `daily_limit` did change, so the request was
        genuinely processed.
     3. `PATCH` with a new secret: ciphertext **and** nonce both changed, `credential_key_id`
        recorded (`cred-dek-v1`) — and the rotated secret still authenticated real sends.
     4. Logs (`control`, `worker`, `worker2`, `postgres`; 630 KB) swept for **both** secrets in **five**
        forms — plaintext, base64, base64 `AUTH PLAIN` (`\0user\0pass`), unpadded base64, and a
        24-char base64 prefix (to catch an SMTP soft-wrap): **clean**. Also swept the 8 delivered
        messages, since variation rewrites the body: clean.
     5. Cross-tenant `GET /v1/report?job_id=…&user_id=<other>` → `404 not_found` naming only the
        caller's own selector; the correctly scoped request → `200`.
     6. Admin call with no key → `401`; wrong key → `401`; **and the Domain-A platform token →
        `401`** (Rule 6: the two auth domains never share a credential).
     7. All **14** admin routes swept (9 GET + pause/unpause/quarantine/unquarantine/PATCH): 31 KB
        of responses, zero occurrences of either secret in any form; `has_credential` is the only
        credential signal that exists.
   - **Trap — a "clean" verdict can be worth nothing, and was.** The first log sweep reported clean
     across all four services. Its positive control then reported clean **too**, on a file with the
     secret deliberately planted in it — because the shell glob had expanded to **zero files** and
     the checker happily iterated an empty list. Both detectors now abort on an empty file list
     (`ABORT: no files given — a leak check over zero files is vacuous`) and both carry a proven
     positive control: they find planted plaintext, find a planted base64 `AUTH PLAIN` blob, and
     still call an untampered file clean. **Never accept a green leak check that has not just
     demonstrated it can go red.**
   - **Trap:** `POST /v1/admin/accounts` decodes strictly, and `POST /v1/admin/pools` too — a wrong
     field name is a hard `400 validation_failed` with `issue: "malformed"` for the whole body, not a
     partial accept. The real field set is `pool_id` (**not** `user_id` — it is derived from the
     pool), `smtp_secret` (**not** `smtp_password`), and `from_address`/`from_name` (**not**
     `email_address`/`display_name`). Likewise `variation_precedence` takes
     `auto|marketing|personal` — `bulk` is the *header value* it emits, not a config value, and is
     correctly refused as `forbidden_value`.
   - **This step found a real defect, in its own cleanup — `G12-OD-1` / ADR-0012, since RULED AND
     FIXED.** Deleting the throwaway account returned **`500`**: `DELETE /v1/admin/accounts/{id}` was
     wedged for any account that had ever been assigned a job, because `job_events`' two `ON DELETE
     SET NULL` FKs were blocked by that table's unconditional append-only trigger. It was wedged, not
     lossy — the delete aborted atomically. It was **FLAGGED rather than hot-fixed** because the fix
     revises ADR-0009's explicit "closed / correct as-is" finding, and revising a ruled ADR is a
     ruling (Rule 2). The user ruled **option A** on 2026-07-29; `send_managed_0024` gives the trigger a
     per-nullable-FK-column carve-out and the delete now returns `204` with the audit rows surviving,
     `account_id` nulled and every content column byte-unchanged. **If you hit the `500` walking this
     guide, your schema is behind `0024`** — run migrations.
     The workaround verified at the time still stands as the everyday lever:
     `POST /v1/admin/accounts/{id}/disable` returns `200` and the account then received **0 of 6**
     subsequent jobs. Prefer it to `DELETE` for taking an account out of service; it is reversible.
   - **Close the stage by restoring defaults.** All 14 dials were written back to their
     `default_value` and confirmed at **0 drift** — a stage that leaves toggles on silently changes
     the baseline for every later run.

---

## Part 4 — What local testing cannot prove

Be clear-eyed about the boundary. Green on everything above does **not** tell you:

- **SPF / DKIM / DMARC actually pass** — needs real DNS and a real receiving provider
- **Inworker placement** — Mailpit accepts everything; it has no spam filter and no opinion
- **Provider quirks** — real auth mechanisms, TLS behaviour, rate-limit responses, and identity-verification rules differ per provider and will surprise you
- **Real bounce and complaint behaviour**
- **Deliverability and domain reputation**
- **Real network conditions** — latency, partitions, packet loss between containers on one host are not real network conditions

Those belong to the first real-provider smoke test and the pilot. Local testing proves the **logic** is right; only real sending proves the **email** is right.

---

## Part 5 — Pre-deploy sign-off

Do not deploy until every line is true.

- [x] Full harness green from a clean slate, zero skips *(Stage 1; `58 passed, 0 failed` from `scripts/dev.sh reset` — `bc9b74e`. The first run reported `35 passed, 23 failed`, which was **two** root causes, not 23. (1) `send_managed_0022` was added without extending `TestRunnerLifecycle`'s `wantChain`; fixed, plus the anti-vacuity assertions the other migrations carry — 0022 is `CREATE OR REPLACE` of two existing function **bodies**, so no column/index/config check can see it, and its `sending→sent` = not-an-error DECISION is now asserted, proven both directions. (2) The 21-failure cascade in the live suites was ONE empty variable: `$DC up -d worker` without `--no-deps` recreated the tmpfs postgres and wiped the DB out from under the live Control mid-suite. `migration_lint` now blocks a new migration that is absent from `wantChain` — nothing caught 0022 because the chain on disk was contiguous, so the gate passed while the suite failed. **Zero skips confirmed by reading them**, not by an absent-SKIP grep: the only `SKIP` string in the log is a test NAME (`TestVariation_CSSSkipBelowPxZeroSurvivesJSONRoundTrip`), and postgres started once at 07:13:37 and was never recreated across the 07:13→07:19 run)*
- [x] Both workers `active` after a reset, each registered exactly once (no eviction churn) *(Stage 1; from `dev.sh reset` + `up2`: two `active` rows with non-null `registered_at`, `worker registered id=` exactly **once** per worker id, and **zero** `stream closed`/evict lines. Verified from a FRESH reset deliberately — a check run right after the harness shows worker 1 registering **twice**, which is correct and not churn: `phase3_send.sh` T3 kills the worker on purpose to prove no outcome can be invented, then restarts it. Counting registrations on a post-harness stack would have failed this line for the wrong reason)*
- [x] All 6 CI gates green *(Stage 1; `migration_lint`, `config_drift`, `no_secret_leak`, `contract_drift`, `config_consumption`, `drain_grace` all PASS. `migration_lint` gained the wantChain-coupling check in this pass, with paired positive/negative controls — its first version passed with `wantChain` reverted, because a bare `grep '"0022"'` matched the version named in the gate's own failure message; now scoped to the `wantChain` literal. `architecture-overview.md` said "all five CI gates" and is corrected — CI has run six for a while)*
- [x] All Go and Node suites green; `gofmt`/`vet` clean *(Stage 1; `go test ./...` green on all four modules (control: adminapi/workerlink/workerproto/credential/dispatch/platformapi, harness, send-managed-migrate, plus the worker's Node suite at **78/78, 0 skipped**), `gofmt -l` empty, `go vet` clean. **No `t.Skip` fired anywhere** — the 8 `?` lines are packages with no test files of their own (`config`, `configordering`, `db`, `httpapi`, `kms`, `singleton`, `tlsutil`, `cmd/control`), and each was checked to be covered behaviorally at the process level by the harness instead of taken on faith: singleton by SUITE 4 (a real second Control exits, `readyz singleton_lock=true`), kms by SUITE 5 (the worker container cannot read the key, and the control that the worker FS is mounted at all), tlsutil/httpapi/workerlink by SUITE 3 (register/heartbeat over real mTLS + the wrong-token 401), configordering by the load-path rejection tests. That is Rule 3's preference, not a coverage gap — but note the state of it: for these seven packages a unit-level regression is caught only if it changes process behavior)*
- [x] Clean-slate reproduction completed from the docs alone, without deviation *(Stage 2; PASS on the **fourth** reset — the earlier three each hit a docs silence and are the deliverable of this stage, not a failure of it. Pass 4 ran `install-and-deploy.md` Steps 9–10 verbatim with zero improvisation: reset 46 s + Steps 9–10 in 10 s, ending at `report` → `{"state":"sent","provider_message_id":"6hLItpFlPk1CFuscrMQHFr","attempt_count":0}` and Mailpit `total: 1`. **Four docs bugs fixed in `install-and-deploy.md` in the same change**, all of them silences rather than wrong statements — the product answered correctly every time: (1) nothing named the bundled sink, so `smtp.example.com` was a hard stop; (2) Step 10 gave no request bodies and no contract link, and all three guesses were wrong (`quote` takes `recipient_count` only, `run` is singular `job_item_id`, `report` uses query params); (3) `UID=` is **readonly in bash**, so the assignment silently fails and `run` posts an empty `user_id` — shell-dependent, which is how it survived review; (4) `public.job_items` is empty after a reset and nothing in this repo seeds it, so the doc named a value the reader could not obtain. Bug 4's symptom actively misleads: `psql -tA` on a `RETURNING` insert prints the command tag `INSERT 0 1` as a second line, and the resulting malformed body makes `run` answer `400 validation_failed` naming `job_item_id: required` — which reads as a wrong field name and is not one. Cross-domain credential separation re-verified here with paired positive controls, since a cross-domain 401 proves nothing if the credential is merely broken)*
- [x] Empty state tells an operator what is missing *(Stage 3, both steps; from a fresh reset with `pools=0`/`accounts=0`/`default_shared_pool_id={"v": null}` asserted as the precondition. A campaign run into that void returns `202 queued` and the job then sits `queued` with `attempt_count=0` and **exactly one** `job_events` row at both 12 s and 60 s — no churn, which is the half that could plausibly have failed, since a 1-second polling dispatcher facing an unservable job is exactly the shape that spins `queued→assigned→queued` forever. The operator is told three ways: `waiting_for_capacity: 1`, a Control log line naming the condition + the exact config key + the fix (rate-limited: 3 lines in ~4 min, not one per tick), and a runbook triage path walked from the observed symptom to a section that names this case in words. `active_conditions[]` stays `[]` BY DESIGN — a never-configured system is a setup state, not an alert, and alerting would page during install)*
- [x] Every admin lifecycle transition works and is audited *(Stage 4; refusals also made diagnosable — `12dade8`, `6004cd7`)*
- [x] Credential never appears in any read, list, or log — plaintext **or base64** *(Stage 5; verified AFTER a real authenticated send so AUTH had actually run, across all 21 `/v1/admin/*` routes, and for the ciphertext + `credential_key_id` too. The shipped scans grepped plaintext only — fixed in `9daa622`)*
- [x] Tenant isolation holds *(Stage 5; cross-tenant `report` is byte-identical to a nonexistent id — `b290953`)*
- [x] Both config-ordering invariants rejected at the API boundary *(Stage 6; **all four directions** of both pairs → HTTP 400 `config_ordering_violation` naming the conflicting key and its current value, nothing landing in the DB, no clamping. Ordered two-step widening AND narrowing both succeed; the wrong order is refused. `default_value` never mutated)*
- [x] An invalid config type does not wedge the Control *(Stage 6; 13 type violations → 400 with the DB unchanged, and no wedge proven by a real job reaching `sent` — not merely a 200 from `/health`. Going past the API with a direct SQL write found a real defect: a type fault was misreported as `db outage DETECTED` against a healthy DB — fixed in `a617fda`)*
- [x] quote → run → report cycle correct end to end *(Stage 7; `describe` pricing matches `send_managed.config` and its capability flags are honest — `sequences`/`open_tracking`/`click_tracking` all `false`; `quote` = N × unit for N ∈ {0,1,7,250,10000} and follows a live price retune with no redeploy; `run` returned 202 `state:queued` (never `sent`) so it did not block; `report` totals matched the DB at every sample and `fraction_complete` tracked 0.25→0.5→0.75→1; all messages in Mailpit with all 6 headers correct from the RAW source and Message-IDs distinct; jobs `sent` with `provider_message_id`, the `job_events` chain complete with the outcome attributed to `worker` (never the dispatcher), `send_logs` written with the right TTL, and zero credential/ciphertext/nonce/key-id occurrences with the matcher proven non-vacuous. Reading `details[]` on quote's rejections rather than just the status code found two real defects — a supplied-but-negative count reported as `required`, and a wrong type naming no field at all — fixed in `fe27588`)*
- [x] All three routing strategies distribute as specified *(Stage 8; distinguished by STRUCTURE, not count: `round_robin` 12 jobs → exactly 6/6 strictly alternating `ABABABABABAB`; `random` 20 jobs → 13/7 with runs of 5 (structurally impossible under round-robin); `one_by_one` 12/0 fully sticky at equal `daily_limit`s. Both accounts' scores healed to 100 after their successes. Walking this step found a docs defect — the runbook claimed the default order "exhausts" the leading account before moving on; it names the wrong default (`round_robin` is the default) and `one_by_one` actually hands over after ONE send when limits are unequal, because the score's `w_prox` term penalizes `daily_sent_count ÷ daily_limit`. Code is correct and locked; docs fixed and the asymmetry pinned by `onebyone_handover_test.go`)*
- [x] Pacing matches `lanes ÷ delay`; doubling lanes roughly doubles throughput *(Stage 8; at delay 4–5s: 1 lane → 11.17 sends/min vs the ~13.3 ideal, the ~0.87s/send gap being real SMTP round-trip overhead; 2 lanes → 24.69/min (2.21×); 4 lanes → 50.47/min (2.04×). Near-linear across a 4× range, which is the per-lane-pacing proof — a global rate would be flat. All three config keys driven through `PUT /v1/admin/config/{key}` so the write serializer and validation ran, then restored to seeded defaults)*
- [x] Dedicated-pool fallback fires only on genuine exhaustion *(Stage 8, both halves. "Only on exhaustion": dedicated account at `daily_limit=4` with 8 jobs → jobs 1–4 dedicated, 5–8 shared, in that order. "Not before": limit raised + `daily_reset_day` aged → all 5 of 5 jobs stayed dedicated, ZERO on shared. Plus the P4-3 kind policy — `fallback_pool_id` pointed at ANOTHER tenant's dedicated pool was ignored and routing fell through to `default_shared_pool_id` (3/3 on shared, zero on the other tenant's accounts), so one tenant's overflow never lands on another tenant's identities)*
- [x] Second concurrent campaign per user rejected *(Stage 8; campaign B attempted 3× while A had 3 non-terminal jobs → `409 conflict` each time and `campB_job_rows=0` — rejected outright, not queued. Envelope carried only `{code, message, request_id}`; the internal reason `user_campaign_active` did not leak (Rule 5). Both controls green: a repeat `run` within the SAME campaign is still accepted while that campaign is in flight (all 6 of A's items went through with 3 already non-terminal), and B was accepted 3×202 once A drained — so this is not a blanket reject)*
- [x] Every error class produces the correct job **and** account outcome *(Stage 9 steps 1–4; verified by the real-Postgres suites named in the step→test table, NOT on the live stack — the compose sink is Mailpit and cannot emit a 535/550/4xx/throttle, and the "controllable Go sink (`tests/harness`)" its comment promised was never built. That comment is corrected and the gap is registered in `future-work.md`. This worker is ticked because the behaviour is genuinely proven; it is not ticked from a live-stack demonstration)*
- [x] Hard bounce does not penalise the account *(Stage 9 step 2; `TestScore_BounceNeverPenalizes` — a 550 leaves score, `consecutive_failures`, and `last_failure_at` byte-unchanged and writes zero `account_events`, with a paired `transport_4xx` positive control proving the scorer is not simply inert)*
- [x] Worker death produces exactly one delivered copy *(Stage 9 step 6; `docker pause` then SIGKILL **while frozen** — 0 Mailpit copies pre-kill so no outcome could ever arrive, which the first attempt got wrong by unpausing first and proved nothing. The lease expired, `reclaim_loop` requeued with `error_class='timeout'`, the surviving worker re-sent: exactly one Mailpit copy and one `send_logs` row, `sending→queued` attributed to `reclaim_loop` not `worker`. Also confirmed `send_managed_0022` live — the `sending→sent` event carried no error class while the row correctly retained `timeout`)*
- [x] Control restart loses and duplicates nothing *(Stage 9 step 7; restarted with 1 job genuinely in-flight on a frozen worker. Drained gracefully, and the startup rescan **extended** the orphan's lease rather than reclaiming it — orphan still `sending`/token 1/attempts 0/no error, so nothing was invented on the way down. The worker then reconnected inside its window and **replayed its buffered outcome** onto the ORIGINAL `attempt_token=1`: 4-row event chain with `sending→sent` by `worker`, no requeue, no second dispatch, one `send_logs` row, 14/14 Mailpit subjects, zero duplicates)*
- [x] **Database outage invents no outcomes** *(Stage 9 step 8, the guide's most important assertion. Postgres stopped 1m48s with a job genuinely in-flight. Control stayed `running (healthy)`, **0 restarts**, backoff to a 30s re-probe cap, every line `loops remain PAUSED, no state invented`, singleton lock correctly marked not-held on `57P01`. `health` and `run` failed **closed** with only `{code, message, request_id}` while the internal reasons went to the log under the matching `request_id` (Rule 5, both directions, leak grep clean). Decisive: `jobs_total` 266→266, `send_logs` 265→**266** (exactly one new send, for the one job that needed it), and the 4 new `job_events` are seq **1089–1092** — the next four after the snapshot, so **zero rows were written during the outage**. Mailpit 16/16, zero duplicates)*
- [x] Pool exhaustion waits rather than fails *(Stage 9 step 9, **both** verdicts, and walking it found the step itself was wrong — it said to disable every account and expect a WAIT, which correctly produces `failed` + `pool_exhausted: all eligible accounts terminal_blocked (§5.1)`. Per the locked §5.1 three-way verdict, quarantined/disabled/manually-paused is `terminal_blocked` → FAIL (needs a human; queueing forever behind a block nothing will lift would be dishonest), while daily-limit / at-limit / in-window-throttle is `self_recovering` → WAIT. Verified the WAIT path properly: 6 jobs `queued`, `attempt_count=0`, zero writes, `waiting_for_capacity: 6`. Re-opening ONE account via the automatic daily reset — `unpause` correctly refuses a non-manual pause with 422 — completed the **byte-identical six ids** on that account with `attempt_count` still 0 for four, so waiting burns no retry budget. Guide corrected; code untouched)*
- [x] Panic switch halts and resumes losslessly *(Stage 10 steps 1–2, both directions and both the mid-flight flip and the stronger "launch a campaign INTO the halt" case, since a local campaign drains fast enough that the flip alone can pass vacuously. Halt proven by the event log — last `job_events` row 3s before the flip, zero after — not by a state snapshot, which can read a pre-flip commit and look like a violation. 10 jobs launched into the halt stayed `queued`/unassigned/`attempt_count=0` with nothing failed; `report` read clean `queued` with no internal reason; health showed `dispatch_paused: true` with `waiting_for_capacity: 0`, distinguishing an intentional halt from a capacity fault. Re-enabled: **byte-identical** 10 ids all `sent`, `attempt_count` still 0, exactly 4 events each — **40 rows, zero requeues, zero reclaims**. Also confirmed a halt does not block outcome persistence for work already in flight, which is correct: refusing it would strand a real send)*
- [x] Shadow mode logs without acting; enforce acts on the same condition *(Stage 10 steps 3–4, **live on the stack, not deferred to the unit suites** — a dead `smtp_port` (2599) makes the worker report `connect_refused` → class `timeout`, which despite classify.go's "no penalty" comment (that means no *flag* action) is genuinely penalizing and drives the score. Needs a **never-succeeded** account: the formula `100 − 60·min(1,cf/5) − 15·prox − 25·recency` bottoms out near 40 for an account with a recent success, above the threshold of 30, so a warm account never trips. Shadow: score 100→27→15, `cf` to 10, **seven** `shadow_would_quarantine` rows all with `from_flags = to_flags`, `quarantined` false throughout, jobs requeuing normally. Third leg proven separately rather than assumed — a low score is only an ORDER BY term in `eligibility.go`, never a filter — and after restoring the port all 6 jobs `sent`, 1 Mailpit copy each, with **4 of 6 attributed to the score-15 account itself** (checked via `assigned_account_id`, because `default_shared_pool_id` puts a shared pool in every tenant's chain, so a job count alone does not say which account sent it). Enforce, second fresh account, identical condition: `quarantined=true` at `cf=4`/score 27 with a real `action='quarantine'`, `reason='score_below_threshold'`, `from_flags ≠ to_flags`, **exactly one** row — and the flag changed ROUTING, not just a boolean: all 6 of that tenant's jobs completed on the fallback accounts, **zero** on the quarantined one, which re-routed rather than failed. Enforce stopped at `cf=4` where shadow ran to `cf=10` — which is the point of the ladder)*
- [x] Health endpoint reflects true state; all three alerts fire *(Stage 10 steps 6–7. Health: every field cross-checked against its own query (queue, accounts, workers, all 5 pools' eligible counts, `errors_last_window` timeout:18/unknown:15, shadow count 9 with real score/threshold pairs), then **perturbed** — pause + disable moved `healthy` 6→4, `paused` 0→1, `disabled` 0→1 and flipped two pools to `headroom: "exhausted"`, so the numbers are live, not cached. Two apparent discrepancies checked before filing and both correct: `sends_last_window` 136 vs 152 `send_logs` rows reconciles exactly at `count(DISTINCT job_id)`=136 (per-job vs per-attempt), and `quarantined: 0` beside an `account_quarantined` condition is by design — that condition is window-scoped over `account_events` since a quarantine has no "still true" row state. Alerts: all three fired with deliveries to a receiver on the compose network, dedup proven (4 polls → 1 delivery), `pool_exhausted` needing a genuinely **waiting** job (a manual pause is terminal_blocked → `failed`, not queued; the daily limit is the self_recovering block that works) and firing once per pool in the chain, `worker_death` at 6m51s and **auto-clearing** on reconnect. Payloads two-vocabulary clean: condition code, subject id, timestamps, nothing else. No-webhook half done on a FRESH edge, not a deduped repeat: HTTP 200, all conditions surfaced, delivery count unchanged, zero errors)*
- [x] **KMS key restore drill passed** *(Stage 11 step 1, by the documented `disaster-recovery.md` §1 route. The working key was `shred -u`'d so the gpg backup was the ONLY copy; restore came back 32 bytes with a SHA-256 identical to the original. Proven by decryption, not by a clean start — and the **wrong-key negative control is what makes that distinction real**: a random 32-byte key starts perfectly cleanly (`kms dev key loaded`, `readyz` `ready`, container `healthy`), so length is all a clean boot proves. Under the wrong key two jobs failed `credential decrypt failed (authentication)` and were **requeued** with `assigned_account_id` NULL — no invented outcome — with **Mailpit `count: 0`** so no plaintext escaped, the client `report` showing only `queued`/`pending: 2` (0 matches for `decrypt|authentication|kms|key|cipher|nonce|SQLSTATE|send_managed_app`), and no key material in the log either. After restore the **same two job ids** went `sent` with one Mailpit copy and one `send_logs` row each, then **all six** accounts' pre-drill credentials decrypted — 12 shared-pool jobs plus one per dedicated tenant via `CAMPAIGN_USER_ID` — with **zero** decrypt failures)*
- [x] Credential rotation completed without interruption *(Stage 11 step 2, by the documented `disaster-recovery.md` §3 route, and "without interruption" proven non-vacuously — **4 real jobs were parked in the freeze and spanned the entire rotation**, since a drained queue would prove nothing. New key gpg-backed-up BEFORE use; `dispatch_paused: true` confirmed before the cutover. In the unreadable window the error is the **explicit** `cannot resolve credential_key_id "cred-dek-v1" (this Control resolves "cred-dek-v2")` — `credential.go:57` checks the label before touching AES, so a rotation mistake is never a silent wrong-key attempt, provably distinct from the generic auth failure. Jobs requeued, Mailpit `count: 0`, nothing invented. Re-entry × 6 → `200`; §3's own verification queries returned `cred-dek-v2|6` and **empty** for rows off the new label, with **6 distinct nonces and 6 distinct ciphertexts** from 6 identical plaintexts. The same 4 parked job ids then went `sent` with **one `send_logs` row each despite `attempt_count = 3`** — no duplicate from the failed attempts — and all six accounts decrypted under v2 with zero failures, 6 audited writes under 6 distinct `request_id`s, and a clean leak grep. Both halves fail closed by **different** errors: right label + wrong key gives generic GCM auth failure (47 of them, Mailpit 0). §3's abandon path was exercised too and is symmetric as documented — rolled back to v1, which is the correct dev-stack end state since `scripts/dev.sh:63` rewrites `.env` to `cred-dek-v1`)*
- [x] Adding a worker requires no Control reconfiguration *(Stage 11 steps 3–4. **Add:** a third identity provisioned, its `token_hash` seeded, container started reusing the dev cert — registered, health `active: 3`, and it received real work (15 jobs split 4/9/2, its sends carrying a `provider_message_id` and `sending→sent` written by actor `worker`). The claim is proven by what did **not** change: the Control's `StartedAt` and `restarts=0` were **identical before and after**, and its env held only worker-1 material, so the token gate is DB-driven. Worker-3's security envelope re-verified independently: no DB/KMS env, only `worker.crt`/`worker.key`/`ca.crt`, no `/secrets/kms/`, no `control.key`/`ca.key`, 0 credential matches in its log. **Remove:** stopped cleanly (exit 0) with a job genuinely `sending` — held there by a blackhole listener, because polling cannot catch a fast send. The job stayed `sending` with `attempt_token` unchanged (nothing invented for an absent worker), was requeued 5 s after its lease by **`reclaim_loop`** with `timeout`/"lease expired" — never by `worker` — then sent on a surviving worker with **one** Mailpit copy. All 13 worker-3 jobs ended `sent`; the cluster-wide non-terminal sweep was empty. A fresh 12-job campaign then went 12/12 `sent` split 10/2 with **0** to the removed worker. Worker 3 aged `active`→`stale` with no operator action)*
- [x] Graceful shutdown strands no job; second Control exits cleanly *(Stage 11 steps 5 and 6. **Shutdown:** the §4.1 sequence observed in order (stop-claim → drain → leave-in-place); 7 jobs still `queued` at the signal **stayed `queued`** with the Control down, workers waited (`ENOTFOUND control; retrying`) rather than inventing, and all 16 reached `sent` on restart with 16 Mailpit copies / 16 `send_logs` rows. The in-flight branch was **forced** with a blackhole, because an empty drain finishes in 1 ms and proves nothing: the in-flight job was left exactly as stored — `sending`, `attempt_token` and lease unchanged, no terminal event — even when SIGKILLed mid-drain, then recovered by the next boot's rescan (`extended 1 orphaned in-flight lease(s)`) with the **worker replaying its own buffered outcome** (actor `worker`, not `reclaim_loop`) and sent with exactly one copy. Cluster-wide non-terminal sweep empty. Forcing that branch also **found and fixed a real defect**: `shutdown_drain_timeout_sec` is 30 s but `docker-compose.yml` declared no `stop_grace_period`, so Docker's 10 s default SIGKILLed every drain (`exitcode=137`) — an invariant already documented in `config-reference.md`, the systemd unit, and the runbook, contradicted only by the dev file. `stop_grace_period: 90s` added and proven by a paired control: 137/truncated → **exit 0** with `drain complete in 18.076s — every in-flight job reported and resolved normally`. **Second Control:** waited the bounded `1m0s` and exited **code 3** with the incumbent still `ready`. The separate mid-life lock-loss defect this item did NOT cover — `G11-OD-1` / ADR-0011 — was **ruled (option B) and fixed on 2026-07-29**: the three loops pause while the lock is unheld and the keeper re-acquires on the seeded `brain_singleton_reacquire_interval_sec`, covered by 7 real-Postgres tests in `internal/dispatch/leadership_test.go`)*
- [x] The latest migration rolls down and back up with no data loss *(Stage 11 step 7. This line did not exist until step 7 was walked — the step was in Part 3 but had no sign-off counterpart, so a reversibility failure could not have blocked a deploy. `send_managed_0022` is trigger-bodies-only (two `CREATE OR REPLACE FUNCTION`s + one helper), so no fixture data was at risk; a migration that dropped a column would need the fixtures rebuilt instead. Measured, not eyeballed: a row-count fingerprint across all nine tables was **byte-identical** before and after the down (`accounts` 6, `jobs` 487, `job_events` 2535, `send_logs` 500, `pools` 5, `workers` 3, `config` 53, `account_events` 62), with the only deltas being the deleted `schema_migrations` row and the dropped helper. Behavior reverted too, not just objects — both trigger bodies confirmed back to their `send_managed_0008` form, which deliberately reinstates the defect 0022 fixed, since a down that keeps half the up is not a reverse. The Control kept serving on the downgraded schema and the down file's own compatibility claim was **verified rather than trusted**: a post-0022 Control on a pre-0022 schema still reports a correct `errors_last_window` because the reader filters on transition shape in Go — every counted row was genuinely error-shaped. Up restored version `0022`, the helper, both gates, and `send_managed_owner` ownership on all four functions (Rule 6 — a down/up must not reassign ownership to the migrating superuser or `FORCE ROW LEVEL SECURITY` stops enforcing); a fresh campaign's `sending→sent` came back untagged. `up` idempotent, `status` `23 applied, 0 pending`, all 6 gates green)*
- [x] All-toggles-off is byte-identical; security holds with all toggles on *(Stage 12, all 7 steps. **Off:** `variation_enabled=false` and all-sub-toggles-at-default produce the same 454-byte message (sha `9db8dbd5…`) — the shipped default already **is** off. **On:** identical content to two recipients differs only in ±1px CSS and inert `_ref`/`_v` params; the same `job_id` re-sent is byte-identical (`ff52b48e…` twice) while a different id differs, so the seed is genuinely id-derived. Zero-width off by default across **all 497** messages Mailpit ever held, and its rate dial governs (112/236 draws at 0.5 vs 10/236 at 0.05, same body). Thread-Index emits nothing even ON — with a `User-Agent`-at-rate-1.0 positive control proving the sniffer is not blind. URL params 6/6 unique per recipient, one value per message, and mechanically asserted inert: 24 URLs keep the author's scheme+host+path and own query params, only `_ref`/`_v` added, no host the author did not write. **Security with all 14 dials at maximum**, after 3 genuinely SMTP-authenticated sends through the varied path: all seven Stage 5 checks re-pass — secret absent from reads and from 630 KB of service logs in **five** encodings (plaintext, base64, base64 `AUTH PLAIN`, unpadded, 24-char prefix) and from the delivered bodies; ciphertext+nonce byte-stable without `smtp_secret` and both changed with one; rotated secret still authenticates; cross-tenant `404` vs correct-tenant `200`; admin `401` for missing, wrong, **and Domain-A** credentials; 14 admin routes swept with `has_credential` the only credential signal. **The first log sweep's "clean" verdict was worthless** — its own positive control also came back clean because the glob matched zero files; both detectors now abort on an empty file list and are proven to go red on planted plaintext and planted base64. Step 7's cleanup found `G12-OD-1` / ADR-0012 (a used account could not be hard-deleted — wedged, not lossy; flagged rather than hot-fixed because the fix revises a ruled ADR, with the `/disable` workaround verified meanwhile). **RULED option A and FIXED 2026-07-29** by `send_managed_0024`: a per-nullable-FK-column carve-out on `job_events`' append-only trigger, so the delete returns `204` with the audit rows surviving (FK nulled, content byte-unchanged) — 5 behavioral tests on real Postgres, 3 anti-vacuity controls, and a paired migration-lifecycle probe that drives the trigger in both directions. All 14 dials restored to `default_value`, 0 drift)*
- [x] Auto-quarantine returned to **shadow** before you finish *(verified at the Stage 12 close, together with the other three kill-switches, since Stage 10 step 4 drove it to `enforce` and an `enforce` left behind would let a later stage quarantine a real account unattended: `autoquarantine_mode = shadow`, `dispatch_enabled = true`, `autopause_enabled = true`, `variation_enabled = true`. Whole-table drift is the stronger check and is also clean — the **only** row differing from `default_value` is the intentional `default_shared_pool_id` fixture, with 0 non-terminal jobs cluster-wide)*

When this is all green, deploy — and pick the real-send guide up at its Layer 2.
