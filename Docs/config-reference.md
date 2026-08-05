# Config reference (living mirror of Phase 9 §4)

Authoritative source: **Phase 9 §4** (`history/phase-9-polish-and-documentation.md`) — originally a
53-key consolidated reference (amended from 50, `Docs/history/open-decisions.md` P8-OD-1: Phase 8
grew from 14 to 17 keys post-Chunk-5). This file is the checked-in mirror, verified by the
CI `config_drift` gate against the live `send_managed.config` seeds.

**Current total: 57 keys** — 14 (`0010`) + 3 (`0017`) + 2 (`0018`) + 6 (`0019`) + 17 (`0020`) +
12 (`0021`+`0023`) + 1 (`0025`, fix-plan item 12) + 2 (`0026`, fix-plan item 13). This number is a
convenience for the reader, not the gate: `config_drift` diffs the live key set against
`config-manifest.txt` and against this file's tables, so a stale count here is a doc bug while a stale
key is a build failure.

## Phase 1 state (14 keys, `send_managed_0010`)

`send_managed.config` is created in Phase 1 (`send_managed_0010`), seeding the **14 foundation keys** below
(Phase 9 §4.1). Later phases append their keys (Phase 5 `send_managed_0017`, Phase 6 `send_managed_0018`,
Phase 7 `send_managed_0019`, Phase 8 `send_managed_0020`, Phase 9.5 `send_managed_0021`, then the fix-plan
alignment keys `send_managed_0025`/`send_managed_0026`) growing to the current 57-key
total. The drift gate diffs the **phase-aware manifest** (`config-manifest.txt`) against the
live `send_managed.config` key set AND this table — all three must match, which is what makes "no
hardcoded tuning constant" structurally checkable. `default_value` is never mutated.

