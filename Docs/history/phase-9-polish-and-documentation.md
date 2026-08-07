# Phase 9 — Polish and documentation

> **Status: DRAFT for review.** This is the **final** phase. It adds **no capability,
> no behavior change, and no schema change.** It finalizes `quote`/`describe`,
> audits the operator surface for completeness, consolidates the **full config
> reference** (every dial, Phase 1–8) and the **full API contract** (platform +
> operator surface) into one place, and runs a deliberate **cross-phase consistency
> audit** confirming the eight prior phases compose into one coherent system.
>
> PLAN ONLY — it specifies what is finalized, the consolidated references, and the
> verification plan, not the implementation.
>
> **Closeout (this revision):** both needs-user items are now **resolved** — P9-A
> (box provisioning: keep out-of-band for V1, but document the full procedure;
> §3.2/§5.4) and P9-B (roadmap dial attribution: one-line pointer, §4 is authority;
> §6.6). A targeted re-verification of four cross-phase seams (A–D) was run and
> proven against the spec text (§6.8): **B, C, D pass;** **A surfaced a real seam
> gap** (the box-SMTP-timeout key had no defined box-delivery mechanism) — **resolved
> additively** with a wire-only `push_job.smtp` field (no new config key, no schema
> change), and a **warn-vs-reject wording conflict** between Phase 6 §5 and Phase 7
> §5.3 was reconciled in favor of Phase 7 (reject). See the **Phase 9 — Closeout
> Delta** below.
>
> **Schema position:** the latest migration remains **`sender_0020`** (Phase 8).
> Phase 9 introduces **no** migration, **no** new config key, **no** table/column/
> enum/constraint/index. The item-A resolution adds **one additive box↔Brain wire
> field** (`push_job.smtp.operation_timeout_sec`) — protocol, not schema, and not
> part of the public REST contract (§5.5). It is the only phase whose deliverable is
> entirely documentation + audit + this one coordinated wire note.

---

## Phase 9 — Closeout Delta

This revision closes out Phase 9's two needs-user items (rulings folded in) and
records a targeted re-verification of four cross-phase seams. **Disposition +
consistency + targeted verification only — no new scope.** One additive box↔Brain
**wire** field resulted from item A (protocol, not schema).

**Rulings applied**

1. **P9-A resolved — box provisioning stays out-of-band for V1, now fully
   documented.** No `/v1/admin/boxes` CRUD in V1 (boxes are operator-deployed infra).
   The complete out-of-band provisioning/decommission procedure is now explicit in
   **§5.4a** (create id+token, where the hash lands, first-connect authorization,
   token revocation/decommission). `/v1/admin/boxes` CRUD is recorded as the **post-V1
   future item**. An operator can stand up and tear down a box from the documented
   procedure without a REST endpoint.
2. **P9-B resolved — one-line roadmap pointer, history not rewritten.** The build
   plan's per-phase dial lists are left as historical roadmap intent; a single pointer
   note (§6.6) records that **Phase 9 §4 is the authoritative consolidated config
   reference** and that all keys are seeded in the `sender_0010…0020` chain. Marked
   resolved.

**Targeted re-verification outcomes (proven in §6.8)**

- **A — box SMTP operation timeout (FINDING → resolved additively).** The key
  `smtp_operation_timeout_sec` is a real seeded key (§4.3, `sender_0018`) and is
  Brain-readable, so the §4.6(a) config-load invariant **is** enforceable Brain-side.
  **But** the box has no DB access, and the Phase 3 §2.3 `push_job.smtp` block carried
  **no timeout field** — so "the box consumes it" (Phase 6 §5) had **no defined
  delivery mechanism**. Resolved by delivering it as an **additive
  `push_job.smtp.operation_timeout_sec`** wire field (exact precedent: Phase 8's
  additive `push_job.variation`) — the Brain, which already reads the key, stamps it
  onto each push; **no new config key, no schema change**, and it is box↔Brain
  protocol, not public REST (§5.5). Also reconciled a **warn-vs-reject conflict**:
  Phase 6 §5 said the config-load check "warns, does not hard-reject," but Phase 7
  §5.3 (the config-load-validation **owner**, and the later phase) explicitly decided
  **reject** (`config_ordering_violation`) and named warn-and-run "the rejected weaker
  alternative." **Phase 7 is authoritative — the invariant is enforced by rejection;
  §4.6 stands.** The Phase 6 §5 sentence is flagged as superseded wording (§6.8 A).
- **B — P5-13 predicate column (PASS).** Phase 4 §2.1 (lines 458–472) uses the
  `throttle_paused_at`-based expired-throttle OR-terms; Phase 5 (§ruling 3, P5-13,
  lines 47–49/551) confirms the same edit was applied to Phase 4. No `updated_at`-based
  leftover in either doc. Both agree.
- **C — `event_actor='dispatcher'` reachability (PASS).** Phase 4 §1 (lines 205–209,
  295, 385) — the `queued→assigned` claim writes exactly one `job_events` row with
  actor GUC `'dispatcher'` (DB-enforced: every state change writes one event). Every
  enum value has a real emitter (table in §6.1a).
- **D — report distinguishes waiting-vs-failed (PASS).** The public `report` contract
  (§5.3) renders a capacity-waiting job as clean pending `state:"queued"`, distinct
  from terminal `state:"failed"` (only true pool exhaustion). This is in the contract,
  not merely the internal state machine (Phase 5 §5.4; Phase 3 §6.3).

**Net:** three seams pass as-is; one (A) was a genuine gap, resolved additively at the
wire level with a superseded-wording flag on Phase 6 §5. **`needs-user` is now empty.**

---

## What is and is not locked here

Phase 9 is **terminal** — nothing depends on it and it depends on everything. It
therefore does not get to change anything upstream; it only records, reconciles,
and verifies. The following are **carried forward and confirmed, not re-decided**:

- The **7 locked enums** (Phase 1 §3), all **additive-only forever**.
- The **39 seeded config keys** across `sender_0010/0017/0018/0019/0020` — this
  document is their single consolidated reference (§4), but it changes none of them.
- The **two config-ordering invariants** (P6-11, P7-11), enforced at config-load
  per Phase 7 §5.3.
- The **two-vocabulary error discipline** and the **additive-only versioning rule**
  (Phase 2 §1), which the consolidated API contract (§5) restates verbatim.
- The **byte-identical safety property** (Phase 8 §4 / T1) and the **locked
  send-path security properties** (Phase 3 §3–§4) — untouched, unweakened.

Phase-9 decisions carry the tag `P9-n` and live in the decision log (§9).

---

## 0. Inputs and locked constraints this phase builds on

Read before this doc: the build plan (Phase 9, lines 272–284), both architecture
diagrams, and the CLOSED Phase 1–8 specs. Their schemas and contracts are locked.

- **Scope (build plan "Locked context").** Shared SMTP pool, all accounts owned by
  us. **Single send only, no sequences.** Reply-To per campaign. Bounce/complaint
  ingestion and open/click tracking are **post-V1**. Users supply only a mail list
  and a template; everything else is backend-side.
- **Boundary.** Thin module. The **platform** owns campaigns, CSV upload, template
  storage/rendering, credits, quotas, `job_items`, and status polling. The **module**
  owns the SMTP account pool, routing, account status, retries, pacing, and per-item
  outcome reporting.
- **Flat unit-price credit model.** The platform charges credits; the module's job
  is to **quote honestly against what it will actually consume** (§1) and to
  **describe its tiers/pricing shape honestly** (§2).
- **The API is the product.** There is no UI. The consolidated contract (§5) is the
  entire interface a UI team or an operator sees. The Phase 9 bar is that both could
  work **without asking a question**.

---

## 1. `quote` accuracy — honest cost against real consumption

