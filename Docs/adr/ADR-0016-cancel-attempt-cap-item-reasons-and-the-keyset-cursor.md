# ADR-0016: cancel, the attempt cap, the reason on every report item, and the keyset cursor

<!-- file: ADR-0016-cancel-attempt-cap-item-reasons-and-the-keyset-cursor.md -->

- **Status:** accepted
- **Date:** 2026-11-17
- **Supersedes:** —
- **Superseded-by:** —

## Context

The platform became the first real caller of this module's Domain A, and a read of that surface from the caller's side (the owner's research record, gaps M2-G1…G7) found five things a caller cannot work around:

| gap | what a caller saw |
|---|---|
| **M2-G1** | `GET /v1/report` said `failed` or `queued` and nothing about **why** — a pool that ran dry, an item that failed six times and an item nobody had tried were indistinguishable. |
| **M2-G2** | exactly one writer produced `failed` (pool exhaustion). Every other SMTP failure that was not a hard bounce returned the job to `queued` with **unbounded** retries; a persistent 554 never became terminal. |
| **M2-G3** | there was **no cancel**. A platform-side "cancelled" campaign left its items `queued`, and the dispatcher kept sending them. |
| **M2-G5** | the campaign report's cursor was built from the `(pageSize+1)`th row, so the item at **every page boundary was skipped**, silently. |
| (sweep) | the same-bug sweep over the operator listings found a second cursor flaw: the cursor carried `created_at` **truncated to the second**, so a second page re-served rows (and looped forever when a second held more than `limit` rows). |

The owner ruled all five in. Contracts are additive-only (Rule 5): nothing is renamed or removed, and the `job_state` enum gains exactly one value.

## Decision

### 1. `reason` and `error_class` on every item (additive)

Every item of the single-job report and the campaign report carries two keys, **always present**, `null` when empty: `reason` (a closed vocabulary, published as `item_reason` in `/v1/describe`) and `error_class` (the existing `error_class` enum, now published as a live `db_enum` vocabulary — it was the one enum deliberately withheld). `itemOutcome` (`platformapi/item_reason.go`) is a pure function of one row plus two live facts (the dispatch rung; whether the tenant has an eligible account), so the whole mapping is a table test. `last_error_message` is **read** only to tell a dispatcher-side requeue (never reached SMTP, `last_error_class='unknown'`) from a real SMTP failure, and is never serialized. A new column `jobs.terminal_reason` records which writer produced a `failed` row, so no text is parsed.

### 2. The attempt cap — two dials, one predicate

`job_max_failed_attempts` (6) and `job_failed_max_age_sec` (86400). Counted in **new** columns (`failed_attempt_count`, `first_failed_at`), because `attempt_count` saturates at 3 and is also bumped by requeues that never reached SMTP. Counted: a worker-reported classified failure, and the reclaim of a job that was `sending`. Never counted: `no_eligible_account`, `no_worker_available`, `push_failed`, the header-injection guard, the reclaim of a job that was only `assigned`. The decision is made in the **same guarded UPDATE** that would have requeued the job (`attempts_cap.go` builds the one SQL predicate used by `outcome.go` and `reclaim.go`): `queued` becomes `failed`, with `completed_at`, `terminal_reason='attempts_exhausted'` and the `sending → failed` event. The age cap is evaluated when a failed attempt is recorded, not by a sweep: a job that is only *waiting* is never failed by age.

### 3. Cancel — `POST /v1/cancel`, and why it needs a guard trigger

`{"campaign_id","user_id"}` → `{"campaign_id","cancelled","in_flight","already_terminal"}`. Every `queued` item of the tenant's campaign becomes the new terminal `cancelled` (one event each); `assigned`/`sending` items finish and report truthfully once. The response is the campaign's position **after** the call (a partition of its non-queued items), so a repeat call is idempotent in the strict sense.

The one thing the ruling could not leave to the caller's reading: an item that is **in flight** when the cancel lands and then **fails** would, by its normal failure path, go back to `queued` and be re-sent — a send after the platform asked us not to. Five paths can return an in-flight job to the queue (the outcome handler's classified failure, the reclaim loop, `requeue()` for four reasons). A guard in each is a guard the next one forgets, so the rule lives where every writer passes: cancel sets `jobs.cancel_requested_at` on every non-terminal item, and a `BEFORE UPDATE OF state` trigger (`fn_jobs_cancel_guard`, `send_managed_0028`) turns an `assigned|sending → queued` transition of a flagged row into `cancelled`. The claim queries skip a flagged row. Cancel therefore never changes an in-flight item's *state*; it only decides where a failure of it may go.

**Two transactions, in this order** (flag and commit; then cancel the queued rows). They are separate because of the account→job lock order (Rule 9 / ADR-0010): the outcome handler takes an account's `FOR UPDATE` and then the job row, and moving a queued row to `cancelled` takes the account's `KEY SHARE` through the event trigger. One transaction holding the in-flight rows while it did that could cycle with an outcome; committing the flag step first means the cancel step holds only `queued` rows, which no outcome path writes. Both steps lock in `id` order (as does `failExhaustedJobs` now), so two multi-row writers can wait on each other but not cycle.

**Race-freedom** rests on row locks, and is proven by forced interleaves (a held claim; a held requeue; each with the cancel **observed blocked** in `pg_stat_activity`), not by repetition: whichever of {claim, in-flight requeue} and {cancel} reaches the row first, the other sees the committed result after waiting on the lock, and no interleaving leaves an item both cancelled and sent, or queued after a cancel.

### 4. The keyset cursor

`campaignItems` keeps the key of every returned row and takes the cursor from the **last row returned**; the extra row only answers "is there another page". The three operator listings (`accounts`, `pools`, `accounts/{id}/events`) already used the last returned row but built the cursor from the second-truncated `created_at`; each resource now carries an unexported full-precision `cursorTS` for the cursor only, and the serialized `created_at` is unchanged.

## Consequences

- The platform can settle on `reason`: a `failed` item is `pool_exhausted` or `attempts_exhausted` and is not charged either way; `cancelled` is terminal and never charged; `bounce_permanent` is billable.
- **The two `failed` writers are now distinguishable**, so every assertion that read "only pool exhaustion writes `failed`" was swept (`TestPolicy_AttemptCountSaturates`, `TestAttemptToken_MonotonicAndNonSaturating` now raise the cap out of the way; the docs that said it were rewritten).
- `send_managed_0027` adds the enum value in a file of its own (a new value cannot be used in the transaction that adds it); its down is a documented forward-only no-op. `send_managed_0028`'s down refuses (23514) while any `cancelled` row exists, deliberately.
- Cancel acts on the items that **exist** when it runs. A `job_item` submitted for the same campaign afterwards is new work; the platform that cancelled decides not to submit more.
- An in-flight item that fails after a cancel ends `cancelled` with its failed attempt still recorded (counter, class, `send_logs` row) — and one that exhausts its attempt cap is still `failed`: it did fail, and that is what is reported.

## Alternatives considered

- **A campaign-level cancellation table** consulted by every requeue path. Rejected: it is read in a statement snapshot, so a requeue that began before the cancel committed could still return an item to the queue; only a flag on the **row**, serialized by the row lock, closes that window.
- **Age cap by sweep.** Rejected: a sweep fails jobs that are merely waiting (a paused module, no capacity) and would convert an operator pause into mass `failed`. Evaluating at the moment a failed attempt is recorded keeps "waiting" and "failing" apart.
- **Cancel as one transaction.** Rejected for the lock-order reason in §3.