| key | default_value | value_type | seeding migration | description |
|-----|---------------|-----------|-------------------|-------------|
| `daily_limit_default` | `{"v": 500}` | integer | send_managed_0010 | Default max sends per account per day. |
| `daily_reset_timezone` | `{"v": "UTC"}` | string | send_managed_0010 | Timezone (IANA tz name) defining the calendar "day" for limits. |
| `daily_reset_hour` | `{"v": 0}` | integer | send_managed_0010 | Hour of day (0–23, in tz) when daily counters reset. Consumed by every daily-reset-boundary computation (eligibility/consume, pool exhaustion, account scoring, the admin health endpoint) — wired in the Phase 9.5 CLOSEOUT config-consumption sweep (2026-07-27); seeded/validated since send_managed_0010 but not read by any of them before that. |
| `quarantine_threshold` | `{"v": 30}` | integer | send_managed_0010 | account_status_score below which an account is auto-quarantined (Phase 5). |
| `retry_count_4xx` | `{"v": 3}` | integer | send_managed_0010 | **RESERVED / NOT-YET-CONSUMED** (flagged in the Phase 9.5 CLOSEOUT config-consumption sweep, 2026-07-27). Documented in Phase 5 §6.2 (ruling 4) as a SOFT/approximate early-life same-account retry hint, but V1 has no per-job avoid-set or per-(job,account) attempt tracker to hook it into (Phase 5's own §6.3 text) — `backoffSecondsForClass` requeues transient classes purely off the exponential `retry_backoff_schedule` ladder, with zero reference to this key. Writing it today has NO effect on requeue behavior. Wiring it requires designing the avoid-set/attempt-tracking mechanism Phase 5 explicitly deferred — an Open Decision for whichever phase takes it on, not a surgical fix. |
| `dispatch_tick_interval_ms` | `{"v": 1000}` | integer | send_managed_0010 | Dispatcher poll interval / config re-read cadence (Phase 4). |
| `send_delay_min_sec` | `{"v": 3}` | integer | send_managed_0010 | Lower bound of the random inter-send delay per lane (Phase 6). |
| `send_delay_max_sec` | `{"v": 7}` | integer | send_managed_0010 | Upper bound of the random inter-send delay per lane (Phase 6). |
| `concurrency_per_user_default` | `{"v": 3}` | integer | send_managed_0010 | Parallel-lane ceiling per user (Phase 4/6). |
| `heartbeat_interval_sec` | `{"v": 120}` | integer | send_managed_0010 | Expected worker heartbeat cadence (Phase 6). |
| `heartbeat_missed_threshold` | `{"v": 3}` | integer | send_managed_0010 | Missed heartbeats before a worker is presumed dead (Phase 6). |
| `job_max_processing_time_sec` | `{"v": 120}` | integer | send_managed_0010 | Per-job max processing time (lease). **Invariant §4.6(a).** |
| `debug_log_retention_days` | `{"v": 7}` | integer | send_managed_0010 | `send_logs.ttl_expires_at` age before the sweep purges raw content (Phase 7). |
| `default_shared_pool_id` | `{"v": null}` | uuid | send_managed_0010 | Base-campaign shared pool; must be set before base sends route (Phase 4). |

## Phase 5 state (+3 keys, `send_managed_0017`)

Phase 5 seeds three new keys via `send_managed_0017` (and reuses the already-seeded
`quarantine_threshold` and `retry_count_4xx` from `send_managed_0010` — no duplicate seeded). The two
`json` keys are validated for inner shape by the Phase 2 config endpoint and re-validated on read
(reject-and-fall-back-to-default with a logged warning if malformed, never crash — §7).

| key | default_value | value_type | seeding migration | description |
|-----|---------------|-----------|-------------------|-------------|
| `status_score_weights` | `{"v":{"w_fail":60,"w_prox":15,"w_recency":25,"fail_saturation":5,"recency_horizon_hours":72}}` | json | send_managed_0017 | Weights + shape parameters for the account status score (Phase 5 §3.2). |
| `retry_backoff_schedule` | `{"v":{"ladder_seconds":[30,120,600],"max_seconds":600}}` | json | send_managed_0017 | Exponential backoff ladder (seconds) for transient/timeout/unknown requeues, capped at max_seconds (Phase 5 §6.4). |
| `throttle_pause_seconds` | `{"v":900}` | integer | send_managed_0017 | Duration (seconds) of a throttle pause before lazy auto-clear, and the throttle-class requeue delay (Phase 5 §4.3/§6.4). |

**`retry_count_4xx` (seeded `send_managed_0010`) is documented as a SOFT/approximate early-life
same-account retry hint (§6.2, ruling 4), NOT a hard per-account ceiling** — but see the
**RESERVED / NOT-YET-CONSUMED** note on its own row above: no code path reads this key today.
Ceasing to use a failing account is governed by `status_score_weights` + `quarantine_threshold`,
not this dial.

## Phase 6 state (+2 keys, `send_managed_0018`)

Phase 6 seeds two new keys via `send_managed_0018` (and reuses the already-seeded `send_delay_min_sec` /
`send_delay_max_sec` / `concurrency_per_user_default` / `heartbeat_interval_sec` /
`heartbeat_missed_threshold` / `job_max_processing_time_sec` from `send_managed_0010` — no duplicates).

| key | default_value | value_type | seeding migration | description |
|-----|---------------|-----------|-------------------|-------------|
| `reclaim_tick_interval_ms` | `{"v":15000}` | integer | send_managed_0018 | Milliseconds between reclaim-loop poll cycles (dead-worker heartbeat scan + per-job lease-expiry backstop). Phase 6 §2. |
| `smtp_operation_timeout_sec` | `{"v":30}` | integer | send_managed_0018 | Worker per-operation SMTP timeout (connect/greeting/socket-read on MAIL/RCPT/DATA). Consumed by the worker (delivered on `push_job.smtp.operation_timeout_sec`). MUST stay `< job_max_processing_time_sec` so an account hang surfaces as a worker-reported timeout (scored/quarantined) rather than an unscored Control lease reclaim. Phase 6 §0/§2 (P6-11). |

The pacing dials (`send_delay_min_sec`/`send_delay_max_sec`, `concurrency_per_user_default`) are the
per-lane inter-send delay + lane ceiling behind Interpretation-A throughput (≈ lanes ÷ avg-delay,
§1.3). The heartbeat dials (`heartbeat_interval_sec` × `heartbeat_missed_threshold`) set the
dead-worker silence window (default 360s, §2.2); `job_max_processing_time_sec` is the per-job lease /
Detector-B backstop deadline (§2.3).

## Phase 7 state (+6 keys, `send_managed_0019`)

Phase 7 seeds six new keys via `send_managed_0019` (and reuses the already-seeded `debug_log_retention_days`
from `send_managed_0010` as the raw-content lifetime — no duplicate). Phase 7 is **fully schema-neutral**:
per-day debug-log summaries derive on demand from the permanent `job_events` + final `jobs` states
(P7-1), so no `send_managed.send_log_summaries` table is created.

| key | default_value | value_type | seeding migration | description |
|-----|---------------|-----------|-------------------|-------------|
| `startup_reconnect_window_sec` | `{"v":120}` | integer | send_managed_0019 | Startup-rescan reconnect-reconciliation window: seconds the rescan extends orphaned in-flight leases (event-free) / holds workers before the continuous reclaim loop reclaims. Consumed by the Control. Phase 7 §1. |
| `shutdown_drain_timeout_sec` | `{"v":30}` | integer | send_managed_0019 | Max graceful-shutdown drain wait for in-flight sends to report before leaving them for the next boot's rescan. **Keep below deploy SIGTERM→SIGKILL grace** — under Compose that is `stop_grace_period` (Docker's default is only **10 s**, which truncates this 30 s drain; `docker-compose.yml` sets `90s` on `control`), and under systemd it is `TimeoutStopSec`. Too short is not unsafe (leave-in-place requeues and marks nothing), just wasteful: a clean drain becomes reclaim work and the process exits 137. Consumed by the Control. Phase 7 §4. |
| `retention_sweep_interval_ms` | `{"v":3600000}` | integer | send_managed_0019 | Housekeeping-loop tick for the `send_logs` retention sweep. Consumed by the Control. Phase 7 §3. |
| `retention_delete_batch_size` | `{"v":1000}` | integer | send_managed_0019 | Rows per `send_logs` retention DELETE batch (bounds lock/vacuum pressure). Owner of Phase 1 #29. Consumed by the Control. Phase 7 §3. |
| `rollup_granularity` | `{"v":"day"}` | string | send_managed_0019 | Bucket granularity for on-demand debug-log summaries. Introduced because Phase 1 has no `metrics_aggregation_period` key (flagged, not silently reused — P7-9). Phase 7 §3. |
| `box_outcome_buffer_ttl_sec` | `{"v":900}` | integer | send_managed_0019 | Bound on the worker outcome-buffer: retain/replay unacked outcomes up to this age, then drop and fall back to reclaim. Consumed by the worker (coordinated Phase 3). Phase 7 §1.3. |