**Owner of build-plan Phase 9 item "quote accuracy against real cost."** Phase 3
§6.1 shipped `quote` as a deliberate trivial pass-through and said so ("Real pricing
tiers and `quote` accuracy against actual cost are Phase 9"). Phase 9 finalizes it.

### 1.1 What `quote` computes (finalized)

Under the platform's **flat price-per-unit** model and the module's **single-send,
no-sequences** scope, the honest cost is exactly:

```
cost = recipient_count × unit_price_credits
```

- **`unit_price_credits`** — credits charged per recipient (the Phase 3 documented
  constant, default **1 credit/recipient**). It is the module's declared unit price,
  surfaced identically in `describe` (§2) so the two can never disagree.
- **`recipient_count`** — the number of recipients the campaign will attempt. In the
  single-send model this equals the number of `job_items`; there is **no** sequence
  multiplier, no per-step fan-out, no per-recipient variable cost.
- **Request/response** (unchanged shape from Phase 3 §6.1):
  - Request: `{ "recipient_count": <int> }` (or the campaign/job_item reference the
    platform passes).
  - Response: `{ "recipient_count": <int>, "cost": <int>, "currency": "credits",
    "unit_price_credits": <int>, "tier": "<tier-id>" }` — Phase 9 adds the
    **additive** `unit_price_credits` and `tier` fields so the platform's
    preview/confirm-cost step can show the caller *why* the number is what it is.
    (Additive fields only — the Phase 3 `recipient_count`/`cost`/`currency` fields
    are unchanged; additive-only versioning rule, §5.)

### 1.2 Never under- or over-quotes — reconciled against run + report

The correctness claim is that **the quote equals what `run` + `report` actually
consume**, in both directions:

- **Never under-quotes.** A campaign creates exactly `recipient_count` `sender.jobs`
  rows (one per `job_item`, enforced by the `job_item_id` UNIQUE constraint — a
  duplicate `run` creates **no** second row, Phase 3 §6.2). Retries, requeues,
  backoff, and reclaim **do not create new jobs** — they re-dispatch the *same* job
  (Phase 5 §6, Phase 6 §2). So no execution path bills more than one unit per
  recipient. **The accepted crash-after-`250` duplicate (§6, P9 audit item) is a rare
  at-most-one-extra *delivery*, not an extra *job* — it is not billed** (billing is
  per accepted job, and the job is already `sent`; the reclaim resend reuses the same
  `job_id`, and the `provider_message_id` UNIQUE backstop keeps it a single job row).
- **Never over-quotes.** Every quoted recipient becomes a real job the module will
  attempt to send until terminal (`sent`/`bounced`/`failed`). A job that ends
  `bounced` (550) or terminal-`failed` (full pool exhaustion) was still *attempted* —
  the platform's charge policy for bounces/failures is a **platform** decision
  (the module reports the terminal state honestly via `report`; whether a bounce is
  refunded is the platform's credits logic, not the sender's). The module quotes the
  **attempt cost**, which is what it consumes.
- **Reconciliation with the platform preview/confirm-cost step.** `quote` is a
  **pure, side-effect-free** computation the platform calls during its preview step;
  it creates no jobs and holds no credits. The platform confirms cost with the caller,
  then calls `run`. Because `quote` and `run` both derive count from the same
  `job_items` set and the same `unit_price_credits`, the confirmed preview equals the
  billed amount. **`quote` is idempotent and repeatable** — calling it twice yields
  the same number; it reserves nothing.

> **Decision P9-1 — quote is attempt-priced, flat, single-send.** `cost =
> recipient_count × unit_price_credits`; no sequence/step multiplier; bounces and
> pool-exhaustion failures are *attempted* (billable at the module boundary),
> refund policy is the platform's. Additive `unit_price_credits`/`tier` fields let
> the preview explain the number. **No new capability, no schema change.**

---

## 2. `describe` — tiers and pricing (finalized)

**Owner of build-plan Phase 9 item "describe tiers and pricing."** Phase 3 §6.4
shipped a minimal static stub and said the full document is Phase 9. Phase 9
finalizes it as the module's **honest self-description** that the platform reads.

### 2.1 The tiers the sender exposes

The module's pricing shape follows the **one real product axis in V1: pool kind**
(Phase 1 `sender.pool_kind` = `shared` | `dedicated`). Everything else the operator
tunes is cost-neutral backend behavior. The finalized `describe` therefore exposes:

| Tier id | Backed by | Pricing shape | V1 price |
|---|---|---|---|
| `shared` | the global shared pool (`default_shared_pool_id`) | flat `unit_price_credits` per recipient | **1 credit/recipient** (default) |
| `dedicated` | a user's dedicated pool, shared-fallback when exhausted (Phase 4) | flat `unit_price_credits` per recipient — **same unit price in V1** | **1 credit/recipient** (default) |

- **V1 reality: the two tiers price identically.** Dedicated pools are a
  *deliverability/isolation* feature (Phase 1/4), **not** a price differentiator in
  V1. `describe` states this honestly rather than inventing a premium the billing
  model does not charge. The **shape** (per-tier unit price) is in place so a future
  dedicated premium is an **additive value change**, never a contract change.
- **Honesty rule.** `describe` must never advertise a tier, capability, or price the
  module does not actually implement. It reflects: single-send only (no sequences),
  Reply-To per campaign, no open/click tracking in V1, and the flat per-recipient
  unit price that `quote` (§1) uses.

### 2.2 The finalized `describe` response

```json
{
  "module": "sender",
  "api_version": "v1",
  "capabilities": {
    "single_send": true,
    "sequences": false,
    "reply_to_per_campaign": true,
    "open_tracking": false,
    "click_tracking": false,
    "content_variation": true
  },
  "pricing": {
    "currency": "credits",
    "model": "flat_per_recipient",
    "unit_price_credits": 1,
    "tiers": [
      { "id": "shared",    "unit_price_credits": 1, "description": "Global shared SMTP pool." },
      { "id": "dedicated", "unit_price_credits": 1, "description": "User-dedicated pool with shared fallback." }
    ]
  }
}
```

- `pricing.unit_price_credits` is the **same constant** `quote` (§1) multiplies by —
  single source of truth, so `describe` and `quote` can never diverge.
- `capabilities.content_variation: true` reflects that Phase 8 exists as
  operator-tunable behavior; it exposes **no** per-toggle detail (variation is
  operator-only tuning, not a platform-visible or priced capability — Phase 8 §3).

> **Decision P9-2 — describe mirrors quote and the pool-kind axis.** Two tiers
> (`shared`/`dedicated`), flat per-recipient price, **identical unit price in V1**
> (dedicated is isolation, not a premium). The per-tier `unit_price_credits` shape
> makes a future premium an additive value change. **No new capability.**

---

## 3. Admin-surface completeness audit

**Owner of build-plan Phase 9 item "admin surface completeness."** This audits
`/v1/admin/*` against everything an operator must do to run the system end to end,
and either confirms coverage or flags a gap (additive only, or Open Decision).

### 3.1 The operator task list vs the endpoints

| Operator must be able to… | Endpoint(s) | Source | Status |
|---|---|---|---|
| **Set `default_shared_pool_id`** (the required Phase 4 setup step) | `PUT /v1/admin/config/default_shared_pool_id` | Phase 2 §6.3; Phase 4 §12 note | ✅ covered |
| Create / read / update / delete accounts | `POST/GET/PATCH/DELETE /v1/admin/accounts[/{id}]` | Phase 2 §2 | ✅ covered |
| Manage credentials (encrypt-on-write, unchanged-secret path) | `POST`/`PATCH /v1/admin/accounts[/{id}]` (Encrypt-only; Brain-only decrypt) | Phase 2 §2.2/§2.4, §3 | ✅ covered |
| Create / read / update / delete pools (shared vs dedicated, fallback) | `POST/GET/PATCH/DELETE /v1/admin/pools[/{id}]` | Phase 2 §5 | ✅ covered |
| **Quarantine / unquarantine** an account | `POST /v1/admin/accounts/{id}/quarantine` \| `/unquarantine` | Phase 2 §7.1 | ✅ covered |
| **Pause / unpause** an account (manual) | `POST /v1/admin/accounts/{id}/pause` \| `/unpause` | Phase 2 §7.1 | ✅ covered |
| **Disable / enable** an account | `POST /v1/admin/accounts/{id}/disable` \| `/enable` | Phase 2 §7.1 | ✅ covered |
| Read **every** config key; update **every** writable key | `GET /v1/admin/config`, `GET /v1/admin/config/{key}`, `PUT /v1/admin/config/{key}` | Phase 2 §6 | ✅ covered |
| Observe account status (score, flags, pause reason, daily counters) | `GET /v1/admin/accounts/{id}` and list (returns `status` object) | Phase 2 §2.1/§2.3 | ✅ covered |
| Read an account's audit trail (who did what, when) | `GET /v1/admin/accounts/{id}/events` | Phase 2 §7.4 | ✅ covered |
| **Provision / rotate / retire a sender box** (create `sender.boxes` row + token) | **— none —** boxes are seeded **out-of-band** (Phase 3 §3.1) | Phase 3 §3.1 | ⚠️ **gap — see P9-A** |
| Observe **fleet/job health** (live box liveness, per-campaign job counts) beyond per-tenant `report` | **— none via admin surface —** `report` is platform-facing/per-tenant | Phase 3 §6.3 | ⚠️ **noted — see P9-A** |

### 3.2 The one genuine gap — box provisioning has no operator endpoint

Every operational task the build plan and Phases 2/4/5 name has a covered endpoint
**except** box lifecycle. Phase 3 §3.1 explicitly registers boxes **out-of-band**:
"their row seeded with a `token_hash`, same discipline as Phase 2's admin keys." That
was correct for Phase 3 (boxes are interchangeable and rarely change), but Phase 9's
completeness bar asks whether an operator can run the whole system through the API.
Provisioning a new box, rotating its token, or retiring a dead one currently requires
a **direct DB seed**, not an API call.

This is **flagged, not fixed** — see **§9 P9-A**. It is an *additive* `/v1/admin/boxes`
CRUD surface if adopted (new routes only; the `sender.boxes` table already exists from
Phase 1, so **no schema change**), or an accepted out-of-band operational step if you
prefer to keep box provisioning a deploy-time concern. **Recommendation:** adopt the
additive endpoint post-V1 — it is not required for a *running* V1 (boxes are seeded at
deploy), and adding it later breaks nothing. Not built here.

> **No other gap found.** The rest of `/v1/admin/*` covers the operator's full
> account/pool/config/status/audit workflow. Fleet observability beyond per-tenant
> `report` is folded into the same P9-A disposition (an admin read surface, additive
> if wanted, not required for V1).

---

## 4. THE FULL CONFIG REFERENCE — every dial, Phase 1–8

**The centerpiece (config-not-code payoff).** One table, every seeded `sender.config`
key across all migrations, with key · type · default · allowed range/values ·
consuming subsystem · effect. **42 keys** (amended from 39 post-Chunk-5, P8-OD-1 —
§4.5 grew from 14 to 17 Phase 8 keys). All are managed through the same
`GET/PUT /v1/admin/config[/{key}]` endpoints (§5.6) and take effect without a
redeploy (each consumer re-reads config; Phase 2 §6.5 / Phase 4 dispatch-tick re-read).

> **`value_type` is an informal application-layer hint**, not a DB-enforced type —
> the DB stores opaque `{"v": …}` JSONB (Phase 1 §5, Phase 2 §6.3). Booleans are
> stored `{"v": true}`; JSON-object dials use `json`.

### 4.1 Phase 1 — `sender_0010` (14 keys, the foundation)

| Key | Type | Default | Allowed / range | Consumed by | Effect |
|---|---|---|---|---|---|
| `daily_limit_default` | integer | `500` | `> 0`, ≤ sane ceiling | Dispatcher / account create | Default per-account daily send cap. |
| `daily_reset_timezone` | string | `"UTC"` | valid IANA tz name | Daily-consume + reset | Zone defining the calendar "day" for limits. |
| `daily_reset_hour` | integer | `0` | `0..23` | Daily reset | Hour (in tz) the daily counter resets. |
| `quarantine_threshold` | integer | `30` | `0..100` | Phase 5 scoring | Score at/under which an account auto-quarantines. |
| `retry_count_4xx` | integer | `3` | `≥ 0` | Phase 5 retry model | Same-account 4xx retries before requeue. |
| `dispatch_tick_interval_ms` | integer | `1000` | `> 0` | Phase 4 dispatch loop | Poll interval of the dispatcher; also the config re-read cadence. |
| `send_delay_min_sec` | integer | `3` | `≥ 0`, `≤ max` | Phase 6 pacing | Lower bound of the random inter-send delay per lane. |
| `send_delay_max_sec` | integer | `7` | `≥ min` | Phase 6 pacing | Upper bound of the random inter-send delay per lane. |
| `concurrency_per_user_default` | integer | `3` | `≥ 1` | Phase 4/6 lanes | Parallel-lane ceiling per user (throughput multiplier). |
| `heartbeat_interval_sec` | integer | `120` | `> 0` | Phase 6 heartbeat | Expected box heartbeat cadence. |
| `heartbeat_missed_threshold` | integer | `3` | `≥ 1` | Phase 6 dead-box detect | Missed heartbeats before a box is presumed dead (Detector A). |
| `job_max_processing_time_sec` | integer | `120` | `> smtp_operation_timeout_sec` | Phase 6 lease backstop | Per-job max processing time (lease). **Invariant §4.6 (a).** |
| `debug_log_retention_days` | integer | `7` | `≥ 0` | Phase 7 retention | `send_logs.ttl_expires_at` age before the sweep purges raw content. |
| `default_shared_pool_id` | uuid \| null | `null` | UUID of an enabled shared pool, or null | Phase 4 routing | Base-campaign shared pool. **Must be set before base sends route (Phase 4).** |

### 4.2 Phase 5 — `sender_0017` (3 keys, status/retry)

| Key | Type | Default | Allowed / range | Consumed by | Effect |
|---|---|---|---|---|---|
| `status_score_weights` | json | `{"w_fail":60,"w_prox":15,"w_recency":25,"fail_saturation":5,"recency_horizon_hours":72}` | JSON object, weights `≥ 0` | Phase 5 scoring | The account-status scoring formula weights (§3.2). Single JSONB (Phase 1 OD #9). |
| `retry_backoff_schedule` | json | `{"ladder_seconds":[30,120,600],"max_seconds":600}` | JSON object, ascending ladder | Phase 5 requeue | Per-attempt backoff ladder for transient/timeout/unknown requeues. |
| `throttle_pause_seconds` | integer | `900` | `> 0` | Phase 5 pause/requeue | Throttle-pause duration before lazy auto-clear; throttle-class requeue delay. |

### 4.3 Phase 6 — `sender_0018` (2 keys, pacing/reclaim)

| Key | Type | Default | Allowed / range | Consumed by | Effect |
|---|---|---|---|---|---|
| `reclaim_tick_interval_ms` | integer | `15000` | `> 0` | Phase 6 reclaim loop | Poll interval of the continuous heartbeat/lease reclaim loop. |
| `smtp_operation_timeout_sec` | integer | `30` | `< job_max_processing_time_sec` | **Box** (Phase 3 SMTP session), **delivered via `push_job.smtp.operation_timeout_sec`** | Box per-operation SMTP timeout. Brain-readable (enforces §4.6 a at config-load); the box has no DB access, so the Brain stamps the resolved value onto each `push_job` (additive wire field — §5.5, item-A resolution). **Invariant §4.6 (a)** — must trip before the job lease so a hang is a scored box-reported timeout, not an unscored Brain reclaim. |

### 4.4 Phase 7 — `sender_0019` (6 keys, runtime hardening)

| Key | Type | Default | Allowed / range | Consumed by | Effect |
|---|---|---|---|---|---|
| `startup_reconnect_window_sec` | integer | `120` | `≤ box_outcome_buffer_ttl_sec` | Phase 7 rescan | How long the startup rescan extends orphan leases / holds boxes before the continuous loop reclaims. **Invariant §4.6 (b).** |
| `shutdown_drain_timeout_sec` | integer | `30` | `> 0`, below deploy SIGTERM→SIGKILL grace | Phase 7 shutdown | Max graceful-drain wait for in-flight sends to report. |
| `retention_sweep_interval_ms` | integer | `3600000` | `> 0` | Phase 7 housekeeping | Tick of the retention sweep loop. |
| `retention_delete_batch_size` | integer | `1000` | `> 0` | Phase 7 retention | Rows per retention DELETE batch. |
| `rollup_granularity` | string | `"day"` | bucket granularity (`"day"` …) | Phase 7 rollup | Summary bucket granularity for debug-log rollup. |
| `box_outcome_buffer_ttl_sec` | integer | `900` | `≥ startup_reconnect_window_sec` | Phase 7 box outcome buffer | How long a box buffers an unacked outcome for replay on reconnect. **Invariant §4.6 (b)** — must outlive the reconnect window or a late reconnect finds its outcome expired → silent duplicate. |

### 4.5 Phase 8 — `sender_0020` (17 keys, content variation; all default OFF/neutral)

**Amended post-Chunk-5 (2026-07-26, RULED — P8-OD-1): 14 → 17 keys.** Three toggle
parameters (`variation_css_magnitude_px`, `variation_zero_width_rate`,
`variation_user_agent_rate`) were promoted from fixed in-code values to independent
config dials — an on/off toggle whose strength is hardcoded is only half a dial
(config-not-code, CLAUDE.md Rule 1). Appended, not a rewrite: the twelve toggles and the
byte-identical-when-off property below are unchanged. See Phase 8's "Post-Chunk-5
amendment" section for the full ruling.

All 17 are seeded off/neutral so that **with every toggle at its default the output
is byte-identical to the Phase 3 send path** (Phase 8 §4 / T1). See Phase 8 for
per-toggle behavior; consolidated here for completeness.

| Key | Type | Default | Consumed by | Effect (default = neutral / off) |
|---|---|---|---|---|
| `variation_html_minify` | boolean | `false` | Box (MIME assembly) | HTML minification. |
| `variation_comment_injection` | boolean | `false` | Box | Invisible HTML comment injection. |
| `variation_comment_intensity` | string | `"low"` | Box | Comment-injection density (`low`/…); inert while injection off. |
| `variation_css_micro` | boolean | `false` | Box | CSS micro-variation. |
| `variation_css_magnitude_px` | integer | `1` | Box | **(added post-Chunk-5)** Max ±px spread for toggle 3; inert while it is off. |
| `variation_unique_url_params` | boolean | `false` | Box | Per-recipient cache-buster URL param. **Callback-free / no-tracking forward boundary (Phase 8 §5 toggle 4, P8-10).** |
| `variation_zero_width` | boolean | `false` | Box | Zero-width character insertion. **Default OFF (deliberate).** |
| `variation_zero_width_rate` | number | `0.05` | Box | **(added post-Chunk-5)** Fraction (0–1) of word-boundary spaces that get a ZWJ; inert while toggle 5 is off. |
| `variation_encoding_rotation` | boolean | `false` | Box | Quoted-printable vs base64 rotation. |
| `variation_mime_boundary_rotation` | boolean | `false` | Box | MIME boundary string rotation. |
| `variation_dynamic_preheader` | boolean | `false` | Box | Hidden preheader span. **Token-free, body-agnostic generic filler (P8-7).** |
| `variation_preheader_templates` | array<string> | 7 token-free defaults | Box | Preheader phrase pool (body-agnostic; no over-claiming). |
| `variation_header_style` | string | `"none"` | Box | Header reordering by client style (`none`=preserve Phase 3 order). |
| `variation_user_agent_injection` | boolean | `false` | Box | User-Agent header injection (decoupled from header-style, P8-6). |
| `variation_user_agent_rate` | number | `0.3` | Box | **(added post-Chunk-5)** Fraction (0–1) of jobs that get an injected User-Agent; inert while toggle 10 is off. |
| `variation_precedence` | string | `"auto"` | Box | Precedence header control (`auto`=omit). |
| `variation_thread_index` | boolean | `false` | Box | Thread-Index. **Inert no-op in V1 (P8-8).** |

### 4.6 The config-ordering invariants — first-class, enforced at config-load

Two dial *relationships* are correctness invariants, **enforced at config-load by
rejection, not merely documented** (Phase 7 §5.3 — the config-load-validation owner).
At startup a violating set makes the Brain **refuse to start**; on a live change the
`PUT` is **rejected** with the clean external code **`config_ordering_violation`**
(two-vocabulary: the internal log names the offending pair and values; the client
sees only the safe code + message). These are validated as a **pair-relationship at
load**, not via a DB CHECK constraint (a CHECK cannot express a cross-row relationship
between two `sender.config` rows).

> **Reject, not warn (reconciled — item-A finding, §6.8).** Phase 6 §5's earlier
> wording said the module "warns … does not hard-reject" for invariant (a). Phase 7
> §5.3 — the later phase that **owns** config-load validation — explicitly decided
> **reject** for *both* invariants and named warn-and-run "the rejected weaker
> alternative" (a silent-duplicate window is worse than a loud refused boot). **Phase 7
> is authoritative: both invariants reject.** The Phase 6 §5 sentence is superseded.
>
> **Enforceability of (a) depends on the box actually using the value (item A).**
> The Brain reads `smtp_operation_timeout_sec` and can compare it to the lease at
> load — so the Brain-side check is real. For the invariant to *matter*, the box must
> apply that timeout; because the box has no DB access, the Brain delivers the
> resolved value on each push via the additive **`push_job.smtp.operation_timeout_sec`**
> wire field (§4.3, §5.5). Without that field the invariant would be enforced but
> hollow — the gap item A closed.

| # | Invariant | Why | Owner |
|---|---|---|---|
| **(a)** | `smtp_operation_timeout_sec < job_max_processing_time_sec` | An account hang must trip the box's operation timeout (→ scored box-reported timeout) **before** the job lease (→ unscored Brain reclaim + thrash). | Phase 6, P6-11 |
| **(b)** | `box_outcome_buffer_ttl_sec ≥ startup_reconnect_window_sec` (with margin) | A late-but-in-window box reconnect must still find its buffered outcome — else the outcome expired → silent duplicate. | Phase 7, P7-11 |

> **Exit-criterion tie-in (build plan Phase 9):** "Every config dial appears in the
> reference with a default and a description." §4.1–§4.5 = **all 42 keys** (amended
> from 39, P8-OD-1), each with type, default, range, consumer, and effect, plus §4.6's
> two invariants as first-class entries. ✅

---

## 5. THE FULL API CONTRACT — platform + operator surface, one reference

**Owner of build-plan Phase 9 item "API contract documentation."** The test of done:
a UI team could build against this **without asking a question**. Two surfaces,
**distinct auth domains, never sharing data** (Phase 2 §1.1/§4.4).

### 5.1 Conventions (both surfaces)

- **Versioning:** all routes under `/v1/`; **additive-only** — never rename or
  repurpose a field or route; only new fields/routes (Phase 2 §1.1).
- **Transport:** JSON over HTTPS; request bodies `application/json; charset=utf-8`.
- **Timestamps:** RFC 3339 UTC (`2026-07-21T14:03:00Z`). **IDs:** UUID strings.
- **Lists:** cursor pagination — `?limit=` (default 50, max 200) + cursor.
- **The two-vocabulary error envelope** (every non-2xx, both surfaces — Phase 2 §1.2):
  ```json
  { "error": {
      "code": "validation_failed",
      "message": "One or more fields are invalid.",
      "details": [ { "field": "smtp_port", "issue": "out_of_range" } ],
      "request_id": "req_01J..." } }
  ```
  `code` is a stable external-status enum; `message` is fixed per code (no
  interpolation of untrusted/sensitive values); `details[].issue` ∈ {`required`,
  `out_of_range`, `malformed`, `not_found`, `conflict`, `forbidden_value`};
  `request_id` bridges to the rich **internal** reason in logs. **The envelope never
  carries** credentials, ciphertext, key ids, SQL, stack traces, SQLSTATE, internal
  hostnames, or the internal reason.

### 5.2 External status codes (additive-only enum — Phase 2 §1.3, + Phase 7 addition)

| HTTP | `error.code` | When |
|---|---|---|
| 400 | `validation_failed` | Body failed field validation (see `details[]`). |
| 400 | `config_ordering_violation` | A config `PUT` would break invariant §4.6 (a) or (b). *(Phase 7 §5.3.)* |
| 401 | `unauthenticated` | Missing/invalid API key. |
| 403 | `forbidden` | Reserved for future scoped roles; unused in V1's single-scope model. |
| 404 | `not_found` | Resource id / config key does not exist. |
| 409 | `conflict` | State/RI conflict (delete pool with accounts; duplicate within pool; limit-below-counter; system user unprovisioned; duplicate `run`). |
| 422 | `unprocessable` | Well-formed but semantically illegal (illegal state transition, fallback cycle, owner-rule violation). |
| 429 | `rate_limited` | Admin rate limit — off by default in V1. |
| 500 | `internal_error` | Unexpected. Generic message only; reason in logs. |

### 5.3 Platform-facing surface (per-tenant isolated; auth domain A)

> **R2-5 RULED 2026-08-07 — the two `user_id` rows in the table below are STALE. Do not implement from
> it; the authority is [`api-contract.md`](../api-contract.md).** This is a quick-reference table in a
> CLOSED phase spec, and CLAUDE.md §8 forbids editing a locked spec as part of build work — so the
> ruling is to **annotate, not rewrite**, and the rows stay exactly as written for the historical
> record. What is stale, precisely:
>
> - **`POST /v1/run`** shows the body as `{"job_item_id": "uuid"}`. `user_id` is **also REQUIRED** (the
>   owning tenant). Omitting it returns `400 validation_failed` / `user_id_required`
>   (`internal/platformapi/handlers.go:144`), not a `202`.
> - **`GET /v1/report`** shows the selector as `?job_id=` | `?job_item_id=` | `?campaign_id=`.
>   **`?user_id=<uuid>` is a REQUIRED tenant selector in addition to one of those**, enforced at
>   `handlers.go:219` with the same error.
>
> Both were verified against the code and against `api-contract.md` §"Platform-facing surface" on
> 2026-08-07 — the living contract is correct on both routes, and it is the document `contract_drift`
> actually verifies against, so no gate could have caught this table. That is the reason for the
> annotation: a stale example inside a frozen doc is invisible to every automated check in the repo,
> and the only remaining control is a reader's eye. **Second row noted:** the item as originally filed
> named `run` only; `report` was found to have the same omission while ruling it.

| Method · Route | Purpose | Request | Success response |
|---|---|---|---|
| `POST /v1/quote` | Cost preview (pure, no side effects, idempotent — §1) | `{ "recipient_count": <int> }` | `200 { recipient_count, cost, currency:"credits", unit_price_credits, tier }` |
| `POST /v1/run` | Async kickoff — create one job, return immediately | `{ "job_item_id": "uuid" }` | `202 { "job_id":"uuid", "state":"queued" }`; duplicate `job_item_id` → existing job or `409 conflict` |
| `GET /v1/report` | Poll live per-item state (clean external state only) | `?job_id=` \| `?job_item_id=` \| `?campaign_id=` | `200 { job_id, state, provider_message_id?, completed_at?, attempt_count }` |
| `GET /v1/describe` | Module self-description: capabilities + tiers/pricing (§2) | — | `200` (the §2.2 document) |

- **`report.state`** ∈ the `sender.job_state` enum (`queued`/`assigned`/`sending`/
  `sent`/`failed`/`bounced`). **`provider_message_id`** appears only when `sent`.
  **Never** returned: SMTP text, `error_reason`, `error_class`, credentials, account
  identities, or any internal mechanic (two-vocabulary rule).
- **"Waiting for capacity" renders as `state:"queued"`** — a clean pending status,
  never `failed`, never an error (Phase 5 §5.4). Only true pool exhaustion produces
  `state:"failed"`. An **additive**, coarse `status_detail:"waiting_for_capacity"`
  MAY be surfaced in a future phase; V1 renders plain `queued`.
- **Per-tenant isolation:** a caller for user A can never see user B's jobs
  (Phase 3 §6).

### 5.4 Operator-facing surface `/v1/admin/*` (auth domain B — separate from A)

**Accounts & lifecycle** (Phase 2 §2, §7):

| Method · Route | Purpose |
|---|---|
| `POST /v1/admin/accounts` | Create (credentials encrypted on write). |
| `GET /v1/admin/accounts` / `GET /v1/admin/accounts/{id}` | List (cursor, filters) / read one (incl. `status`). |
| `PATCH /v1/admin/accounts/{id}` | Update (incl. unchanged-secret path). |
| `DELETE /v1/admin/accounts/{id}` | Hard delete (no FK referents) — else use `/disable`. |
| `POST /v1/admin/accounts/{id}/quarantine` \| `/unquarantine` | Manual quarantine toggle (precondition-guarded). |
| `POST /v1/admin/accounts/{id}/pause` \| `/unpause` | Manual pause (`pause_reason='manual'`); unpause refuses non-manual pauses. |
| `POST /v1/admin/accounts/{id}/disable` \| `/enable` | Disable / re-enable. |
| `GET /v1/admin/accounts/{id}/events` | Read the account's audit trail (cursor). |

**Pools** (Phase 2 §5):

| Method · Route | Purpose |
|---|---|
| `POST /v1/admin/pools` | Create (shared vs dedicated; owner rule; fallback). |
| `GET /v1/admin/pools` / `GET /v1/admin/pools/{id}` | List (filters `?kind=`, `?owner_user_id=`, `?enabled=`) / read one. |
| `PATCH /v1/admin/pools/{id}` | Update (incl. `routing_strategy`, `fallback_pool_id`, `enabled`). |
| `DELETE /v1/admin/pools/{id}` | Delete — `409 conflict` if accounts reference it. |

**Config** (Phase 2 §6 — manages **all 42 keys of §4**, amended from 39, P8-OD-1):

| Method · Route | Purpose |
|---|---|
| `GET /v1/admin/config` | All keys → `{ "items": [ <config object>, … ] }`. |
| `GET /v1/admin/config/{key}` | One config object, or `404 not_found`. |
| `PUT /v1/admin/config/{key}` | Update **`value` only** → `200` + config object. Type-validated per `value_type`; semantic checks (§4 ranges); **invariant checks (§4.6) → `config_ordering_violation`**. New keys not operator-creatable (`404`). |

### 5.4a Box provisioning — the out-of-band procedure (P9-A ruling 1, V1)

Boxes are **operator-deployed infrastructure**, not tenant/platform resources, so V1
has **no `/v1/admin/boxes` REST surface** — provisioning is a documented out-of-band
operational procedure (same discipline as seeding the admin API key and the box
`token_hash`, Phase 3 §3.1). It is complete enough that an operator can **stand up and
tear down a box without asking a question**:

1. **Create id + token.** Mint a box identity: `box_id` = a UUID the operator chooses
   (Phase 1 — box IDs are **client-supplied**, not DB-generated), and a
   high-entropy opaque `token` (the box's bearer secret, held only by the box).
2. **Seed the row / store the hash.** Insert a `sender.boxes` row with that `id`,
   `token_hash` = a one-way hash of the token (**never** the plaintext — Phase 1
   `sender.boxes` stores `token_hash` only), `status='active'`, `last_heartbeat_at`
   null, `box_metadata` optional. The plaintext token is delivered to the box's
   deploy config out-of-band and lives **only** on the box.
3. **Register / authorize on first connect.** The box dials the Brain and opens the
   mTLS stream, then sends `register {box_id, token}` (Phase 3 §3.1). The Brain hashes
   the presented token and compares to `sender.boxes.token_hash` for that `box_id`; a
   match → `register_ack{ok}` and the box begins receiving `push_job`s. A missing
   `box_id` or a **mismatched** token is rejected (a different box cannot hijack an
   existing id without the token). A `register` for an existing id **with** the
   matching token is a benign **re-registration** (boxes are interchangeable/stateless).
4. **Decommission / revoke.** To retire a box, set its row `status='disabled'` (or
   delete it) and **rotate away its token** (clear/replace `token_hash`). On the next
   connect the now-revoked token no longer matches → the box is refused; in-flight
   jobs it held fall to the Phase 6 reclaim loop and re-dispatch to a live box. Token
   **rotation** is the same procedure: write a new `token_hash`, roll the box's deploy
   secret.

> **Post-V1 future item:** an additive `/v1/admin/boxes` CRUD (+ token-rotation
> endpoint) and a fleet/health read surface. The `sender.boxes` table already exists
> (Phase 1), so adopting it later is **routes-only, no schema change** — it does not
> alter this V1 procedure, only automates it.

> **Box provisioning is therefore NOT on the admin REST surface in V1** — by the P9-A
> ruling, documented above rather than endpointed.

### 5.5 What is NOT part of the public REST contract

The **box↔Brain protocol** is internal (mTLS channel, Phase 3 §3) and is **not**
part of either public surface. The **wire-only box↔Brain extensions** are:

- the Phase 8 **`push_job.variation`** block and the deterministic `Message-ID`
  refinement (P8-2/P8-5); and
- the **`push_job.smtp.operation_timeout_sec`** field (this closeout's item-A
  resolution) — the Brain stamps the resolved `smtp_operation_timeout_sec` (§4.3)
  onto each push so the DB-less box can apply it, making the §4.6(a) invariant real
  rather than hollow. Additive to the Phase 3 §2.3 `smtp` block (which already carries
  host/port/username/secret/security); it carries **no** credential beyond the
  existing `secret`, and absent it the box falls back to its built-in default (the
  field only tightens the bound the Brain already governs).

None of these appear in `quote`/`run`/`report`/`describe` or `/v1/admin/*`. Keep the
two vocabularies separate: **additive-only versioning governs the public REST contract;
the box protocol evolves independently** and carries no credential to any client
beyond the per-job `secret` (Phase 3 §4.4).

> **Exit-criterion tie-in (build plan Phase 9):** "The API contract is complete
> enough for a UI to be built against it without asking questions." §5.1–§5.5 give
> every endpoint, request/response shape, the shared error envelope + code enum, the
> additive-only rule, and the explicit public/internal boundary. ✅

---

## 6. Cross-phase consistency audit

**The most valuable part of Phase 9** — a deliberate pass confirming the eight phases
compose into one coherent system. Findings that are *contradictions* are flagged as
Open Decisions (§9), **never silently fixed**.

### 6.1 Every enum value is reachable / used

| Enum | Value | Reached by |
|---|---|---|
| `job_state` | `queued` | `run` creates it; requeue returns to it. |
| | `assigned` | dispatcher claim (Phase 4). |
| | `sending` | box send in flight (Phase 3/4). |
| | `sent` | `report{accepted}` with `provider_message_id` (Phase 3 §5.3). |
| | `failed` | terminal — **only** full pool exhaustion (Phase 5 §5). |
| | `bounced` | 550 permanent (Phase 5 taxonomy row). |
| `error_class` | `bounce_permanent` | 550/no-such-user (Phase 5 §1). |
| | `transport_4xx` | generic 4xx (Phase 5). |
| | `auth_failure` | 535/534/530/…, `starttls_unavailable` where AUTH required (Phase 5). |
| | `throttle` | throttle/daily-limit reply (Phase 5). |
| | `timeout` | box `error_reason=timeout` (Phase 6 §2.5). |
| | `unknown` | unmapped box `error_reason` (Phase 5 §1). |
| `pool_kind` | `shared`, `dedicated` | pool create (Phase 2 §5); routing (Phase 4). |
| `routing_strategy` | `random`, `round_robin`, `one_by_one` | per-pool column (Phase 4 §1). |
| `box_status` | `active`, `stale`, `disabled` | register/heartbeat/reclaim (Phase 3/6/7). |
| `event_actor` | `dispatcher`, `box`, `reclaim_loop`, `admin`, `system` | each writes events in its phase. |
| `pause_reason` | `daily_limit`, `throttle`, `manual` | daily reset / Phase 5 throttle / manual pause. |

**All values reachable.** No orphan enum value. (`box_status='stale'` and
`event_actor='reclaim_loop'` were seeded early and are used from Phase 6/7 — confirmed
reachable, not dead.)

### 6.1a Every `event_actor` has a real emitter (item-C re-verification)

Reachability of an enum value in the DB is not enough — each must be *written* by a
real code path. Confirmed emitter per actor:

| `event_actor` | Real emitter (writes a `job_events`/`account_events` row) |
|---|---|
| `dispatcher` | The Phase 4 claim (`queued→assigned`, actor GUC `'dispatcher'`, Phase 4 §1 lines 205–209/295/385) and the outcome-handler requeue (`assigned→queued`, §2.2 line 881). |
| `box` | A box-**reported** outcome (`sending→sent`/`bounced`/reported-`timeout` requeue), Phase 3 §2.4 / Phase 5 §2.1. |
| `reclaim_loop` | A Brain-**declared** timeout reclaim of a silent/expired-lease job (`sending→queued`, no send outcome, no score recompute), Phase 6 §2.4. |
| `admin` | Every `sender.account_events` row from an operator lifecycle action, Phase 2 §7.4. |
| `system` | The `run` genesis event (`→queued`, actor GUC `'system'`), Phase 3 §6.2. |

**Every actor value has a distinct, real emitter** — none is defined-but-unwritten.
(Item C specifically confirmed: `dispatcher` is emitted, DB-enforced by the
"every state change writes exactly one event" trigger, Phase 4 line 205.)

### 6.2 Every state transition has exactly one owner

| Transition | Sole owner |
|---|---|
| `→ queued` (genesis) | `run` (Phase 3 §6.2). |
| `queued → assigned` | dispatcher claim CTE (Phase 4 §1). |
| `assigned → sending` | box push/send start (Phase 3). |
| `sending → sent` | Brain outcome handler on `report{accepted}` (Phase 3 §5.3). |
| `sending → bounced` | Brain outcome handler on 550 (Phase 5). |
| `sending → queued` (requeue — the **only** reassignment mechanism) | Brain outcome handler (Phase 5 §6) **and** the reclaim loop (Phase 6 §2) — see note. |
| `queued → failed` (terminal) | Brain outcome handler on pool-exhaustion (Phase 5 §5). |

- **Note on the two requeue paths.** `sending → queued` has two *triggers* (an
  outcome-driven requeue after a transient failure, and a reclaim-driven requeue of a
  silent/expired-lease job), but **one owner concept**: the Brain. The reclaim loop's
  UPDATE is state-guarded (`WHERE state IN ('assigned','sending')`, Phase 6 §2.1) so a
  report and a reclaim can never double-move the same job. **Not a contradiction** —
  two triggers, one guarded owner, verified race-safe. Confirmed.

### 6.3 The accepted-duplicate window — one authoritative place, referenced elsewhere

The crash-after-`250`-before-persist duplicate is documented **authoritatively in
Phase 6 §2.6** ("Reclaim + idempotency: the accepted duplicate window"). Every other
mention **references** the invariant rather than redefining it: Phase 1 `#7` (the
accepted-risk decision), Phase 3 §5.3 (idempotency mechanism), Phase 7 §1.2 (rescan
minimizes but inherits it), Phase 8 §7 (deterministic seeding makes the duplicate
byte-identical, "un-improved" only in `Message-ID`/`Date`). **One definition, consistent
references — no divergent second definition.** Confirmed.

### 6.4 "Waiting for capacity" vs failed surfaces correctly through `report`

Phase 5 §5.4 (ruling 5) requires a capacity-waiting job to read as clean pending
`state:"queued"`, never `failed`/error; only pool exhaustion → `failed`. Phase 3
§6.3 `report` returns `state` only (no reason). **The two agree**: a waiting job is
`queued` in `sender.jobs`, so `report` renders `queued` — indistinguishable from an
unclaimed job, which is the intended platform-facing behavior. Confirmed. (The
optional additive `status_detail:"waiting_for_capacity"` remains a future nicety, §5.3.)

### 6.5 Coordinated cross-phase edits reflected in BOTH source and target

The module uses a **coordinated cross-spec note** pattern (a spec that introduces a
box-consumed field/behavior adds a matching note in the consuming spec). Audit:

| Coordinated edit | Source doc | Target doc | Reflected in both? |
|---|---|---|---|
| **P5-13** — Phase 4 §2.1 claim predicate gains a reason-scoped OR-term for waiting-vs-failed | Phase 5 (§close, P5-13) | Phase 4 §2.1 | ✅ both |
| **Phase 3 box-behavior note** — `smtp_operation_timeout_sec` consumed by box | Phase 6 (§5, P6-11) | Phase 3 §0/box notes | ✅ both — **but the *delivery mechanism* to the DB-less box was undefined; closed by item A (§6.8 A) with the additive `push_job.smtp.operation_timeout_sec` wire field** |
| **Phase 3 box-behavior note** — box outcome buffer / reconnect replay | Phase 7 (§1.2) | Phase 3 coordinated note | ✅ both |
| **Phase 8 protocol extension** — `push_job.variation` block + deterministic `Message-ID` | Phase 8 (§2.2/§7, P8-2/P8-5) | Phase 3 Closeout Delta ("Phase 8 coordinated note") | ✅ both |

**All four coordinated edits appear in both their source and target docs.** No
one-sided edit found. Confirmed.

### 6.6 Documentation-vs-reality: build-plan "config dials introduced" attribution (P9-B — RESOLVED)

**One drift found; resolved by ruling 2 with a one-line pointer, history not
rewritten.** The build plan lists config dials as "introduced" in Phase 2 (per-account
daily limit, daily reset schedule, quarantine threshold) and Phase 4 (routing strategy
per pool, dispatch tick interval). The actual specs seed **all of those keys in Phase 1
`sender_0010`** (`daily_limit_default`, `daily_reset_timezone`/`_hour`,
`quarantine_threshold`, `dispatch_tick_interval_ms`), and **`routing_strategy` is a
per-pool column, not a config key**. Phases 2 and 4 introduced **zero new
`sender.config` keys** — they built the *admin surface* and the *consumers* for keys
seeded in Phase 1. This is a **build-plan roadmap imprecision, not a spec
contradiction.**

**Resolution (P9-B, ruling 2):** the build plan's per-phase dial lists are **not
rewritten** — they stand as historical roadmap intent. The authority is reset by a
single pointer:

> **Config-reference authority.** **Phase 9 §4 is the authoritative consolidated
> config reference.** Every config key is seeded in the Phase 1→8 migration chain
> (`sender_0010…0020`); the build plan's per-phase "config dials introduced" lists are
> roadmap intent, not the seed-location record. Where the two differ, **§4 is truth.**

That pointer belongs in the build plan (a one-line editorial note) and is restated here
so the authority is unambiguous from within the spec set. **P9-B resolved.**

### 6.7 Migration chain integrity

`sender_0001…0012` (Phase 1 schema) → `0013`/`0014` (Phase 2) → `0015` (Phase 4) →
`0016`/`0017` (Phase 5) → `0018` (Phase 6) → `0019` (Phase 7) → `0020` (Phase 8).
**Contiguous, no gaps, each with a reversible DOWN and a `schema_migrations` insert;
`sender_0020` is the latest.** Phase 7's drafted-but-not-created `send_log_summaries`
`sender_0020` was **explicitly not created** (P7-1 chose schema-neutral), so there is
**no collision** with Phase 8's `sender_0020`. Confirmed — the number was free when
Phase 8 claimed it.

> **Net audit result (post-closeout):** the eight phases compose coherently. The
> initial pass found **zero spec-level contradictions** and two dispositions (P9-A,
> P9-B) — both now **resolved** (§ Closeout Delta; §5.4a, §6.6). The targeted
> re-verification (§6.8) then proved seams B/C/D clean and surfaced **one real seam
> gap** (item A: the box-timeout delivery mechanism was undefined) plus **one
> warn-vs-reject wording conflict** — both **resolved** (A additively at the wire
> level; the conflict in favor of Phase 7's reject). **No open contradiction
> remains.**
>
> **Post-closeout amendment note (2026-07-26, P8-OD-1 RULED-WIDEN — read before trusting any
> key-count in this section or in §4).** Phase 8 was subsequently amended: three toggle
> parameters (`variation_css_magnitude_px`, `variation_zero_width_rate`,
> `variation_user_agent_rate`) were promoted from fixed-in-code values to independent config
> dials, appended to the SAME `sender_0020` migration referenced above (the migration-chain
> claim in this section — "0020 is the latest," "contiguous, no gaps" — is UNCHANGED and still
> true; only the ROW COUNT `sender_0020` seeds changed, 14→17). **Two specs were touched by
> this single ruling, additively, not rewritten:** this document's own §4.5 (14→17 keys,
> heading/table amended with an explicit note) and `phase-8-content-variation.md` (a new
> "Post-Chunk-5 amendment" section). **Any future cross-phase audit of this document (a repeat
> of §6.8's method, or a new one) MUST re-derive its key counts from the LIVE `sender.config`
> table / `Docs/config-manifest.txt` / `Docs/config-reference.md` — not from this section's
> prose, which was accurate at Phase 9's original closeout but predates the amendment.** The
> module-wide total is **42** keys (not the 39 this document's §4/§5.6 originally stated before
> the amendment note was added there), and the Phase 9.5 forward-pointer total is **53** (not
> 50). See `Docs/open-decisions.md` P8-OD-1 for the full disposition.

### 6.8 Targeted re-verification of four cross-phase seams (A–D, with evidence)

Not a re-assertion — each seam is proven against the actual spec text, with the finding
surfaced (not papered over) where one exists.

- **A — box SMTP operation timeout: key existence + Brain-readability + box
  consumption.** *(FINDING → resolved additively.)*
  - **Key exists:** `smtp_operation_timeout_sec` is one of the 39 (§4.3), seeded by
    `sender_0018` (Phase 6 §5, lines 700/737). ✅
  - **Brain-readable:** it lives in `sender.config`; the Brain reads it and can compare
    it to `job_max_processing_time_sec` at config-load — so the §4.6(a) invariant check
    is real (Phase 7 §5.3, lines 660–691). ✅
  - **Box consumption — the gap:** the box has **no DB access** (Phase 3 §3.4), and the
    Phase 3 §2.3 `push_job.smtp` block (lines 365–371) carried **only**
    host/port/username/secret/security — **no timeout field**. Phase 6 §5 (lines
    748–760) asserts "a value the box reads / consumed by the box" but **no spec defined
    how** the value reaches the DB-less box. So the invariant was *enforced but hollow*:
    the Brain could reject a bad ordering, yet the box had no way to actually apply the
    timeout — a genuine cross-phase seam gap, **not** a mere wording issue.
  - **Resolution (additive, per the ruling's "resolve to the enforceable version"):**
    the Brain — which already reads the key — stamps the resolved value onto each push
    via an **additive `push_job.smtp.operation_timeout_sec`** wire field (§4.3, §5.5),
    exactly the precedent set by Phase 8's additive `push_job.variation`. **No new
    config key, no schema change**; box↔Brain protocol only. If the field is absent
    (older Brain), the box falls back to its built-in default and the field merely
    tightens a bound the Brain already governs — so the change is backward-safe. **§4.6
    now stands on a real delivery path.**
  - **Secondary finding — warn vs reject (reconciled).** Phase 6 §5 (lines 753–755)
    said the module "logs an internal warning if the two cross; it does not
    hard-reject." Phase 7 §5.3 (lines 677–691) — the **owner** of config-load
    validation and the later phase — explicitly decided **reject** for both invariants
    and named warn-and-run "the rejected weaker alternative." **Phase 7 is
    authoritative: reject.** §4.6 is updated to reject; the Phase 6 §5 warn-only
    sentence is **superseded wording** (a one-line reconciliation the Phase 6 doc should
    carry, noted here; not a behavior change to any built system).
- **B — P5-13 predicate column (PASS).** Phase 4 §2.1 (lines 458–472) uses
  `pause_reason='throttle' AND NOW() >= throttle_paused_at + :throttle_pause_seconds` —
  the Phase 5 additive column (`sender_0016`), **not** any `updated_at`-based form.
  Phase 5 (ruling 3 / P5-13, lines 47–49, 546–551) confirms the same predicate was
  applied to the Phase 4 doc. **Both docs show the identical `throttle_paused_at`-based
  predicate.** No leftover, no mismatch. ✅
- **C — `event_actor='dispatcher'` reachability (PASS).** Phase 4 §1 (lines 205–209,
  295, 385) writes a `job_events` row with actor GUC `'dispatcher'` on the
  `queued→assigned` claim (and on the requeue branch, line 881), DB-enforced by "every
  state change writes exactly one event." All five actors have named emitters (§6.1a).
  ✅
- **D — report distinguishes waiting vs failed (PASS).** The **public** `report`
  contract renders a capacity-waiting job as clean pending `state:"queued"`, distinct
  from terminal `state:"failed"` (only true pool exhaustion) — Phase 5 §5.4 (lines
  720–740) with Phase 3 §6.3, restated in this doc's §5.3. This lives in the **report
  contract**, not merely the internal state machine: a job on daily-limit-paused-only
  capacity reads as pending, and only pool exhaustion yields `failed`. ✅

---

## 7. Documentation-matches-behavior

- **The documentation set describes the system as specified.** Each phase's spec is
  its own standalone document; the §4 config reference and §5 API contract are
  **derived from and consistent with** those specs (audited in §6). Where the
  roadmap (build plan) drifts from the specs, the **specs + migrations are
  authoritative** and the drift is flagged (§6.6 / P9-B), not silently reconciled in
  either direction without your ruling.
- **How docs stay current (discipline restated).** Documentation is **part of each
  phase's deliverable, never a trailing afterthought** — a phase is not closed until
  its spec, its migration(s), and any coordinated cross-spec notes are all written and
  consistent. The coordinated-note pattern (§6.5) is the mechanism that keeps a
  box-consumed change reflected in both the introducing and the consuming doc. Phase 9
  adds the **two consolidated references** (§4, §5) as the single places an operator
  or UI team reads; when a future additive change lands (e.g. P9-A box endpoints, a
  dedicated-tier premium), it updates **those tables plus the owning phase spec** in
  the same change — the discipline that kept Phases 1–8 consistent continues.
- **Verified against the running system** (build-plan exit criterion): the §8
  end-to-end pass is the mechanism — documentation claims are checked against observed
  behavior, not asserted.

---

## 8. Verification plan — end-to-end behavioral pass

**Not new unit tests** — a deliberate end-to-end pass proving the finalized module
behaves as the consolidated docs describe. Real Postgres, real box(es), a test SMTP
sink, a test key held outside the DB (Phase 3 discipline).

- **E1 — Full campaign lifecycle.** A real campaign flows `quote → run → dispatch →
  send → report` to completion. **Assert:** `quote` returns `recipient_count ×
  unit_price_credits`; `run` returns `202` immediately (no block on send); jobs move
  `queued → assigned → sending → sent`; `report` reflects each terminal state with
  `provider_message_id` on `sent`. The **billed amount equals the quoted amount**
  (§1.2).
- **E2 — `describe`/`quote` agreement.** `describe.pricing.unit_price_credits` equals
  the constant `quote` multiplies by; both tiers present; capabilities honest
  (`sequences:false`, tracking `false`). Change `unit_price_credits` (if exposed as a
  dial in a future change — in V1 it is the documented constant) and confirm both move
  together. *(V1: assert the two read the same source.)*
- **E3 — Config reference exercised (change a dial, observe the effect).** Pick a live
  dial from §4 — e.g. `send_delay_min_sec`/`_max_sec` — `PUT` a new value via
  `/v1/admin/config/{key}`, and **observe the send rate change without a redeploy**
  (config re-read cadence, Phase 2 §6.5). Assert the reference's stated "consumed by"
  and "effect" match observed behavior for a representative dial in each subsystem
  (pacing, dispatch, retention).
- **E4 — Ordering invariants reject an unsafe config.** `PUT
  smtp_operation_timeout_sec` ≥ `job_max_processing_time_sec` → **`400
  config_ordering_violation`** (invariant §4.6 a), no write. Likewise `PUT
  box_outcome_buffer_ttl_sec` < `startup_reconnect_window_sec` → rejected (invariant
  §4.6 b). Assert the clean envelope (code + safe message + `request_id`), and that
  the offending pair/values appear **only** in the internal log, never the response.
- **E5 — Accepted-duplicate surfaces correctly.** Force the crash-after-`250`-before-
  persist window (kill the box between SMTP `250` and outcome persist); reclaim
  re-dispatches. **Assert:** the job ends **exactly one** `sent` row (the
  `provider_message_id` UNIQUE backstop holds), at most one extra *delivery* (the
  accepted residual, Phase 6 §2.6), and — with variation on — the duplicate is
  **byte-identical** (deterministic `job_id` seeding, Phase 8 §7). `report` shows a
  single clean `sent`.
- **E6 — Waiting-for-capacity surfaces as clean pending.** Drive all eligible
  accounts to `daily_limit`-paused; submit a job. **Assert:** `report` shows
  `state:"queued"` (pending), **never** `failed`/error; no internal pause reason
  leaks; the job sends after the daily reset. Then exhaust the pool for real (all
  quarantined) → **`state:"failed"`** only then.
- **E7 — Byte-identical & security regression guard (carried).** Re-run Phase 8 **T1**
  (all toggles off → byte-identical to Phase 3) and Phase 3/Phase 8 **S1** (variation
  never touches credentials / TLS-before-AUTH ordering / memory-only-zeroed rules).
  Assert both still pass — Phase 9 changed nothing that could weaken them.
- **E8 — UI-buildability spot check (contract completeness).** Independently derive
  the request/response for every §5 endpoint **from the §5 tables alone** and confirm
  a caller needs no out-of-band knowledge: enum domains (`job_state`, error `code`),
  the error envelope, pagination, and the public/internal boundary are all present.
  *(This is the concrete form of the build-plan "without asking a question" criterion.)*
- **E9 — Box actually receives and applies the SMTP operation timeout (item-A guard).**
  Set `smtp_operation_timeout_sec` to a small value via `/v1/admin/config`; push a job
  to an account that accepts the TCP connection but **hangs** (never answers a command).
  **Assert:** (a) the `push_job` carries `smtp.operation_timeout_sec` = the resolved
  config value (wire field present, §5.5); (b) the **box's own operation timeout fires
  first** — before the job lease `job_max_processing_time_sec` — producing a
  **box-reported** `error_reason=timeout` (scored by Phase 5), **not** an unscored Brain
  reclaim; (c) with the field **absent** (simulated older Brain), the box falls back to
  its built-in default and still times out safely. This proves §4.6(a) is *enforceable
  in effect*, not merely checked at the Brain.
- **E10 — Box out-of-band provisioning/decommission (P9-A guard).** Follow §5.4a end to
  end with no REST endpoint: mint `box_id`+token, seed the row with `token_hash`,
  connect+`register`, receive jobs; then **decommission** (disable + rotate token) and
  confirm the revoked token is refused on reconnect and in-flight jobs reclaim to a live
  box. Proves an operator can stand up **and** tear down a box from the documented
  procedure alone.

> **Exit-criterion tie-in (build plan Phase 9):** every dial in the reference with a
> default + description (§4, checked by E3); contract complete enough to build a UI
> against without questions (§5, checked by E8); documentation matches behavior
> verified against the running system (E1–E7). ✅

---

## 9. Open Decisions

The initial audit found **zero spec-level contradictions**; the targeted
re-verification (§6.8) surfaced **one seam gap (A)** and **one wording conflict**,
both resolved. **All Phase 9 decisions are now resolved — `needs-user` is empty.**

**Resolved this closeout (rulings applied):**

- **P9-A — Box provisioning (RESOLVED — out-of-band for V1, documented).** No
  `/v1/admin/boxes` CRUD in V1; boxes are operator-deployed infra. The full
  provisioning/decommission procedure is now explicit in **§5.4a** (create id+token,
  hash storage, first-connect authorization, revocation), tested by **E10**.
  `/v1/admin/boxes` CRUD + fleet-health read surface recorded as the **post-V1 future
  item** (routes-only when adopted; `sender.boxes` already exists — no schema change).
- **P9-B — Roadmap dial attribution (RESOLVED — one-line pointer, history intact).**
  The build plan's per-phase dial lists stay as historical intent; a single pointer
  note (added to the build plan's Phase 9 section, restated in §6.6) makes **Phase 9 §4
  the authoritative config reference** and records that all keys are seeded in
  `sender_0010…0020`. §4 is truth where they differ.

**Resolved by the re-verification (§6.8):**

- **Item A — box-SMTP-timeout delivery (RESOLVED additively).** The key existed and was
  Brain-readable, but the DB-less box had **no defined way to receive it** — a real seam
  gap that would have left §4.6(a) enforceable-but-hollow. Closed with the additive
  **`push_job.smtp.operation_timeout_sec`** wire field (§4.3, §5.5; guarded by E9). **No
  new config key, no schema change** — box↔Brain protocol only.
- **Warn-vs-reject conflict (RESOLVED).** Phase 7 §5.3 (config-load-validation owner)
  wins over Phase 6 §5's older warn-only wording: both invariants **reject**
  (`config_ordering_violation`). §4.6 updated; Phase 6 §5 sentence flagged superseded.
- **Items B, C, D — PASS** as verified in §6.8 (P5-13 predicate consistent in both docs;
  `dispatcher` actor really emitted; report renders waiting distinct from failed).

**Resolved earlier in this phase (no user input needed):** P9-1 (quote attempt-priced
flat single-send), P9-2 (describe mirrors quote, pool-kind axis, identical V1 unit
price) — both finalize existing stubs with additive response fields only.

> **`needs-user` list: EMPTY.** Every Phase 9 item (P9-1, P9-2, P9-A, P9-B, and
> re-verification items A–D) is resolved. The nine-phase spec set is complete.

---

## 10. Out of scope for Phase 9

- **Any new feature, behavior change, or schema change.** Phase 9 finalizes,
  consolidates, documents, and audits. The one genuine capability gap (box
  provisioning) is **resolved by documenting the out-of-band procedure (§5.4a), not by
  building an endpoint** (P9-A).
- **Building the `/v1/admin/boxes` surface** or a fleet-observability read surface —
  **post-V1 future item** (routes-only, no schema change), not built in V1.
- **Rewriting the build plan's per-phase dial lists** — P9-B added only a one-line
  authority pointer; history is intact and §4 is the reference.
- **A dedicated-tier price premium** — the `describe` shape supports it as a future
  additive value change; V1 prices both tiers identically (§2.1).
- **Open/click tracking, bounce/complaint ingestion, sequences** — post-V1 (build
  plan); `describe` honestly reports them absent.
- **Anything that would weaken the byte-identical property (Phase 8 T1) or the
  send-path security properties (Phase 3 §3–§4)** — non-negotiable, re-guarded by E7.

---

*Phase 9 is the final phase, now closed out. With §4 (every dial) and §5 (every
endpoint + the §5.4a box procedure) an operator can stand up, run, and tear down the
system and a UI team can build against it — both without asking a question; §6 confirms
the eight phases compose coherently, and the targeted re-verification (§6.8) proved
seams B/C/D and resolved the one real gap (item A, additively) and the warn-vs-reject
conflict (Phase 7 authoritative); §1–§2 finalize honest cost and self-description;
`needs-user` is empty (§9). **Schema position: fully schema-neutral — no migration, no
new config key, no table/column/enum/constraint/index; `sender_0020` remains the latest
migration. The item-A resolution added exactly one additive box↔Brain wire field
(`push_job.smtp.operation_timeout_sec`) — protocol, not schema, and not part of the
public REST contract.** The nine-phase spec set is complete.*