The rescan/window keys (`startup_reconnect_window_sec`, `box_outcome_buffer_ttl_sec`) are jointly
governed by the P7-11 ordering invariant below. The retention keys drive the single-Control
`send_logs` sweep (§3); job outcomes in `job_events` + final `jobs` states are **never** swept.

## Phase 8 state (+17 keys, `send_managed_0020`)

Phase 8 seeds seventeen new keys — appended to the SAME `send_managed_0020` version file Chunk 1 created
for `send_managed.jobs.attempt_token` (flagged migration-filename deviation from the locked spec's own
`send_managed_0020_content_variation_config` name; see `Docs/history/open-decisions.md` P8-OD-1). **Amended from
fourteen to seventeen, 2026-07-26 (P8-OD-1, RULED):** three toggle parameters
(`variation_css_magnitude_px`, `variation_zero_width_rate`, `variation_user_agent_rate`) were
promoted from fixed in-code values to independent dials — see the ruling in
`Docs/history/phase-8-content-variation.md`'s "Post-Chunk-5 amendment" section and Phase 9 §4.5. All
seventeen default to off/neutral, so **the whole set at default is byte-identical to the Phase 3
send path** (Phase 8 §4/T1). Every key is consumed by the worker via the additive `push_job.variation`
snapshot the Control stamps per job (§2.2) — the worker itself never reads `send_managed.config`.

| key | default_value | value_type | seeding migration | description |
|-----|---------------|-----------|-------------------|-------------|
| `variation_html_minify` | `{"v":false}` | boolean | send_managed_0020 | Toggle 1 — minify HTML body at worker-side MIME assembly. Off = Phase 3 body. |
| `variation_comment_injection` | `{"v":false}` | boolean | send_managed_0020 | Toggle 2 — inject invisible HTML comments after a fraction of closing tags. Off = no comments. |
| `variation_comment_intensity` | `{"v":"low"}` | string | send_managed_0020 | Parameter of toggle 2: `low`=5%/`medium`=15%/`high`=30% of closing tags. Inert while toggle 2 is off. |
| `variation_css_micro` | `{"v":false}` | boolean | send_managed_0020 | Toggle 3 — vary inline padding/margin px by ± `variation_css_magnitude_px` (skip <3px, never <0). Off = CSS untouched. |
| `variation_css_magnitude_px` | `{"v":1}` | integer | send_managed_0020 | **(amended)** Parameter of toggle 3: max ±px spread. Was a fixed ±1 in code; promoted to a dial (P8-OD-1). Inert while toggle 3 is off. |
| `variation_unique_url_params` | `{"v":false}` | boolean | send_managed_0020 | Toggle 4 — inert cache-busting `_ref=`/`_v=` param seeded from `job_id`. **NOT tracking** — permanent forward boundary (P8-10). Off = URLs untouched. |
| `variation_zero_width` | `{"v":false}` | boolean | send_managed_0020 | Toggle 5 — insert U+200D (ZWJ) at a `variation_zero_width_rate` fraction of word boundaries. **DEFAULT OFF (deliberate).** |
| `variation_zero_width_rate` | `{"v":0.05}` | number | send_managed_0020 | **(amended)** Parameter of toggle 5: fraction (0–1) of word-boundary spaces that get a ZWJ. Was a fixed 0.05 in code; promoted to a dial (P8-OD-1). Inert while toggle 5 is off. |
| `variation_encoding_rotation` | `{"v":false}` | boolean | send_managed_0020 | Toggle 6 — rotate body `Content-Transfer-Encoding` between quoted-printable/base64 (seeded). Decodes identically. |
| `variation_mime_boundary_rotation` | `{"v":false}` | boolean | send_managed_0020 | Toggle 7 — vary the MIME multipart boundary delimiter style (seeded). Off = single-part, the Phase 3 shape. |
| `variation_dynamic_preheader` | `{"v":false}` | boolean | send_managed_0020 | Toggle 8 — inject a hidden preheader span (seeded template) after `<body>`. V1 = no per-contact merge data (P8-7); tokens cleaned to empty. |
| `variation_preheader_templates` | `{"v":[7 token-free defaults]}` | array<string> | send_managed_0020 | Parameter of toggle 8: candidate templates (body-agnostic filler, P8-7 Closeout Ruling 2). Inert while toggle 8 is off. |
| `variation_header_style` | `{"v":"none"}` | string | send_managed_0020 | Toggle 9 — reorder headers to a client style (`none`/`gmail`/`outlook`/`apple`/`thunderbird`/`random`). `none` = preserve Phase 3 order (the §4 identity). |
| `variation_user_agent_injection` | `{"v":false}` | boolean | send_managed_0020 | Toggle 10 — inject a seeded `User-Agent` header for a `variation_user_agent_rate` fraction of jobs. Decoupled from `variation_header_style` (P8-6). |
| `variation_user_agent_rate` | `{"v":0.3}` | number | send_managed_0020 | **(amended)** Parameter of toggle 10: fraction (0–1) of jobs that get an injected User-Agent. Was a fixed 0.3 in code; promoted to a dial (P8-OD-1). Inert while toggle 10 is off. |
| `variation_precedence` | `{"v":"auto"}` | string | send_managed_0020 | Toggle 11 — Precedence header control (`auto`/`marketing`/`personal`). `marketing` adds `Precedence: bulk`; `auto`/`personal` omit (= Phase 3). |
| `variation_thread_index` | `{"v":false}` | boolean | send_managed_0020 | Toggle 12 — Thread-Index header. **Inert no-op in V1** (P8-8): emits nothing on or off (no sequences to index). |

## Phase 9.5 state (+11 keys, `send_managed_0021`)

Phase 9.5 seeds eleven new keys via `send_managed_0021` — the operability & control-plane safety layer
(B1–B5) plus the two config-not-code completeness dials (S10, S11). Fully schema-neutral: no table,
column, enum, constraint, or index is added or altered. Every default preserves current behavior
except `autoquarantine_mode`, whose default `shadow` is a deliberate launch-safety choice (Phase 5's
`enforce` behavior is reached by one config write — see `Docs/history/open-decisions.md`).

| key | default_value | value_type | seeding migration | description |
|-----|---------------|-----------|-------------------|-------------|
| `dispatch_enabled` | `{"v":true}` | boolean | send_managed_0021 | B1 global dispatch panic switch. `false` = the tick claims/routes/pushes nothing; queued jobs stay queued, nothing lost; reclaim/outcome/heartbeat/config-read continue. Phase 9.5 §1. |
| `autoquarantine_mode` | `{"v":"shadow"}` | string | send_managed_0021 | B2 auto-quarantine shadow ladder: `off\|shadow\|enforce`. **Default `shadow`** (launch-safety) decides but does not act, logging an `account_events` row; `enforce` is the Phase 5 behavior; `off` disables the decision entirely. Phase 9.5 §2.1. |
| `autopause_enabled` | `{"v":true}` | boolean | send_managed_0021 | B2 auto-pause (throttle) kill-switch. `false` = the module does not auto-pause accounts on a throttle outcome (manual pause/unpause unaffected). Phase 9.5 §2.2. |
| `variation_enabled` | `{"v":true}` | boolean | send_managed_0021 | B2 content-variation master kill-switch. `false` = bypass the entire Phase 8 variation layer regardless of individual toggles (baseline Phase 3 assembly, byte-identical). Phase 9.5 §2.3. |
| `alert_webhook_url` | `{"v":""}` | string | send_managed_0021 | B4 optional outbound alert webhook. Empty (default) = disabled, conditions remain visible only via `GET /v1/admin/health`. Set = POST a two-vocabulary-clean payload on each condition's rising edge, deduped until it clears. Phase 9.5 §4.3. |
| `health_metrics_window_sec` | `{"v":3600}` | integer | send_managed_0021 | B3 rolling window (seconds) for the rate/error/shadow counts in `GET /v1/admin/health`. Phase 9.5 §3.2. |
| `brain_singleton_acquire_timeout_sec` | `{"v":60}` | integer | send_managed_0021 | B5 bounded wait (seconds) for the Control **startup** singleton advisory lock before exiting non-zero. `0` = fail-fast, no wait. Phase 9.5 §5.2. Contrast `brain_singleton_reacquire_interval_sec`, which governs a **mid-life** loss. |
| `brain_singleton_reacquire_interval_sec` | `{"v":5}` | integer | send_managed_0023 | How often (seconds) a Control that **lost** the singleton advisory lock mid-life re-attempts `pg_try_advisory_lock`. While the lock is not held, dispatch/reclaim/retention are **PAUSED** (no claim, no push, no reclaim, no sweep), `readyz` reports `not_ready`, and the outcome path stays **up** so an in-flight send can still report. `0` = never re-acquire (stays paused until restarted) — also the fail-closed fallback if the row is unreadable. **READ ONCE AT STARTUP, not per tick** — unlike most keys here, a change takes effect only on the next Control start (the keeper is handed the cadence at boot). Deliberate: this key governs recovery from a state in which the DB may be unreachable, so sourcing the cadence from a fresh DB read would leave it undefined in exactly the situation it exists for. G11-OD-1 / ADR-0011 option B. |
| `unit_price_credits` | `{"v":1}` | integer | send_managed_0021 | S10 — credits charged per recipient (`quote`/`describe`). Was a documented constant; default unchanged, now retunable without a redeploy. Phase 9.5 §2.4. |
| `variation_comment_intensity_rates` | `{"v":{"low":5,"medium":15,"high":30}}` | json | send_managed_0021 | S11 — percent of closing tags injected per `variation_comment_intensity` bucket. Was hardcoded 5/15/30; default unchanged. Worker-consumed via `push_job.variation`. Phase 9.5 §2.4. |
| `variation_zero_width_rate_pct` | `{"v":5}` | integer | send_managed_0021 | S11 — percent of word boundaries eligible for zero-width insertion (`variation_zero_width`). **RESERVED / NOT-YET-CONSUMED (P9.5-OD-3, RULED)** — a documented duplicate of the already-wired `variation_zero_width_rate` (P8-OD-1, fraction 0–1); that key remains the sole source of truth for this rate to avoid two competing dials driving the same worker behavior. Phase 9.5 §2.4. |
| `variation_css_micro_params` | `{"v":{"delta_px":1,"skip_below_px":3}}` | json | send_managed_0021 | S11 — CSS micro-variation pixel delta + skip-below threshold (`variation_css_micro`). `skip_below_px` is worker-consumed via `push_job.variation` (P9.5-OD-3) — that sub-field is CONSUMED. `delta_px` is **RESERVED / NOT-YET-CONSUMED (P9.5-OD-3, RULED)** — a documented duplicate of the already-wired `variation_css_magnitude_px` (P8-OD-1), which remains the sole source of truth for the CSS magnitude. Phase 9.5 §2.4. |

## Fix-plan item 12 state (+1 key, `send_managed_0025`) — the graduated dispatch ladder

One key, seeded by the Pass-2 alignment sweep rather than by a numbered phase. Schema-neutral: no
table, column, enum, constraint, or index is added or altered — one `send_managed.config` row.

**`dispatch_enabled` is NOT replaced and NOT dropped.** The boolean has many live consumers (the
dispatch loop, the health surface, the campaign report, disaster-recovery procedure), so dropping it
would break Rule 5 and every runbook step that names it. Instead of becoming a second independent
lever — which is how half a kill switch stays pulled — it composes as a severity **FLOOR** of
`accept_and_defer`, which is exactly the behaviour it already had. Every operator who flipped that
boolean before this key existed sees identical behaviour after it.

| key | default_value | value_type | seeding migration | description |
|-----|---------------|-----------|-------------------|-------------|
| `dispatch_mode` | `{"v":"serving"}` | string | send_managed_0025 | Graduated dispatch kill-switch ladder: `serving\|accept_and_defer\|refuse_new\|stopped`, in increasing severity. `serving` = normal operation. `accept_and_defer` = `POST /v1/run` still accepts and queues, but nothing is claimed or pushed; reclaim, outcome ingestion and heartbeats continue. `refuse_new` = as `accept_and_defer`, and `POST /v1/run` is refused `503 paused` so no backlog accumulates; in-flight work still drains and is still reclaimed. `stopped` = as `refuse_new`, and the reclaim sweep is frozen too, so an expired lease is left in place rather than requeued — **nothing is stranded**: reclaim's predicate is a DB read, so the lease is still expired on the first tick after release. **Outcome ingestion is NEVER gated at any rung** — an outcome is the record of a mail that has already left, and discarding it would destroy evidence rather than prevent a send. Composes with `dispatch_enabled` as a MAXIMUM (see above), never as a second independent lever. An unrecognized value falls back to `accept_and_defer` at `level=error`: no mail leaves under an unreadable policy, and one typo does not freeze the reclaim sweep. Rejected at the admin PUT boundary too, so a typo is reported to the operator rather than silently halting mail at a rung they did not choose. Consumed by `internal/dispatch/rung.go` (claim/intake/reclaim predicates), `internal/platformapi/dispatch_rung.go` (the `/v1/run` intake gate), and `internal/adminapi/dispatch_rung.go` (the health surface's `dispatch_rung`). |

**Which surfaces report it.** `GET /v1/admin/health`'s `dispatch.dispatch_rung` names the effective
rung; `dispatch.dispatch_paused` is retained unchanged in meaning ("the effective rung is not
`serving`") and now derives from BOTH dials rather than from `dispatch_enabled` alone. `GET /v1/report`'s
`dispatch_paused` does the same. Before this key, a Control halted by anything other than that one
boolean reported `dispatch_paused: false` to every polling UI — work visibly not moving with the
dashboard insisting nothing was wrong.

## Fix-plan item 13 state (+2 keys, `send_managed_0026`) — a shadow ladder for every autonomous mutator

An **autonomous mutator** is a write nobody asked for: state the Control changes on its own
initiative, as opposed to transcribing a worker's report or executing an operator's request. Module 1
(`coldmail-verify`) has had the same property from the start — all four of its mutators default to
`recommend_only`, computing the decision and recording the recommendation while mutating nothing. This
module had it for exactly one writer (`autoquarantine_mode`, `send_managed_0021`); `send_managed_0026`
generalizes it to the two that were left.

Every dial on this ladder takes the same three values, in increasing **activity**. (Written as a list,
not a table, on purpose: `config_drift` extracts documented key names from the FIRST CELL of every
table row matching `` | `lowercase_identifier` | ``, so a values table here would register `off`,
`shadow` and `enforce` as three seeded config keys and fail the gate. Found by running it.)

- **`off`** — the decision is not evaluated at all. No write, no observation row, no log line.
- **`shadow`** — the decision IS evaluated and RECORDED; the write is NOT performed. **The seeded default.**
- **`enforce`** — the decision is evaluated and the write is performed: the pre-`0026` behaviour.

An unrecognized value falls back to **`shadow`** at `level=error` — never `enforce` (a typo must not
silently gain the power to act, least of all the power to DELETE) and never `off` (a silently disabled
control an operator believes is running is module 1's audit finding D2 reproduced here). The admin
`PUT` boundary rejects anything off-list as well, so a typo is reported to the operator instead of
quietly demoting a control they thought they had promoted.

> **⚠ THIS CHANGES DEFAULT BEHAVIOUR ON UPGRADE.** Both keys are seeded `shadow`, so a deployment that
> runs `send_managed_0026` **stops auto-pausing accounts** and **stops deleting expired `send_logs`**
> until an operator promotes each dial. Promoting `retention_mode` to `enforce` is a **REQUIRED**
> go-live step (`Docs/go-live-checklist.md`), not a nicety: left at `shadow`, `send_logs` — which holds
> recipient, subject and body — **grows without bound** and the retention promise in
> `Docs/data-retention.md` is not being kept. The sweep logs
> `retention_shadow_would_delete` with the pending row count on every tick, so the cost of leaving it
> is visible rather than silent.

| key | default_value | value_type | seeding migration | description |
|-----|---------------|-----------|-------------------|-------------|
| `autopause_mode` | `{"v":"shadow"}` | string | send_managed_0026 | Auto-pause (throttle) shadow ladder: `off\|shadow\|enforce`. `enforce` is the Phase 5 behaviour — a throttle outcome sets `accounts.paused` with `pause_reason='throttle'`. `shadow` **decides and records but does not pause**: it writes a `send_managed.account_events` observation row (`actor='system'`, `action='updated'`, `from_flags == to_flags`, `reason` starting `shadow_would_pause:`) and the account keeps sending through the throttle. `off` evaluates nothing. **Auto-UNPAUSE is never gated by this dial at any value** — the reversal keeps working, because blocking a reversal would leave real pauses in place forever, manufacturing the stuck-account incident an observation period exists to avoid. Composes with `autopause_enabled` as a **CEILING** (see below). Consumed by `internal/dispatch/account_effect.go` via `Config.AutopauseDecides()` / `Config.AutopauseWrites()`. |
| `retention_mode` | `{"v":"shadow"}` | string | send_managed_0026 | Debug-log retention sweep ladder: `off\|shadow\|enforce`. `enforce` is the Phase 7 behaviour — the batched `DELETE` of `send_logs` rows past `ttl_expires_at`. `shadow` **counts and logs what it would drop and deletes nothing**, emitting `retention_shadow_would_delete` with the pending count each tick. `off` neither counts nor deletes. The shadow count is deliberately **not** bounded by `retention_delete_batch_size` (that is a rate control for the write; reporting "1000" every tick because that is the batch would be a useless number to size a retention window against), and it is a `count(*)` rather than a rolled-back `DELETE` — a rollback would take real row locks, generate real WAL and bloat the table on every tick for an observation. **The sweep still touches `send_managed.send_logs` ONLY at every value**: job outcomes (`job_events` + final `jobs` states) are permanent by construction (§3.2), which is asserted against the source of the sweep, not just its behaviour. No boolean composes with it — `retention_sweep_interval_ms` and `retention_delete_batch_size` are rate controls, not enables. Consumed by `internal/dispatch/retention.go` via `Config.RetentionCounts()` / `Config.RetentionDeletes()`. |

**`autopause_enabled` is NOT replaced and NOT dropped**, for the same reason `dispatch_enabled`
survived `send_managed_0025`: it has live consumers and named runbook steps. It composes as a severity
**CEILING** — `autopause_enabled=false` caps the effective mode at `off`, so an operator who disabled
auto-pause never receives one regardless of what `autopause_mode` says. It never *raises* a mode:
`enabled=true` with `mode=shadow` is shadow, not enforce; a boolean cannot promote a dial an operator
left in observation.

**Why a CEILING here when `dispatch_enabled` is a FLOOR.** Both are the same rule stated on inverse
scales: *the boolean can only ever reduce what the system does, never increase it.* On the `dispatch_mode`
ladder "does less" means a **more** severe rung, so the boolean is a floor. On this ladder "does less"
means a **less** active mode, so the boolean is a ceiling. Because the two ladders run in opposite
directions, they are deliberately **separate machinery** — separate file (`internal/dispatch/mutator_mode.go`
vs `rung.go`), separate ordering table, separate names. Sharing one severity map would put `stopped` and
`enforce` at the same end of the scale, which is exactly backwards. `ADR-0014` records the decision.

**Which mutators are on the ladder, and which are deliberately not.** The audit behind this key is not
prose: `TestMutatorMode_EveryAutonomousMutatorIsDialledOrExplicitlyExcluded` **discovers** every file in
`internal/dispatch` that writes to `send_managed.*` and requires each to be either enumerated as dialled
(with its dial actually referenced in that file) or enumerated as excluded with a recorded reason. A new
autonomous mutator therefore cannot ship enforcing on day one — its file appears in neither map and the
test goes red naming it. Currently dialled: auto-quarantine (`autoquarantine_mode`), auto-pause
(`autopause_mode`), the retention sweep (`retention_mode`). Deliberately excluded, with reasons in that
test's `autonomousWriteExclusions` map: the score recompute (derived state, not an action), auto-unpause
(a reversal), the claim/lease/push/reclaim/rescan/exhaustion paths (already dialled by `dispatch_mode`),
and outcome ingestion (transcribing an external event, never autonomous).

## Both config-ordering invariants (enforced at BOTH the admin PUT boundary AND config-LOAD; CLAUDE.md Rule 1, Phase 7 §5.3 authoritative)

**Enforced at two independent points (Phase 9 CLOSEOUT, P9-OD-2 RULED-AND-CLOSED, 2026-07-27):**
the admin `PUT /v1/admin/config/{key}` boundary rejects a write that would invert either pair
BEFORE it ever lands in `send_managed.config` (validated against the counterpart key's LIVE value, not
the submitted value alone); the config-LOAD path (`internal/dispatch/config.go`'s `LoadConfig`)
independently re-checks the same invariant on every tick as a backstop, in case a bad pair reaches
the table by any OTHER path (a direct SQL write, a future second write surface). Both enforcement
points call the SAME pure functions in `internal/configordering` — they cannot silently diverge on
what "valid" means for either pair.

**Config writes are serialized against each other** (Phase 10 R4). The read of the counterpart's
value, the ordering check, and the write all happen in ONE transaction, holding a transaction-scoped
advisory lock (`Docs/advisory-locks.md`, `9_300_000_001`) that every config `PUT` takes. Without
that, two *concurrent* PUTs — one lowering the boundary key, one raising the dependent key — each
read a counterpart value that was still valid at the moment it read, so both passed the check and
both committed, landing an inverted pair anyway. Serializing costs nothing (config writes are rare
operator actions, never on a hot path) and closes the window rather than documenting it. The
practical consequence for an operator: two simultaneous PUTs to the two halves of one pair will see
one succeed and one rejected with `config_ordering_violation`, never both succeed.

- `smtp_operation_timeout_sec < job_max_processing_time_sec` — **enforced as of Phase 6 (load) /
  Phase 9 CLOSEOUT (write)** (both keys seeded: the timeout via `send_managed_0018`, the lease via
  `send_managed_0010`). A `PUT` to either key that would invert the pair is rejected with
  `400 config_ordering_violation`, naming the counterpart key, its current value, and the required
  relation; the value never lands. `LoadConfig` additionally rejects a violating set at load (strict
  `<`; equality also rejected) as defense in depth, so the Control never runs on an inverted pair even
  if one somehow reaches the table by another path.
- `box_outcome_buffer_ttl_sec >= startup_reconnect_window_sec` — **enforced as of Phase 7 (load) /
  Phase 9 CLOSEOUT (write)** (both keys seeded via `send_managed_0019`). Same dual-boundary enforcement as
  above. A reconnecting worker must still hold its buffered outcome when it re-attaches inside the
  rescan window; if the buffer TTL is shorter than the window, a worker that reconnects
  late-but-in-window has already dropped its outcome → the job is lease-reclaimed and re-sent → a
  **silent duplicate**. Bare equality is the floor; a margin (~2×, defaults `900 ≥ 2×120`) covers
  reconnect-backoff.

**Widening BOTH keys of a pair upward requires ordering the two writes** — each individual `PUT` is
validated against the counterpart's value AT THAT MOMENT, so raising the boundary key first (the
lease, or the reconnect window), then the dependent key (the timeout, or the buffer TTL), succeeds;
attempting the dependent key first fails cleanly and instructively.

A config set violating either invariant is rejected with external code `config_ordering_violation`
— never clamped or auto-corrected (Rule 1) — at whichever boundary it is attempted.

### A value of the wrong type is the OTHER config fault

An ordering violation is two individually-valid values in the wrong relation. The other way a config
set becomes unusable is a **single value that does not parse as the type its key is declared to
hold** — a quoted numeric on an integer key, a missing `{"v": …}` wrapper, a corrupt row. The admin
`PUT` boundary rejects that with `400 validation_failed` against the key's `value_type` (the table
above), so it cannot arrive through the API; a live one therefore came from a direct SQL write, a
restore/seed, or a bad migration.

Both faults are handled the SAME way, and that sameness is the contract:

- **Rejected, never clamped or defaulted** (Rule 1). `LoadConfig` returns a zeroed `Config` alongside
  the error, so no loop can run on a partially-usable set.
- **Fail-closed, not fail-stop.** The dispatch, reclaim and retention loops decline to act — nothing
  is claimed, nothing is marked sent/failed/bounced/requeued, jobs stay exactly `queued` — while the
  Control **stays alive** and never crash-loops. Repair the value and the next tick resumes on its own,
  with no restart and no redeploy.
- **Classified as a CONFIGURATION fault, not a DB outage.** The database answered perfectly; only the
  value is unusable. So a config fault must never log `db outage DETECTED`, and must never trip the
  §2.2 outage gate (whose halt the next `SELECT 1` would immediately clear, flapping
  DETECTED/RESOLVED at the 500ms base interval and burying the one log line that names the bad key).
  Enforced structurally: every config-fault error implements the `configFault` marker interface that
  `IsOutage` tests for, so a new fault class is exempt by construction rather than by remembering to
  update the classifier. Asserted behaviorally in both directions —
  `internal/dispatch/config_typefault_test.go` (type faults, every typed reader) and
  `outage_test.go`'s `TestConfigOrdering_IsNotADBOutage` (ordering faults), each with a real severed
  connection as the positive control proving a genuine outage still *does* halt and log.
- **Diagnosable:** the logged message names the offending key, at `level=error`. Triage is
  [`operator-runbook.md`](./operator-runbook.md) Step 8.

## Deliberate hardcoded safety BOUNDS (not dials — Phase 9 Chunk 3 consolidation)

CLAUDE.md Rule 1 forbids a literal that governs runtime *behavior*. It does not forbid a literal
that governs a defensive *ceiling* an operator would never want to tune per-deployment — a
fat-finger/DoS/reputation guard, not a tunable dial. Every such bound in this codebase is
documented at its definition site AND here, so "no hardcoded tuning constant" stays auditable
against a complete, not merely a plausible, list. None of these are seeded `send_managed.config` rows;
promoting one to a real dial is a future additive migration, not a silent default change.

| Site | Value | Why it's a bound, not a dial |
|---|---|---|
| `internal/adminapi/account_validate.go` `DailyLimitCeiling` | `100_000` | OD-24 (RULED, option c): the locked spec's §3/§6.4 wording references a "configurable ceiling" for `daily_limit`/`daily_limit_default`, but no such key was ever seeded in the Phase 1→9.5 chain. A fat-finger/reputation guard on per-account daily send volume, not an operator-facing tuning dial — 100k comfortably admits every real per-mailbox provider limit while catching an extra digit or two; a too-high limit degrades gracefully at send time (provider throttle → throttle class → pause) rather than causing a correctness failure. |
| `internal/dispatch/pool.go` `maxFallbackHops` | `8` | P4-19: bounds an ALREADY-acyclic fallback-pool walk (Phase 2's cycle check prevents a real cycle) — defense-in-depth against a walk that should never be long, not a behavior an operator tunes. |
| `internal/dispatch/dispatch.go` `sharedPoolNotConfiguredLogInterval` | 1 min | Governs an internal LOG line's noise floor, not send/dispatch behavior. |
| `internal/singleton/singleton.go` keeper ticker / lock-poll backoff | 5s / 200ms | Infra liveness-check cadence and advisory-lock poll rate — not a product/operator dial. |
| `internal/workerlink/stream.go` register-handshake read deadline / push_job write deadline | 15s / 10s | DoS/hang guards on the worker↔Control handshake and a wedged-peer write — the register protocol has no variable-length legitimate case, so this bounds abuse, not a real workload. |
| `internal/workerlink/workerlink.go`, `internal/httpapi/health.go` `ReadHeaderTimeout` | 5s | Standard Go `http.Server` slow-header-DoS hardening — a platform-wide idiom, not a product dial. |
| `internal/adminapi/health_alerts.go` `webhookHTTPClient` timeout / `webhookMaxAttempts` | 5s / 3 attempts | Bounds an UNTRUSTED outbound egress (`alert_webhook_url`, Rule 5): a slow or dead receiver must never hold a health-endpoint response open or retry unboundedly. After 3 attempts the alert is dropped, deliberately — the health endpoint remains the current-truth source, so a lost POST costs a notification, never state. Not a dial: an operator tuning this would be tuning how long their own monitoring stack may stall the admin API. |
| `cmd/control/main.go` shutdown grace period | 10s | A drain-on-SIGTERM bound, not a runtime behavior a campaign/operator would tune per-deployment. |
| `services/worker/src/smtp.js` QUIT-after-DATA timeout | 2000ms | QUIT is best-effort (the send already succeeded); this bound only affects how long the worker waits before moving on, never the delivery outcome. |
| `services/worker/src/index.js` register-retry cap/interval | 30 attempts / 1000ms | Startup-only bounded retry while the Control may still be coming up — a deployment-ordering concern, not a per-campaign dial. |
| `services/worker/src/variation-headers.js` `HEADER_ORDERS` (the gmail/outlook/apple/thunderbird client-style header-position tables) | 4 fixed orderings | Phase 8 toggle 9 (`variation_header_style`) selects WHICH table to apply via the seeded config *enum* (`none`/`gmail`/`outlook`/`apple`/`thunderbird`/`random`) — the table CONTENTS (which header goes in which position for a given real mail client) are an external, empirically-observed fact about how those clients format mail, not a tunable rate/threshold/timeout. Making the header orders themselves config-driven would let an operator invent a fifth "client style" with no matching real client — the dial that exists (`variation_header_style`) is the correct one; the tables it selects between are reference data, the same class as the IANA timezone database `daily_reset_timezone` validates against. |

See `Docs/history/open-decisions.md`'s Phase 4 closeout ("hardcoded-timeout triage") for the full
historical enumeration and reasoning; this table is the Phase 9 §4 consolidation of the same
audit, re-verified against the current tree.
