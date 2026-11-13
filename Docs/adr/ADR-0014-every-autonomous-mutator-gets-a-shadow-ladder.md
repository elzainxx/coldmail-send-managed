# ADR-0014: every autonomous mutator gets an off/shadow/enforce ladder seeded at `shadow`, and `autopause_enabled` is composed as a CEILING rather than replaced

<!-- file: 0014-every-autonomous-mutator-gets-a-shadow-ladder.md -->


- **Status:** accepted
- **Date:** 2026-08-05
- **Supersedes:** —
- **Superseded-by:** —

## Context

An **autonomous mutator** is a write nobody asked for: state the Control changes on its own
initiative, as distinct from transcribing a worker's report or executing an operator's request.

Module 1 (`coldmail-verify`) has had one property from the start that this module did not: *every*
autonomous mutator defaults to `recommend_only` — it computes its decision and records the
recommendation while mutating nothing, and an operator promotes it after watching what it would have
done. The cross-module alignment pass (fix-plan item 13) asked why this module had that property for
exactly one writer.

The answer was that `autoquarantine_mode` (`send_managed_0021`, Phase 9.5 §2.1) got a ladder because
quarantine is **manual-recovery-only** — an over-eager auto-quarantine strands an account until a human
intervenes, which made the observation period obviously worth building. The other two autonomous
writers were argued past:

- **Auto-pause on a throttle outcome.** §2.2 gave it a plain boolean, reasoning that a throttle pause
  is SELF-REVERSING (auto-unpause clears it after `throttle_pause_seconds`) and so needs no
  observation period.
- **The retention sweep.** Phase 7 §3 gave it rate controls (`retention_sweep_interval_ms`,
  `retention_delete_batch_size`) and no mode dial at all, because retention is a documented promise
  rather than a policy decision — the raw content is *supposed* to be dropped.

Both arguments are wrong in the same way, and in opposite directions of severity.

**Self-reversing is not the same as harmless.** A throttle pause stops a tenant's sending for its
duration. That is a live, caller-visible effect, and "it fixes itself in fifteen minutes" is not an
answer to "was that threshold calibrated for this provider?" — which is exactly the question an
operator wants to answer by watching before the system acts.

**And the retention sweep is the single most irreversible writer in the module.** A pause reverses
itself. A quarantine reverses with one admin call. A `DELETE` of `send_logs` reverses with nothing at
all: recipient, subject, body and SMTP response are gone, and flipping the dial back does not bring
them back. The writer that most needed an observation period was the one that had no dial. It is worth
naming why it was missed: the argument that got it there ("this is a promise, not a policy") was
sound about the *intent* and silent about the *blast radius* of a mis-seeded
`debug_log_retention_days` or a `ttl_expires_at` computed one unit off.

A third, forward-looking problem: nothing prevented the NEXT autonomous mutator from shipping
enforcing on day one. The audit that produced this ADR was a person reading thirteen files. Repeating
that by hand every time someone adds a writer is the failure this pass keeps finding in other
costumes — a prose audit standing in for a control.

## Decision

**1. `send_managed_0026` seeds two new keys, `autopause_mode` and `retention_mode`, on the same
three-value ladder as `autoquarantine_mode`, both defaulting to `shadow`.**

| value | behaviour |
|---|---|
| `off` | the decision is not evaluated at all: no write, no observation row, no log line |
| `shadow` (seeded) | the decision IS evaluated and RECORDED; the write is NOT performed |
| `enforce` | the decision is evaluated and the write is performed — the pre-`0026` behaviour |

Ordered by **activity**: each value does strictly more than the one before.

**2. This deliberately CHANGES DEFAULT BEHAVIOUR on upgrade, and `retention_mode=enforce` becomes a
REQUIRED go-live step.**

A migrated deployment stops auto-pausing and stops deleting expired `send_logs` until an operator
promotes each dial. The alternative — seed `enforce` to preserve behaviour, and let each deployment
opt into observation — was considered and rejected: a safety default that must be opted into is not a
default, and it would have made the whole item cosmetic for every existing deployment, which is where
the risk actually lives.

The cost is accepted with its teeth showing rather than smoothed over. Left at `shadow`,
`send_managed.send_logs` — recipient, subject, body, headers, SMTP response — **grows without bound**,
and the retention promise in `Docs/security-posture.md` is not being kept while it does. So:

- `Docs/go-live-checklist.md` carries the promotion as a REQUIRED gate, not a suggestion.
- The sweep logs `retention_shadow_would_delete` with the pending row count **on every tick**, so the
  cost of leaving it is continuously visible rather than silent. A shadow mode that observes silently
  is indistinguishable from an off switch, which is the defect below.

**3. `autopause_enabled` is NOT dropped and NOT reinterpreted. It is composed as a severity CEILING:
`enabled=false` caps the effective mode at `off`.**

Same reasoning as ADR-0013's floor: Rule 5 is additive-only, but the load-bearing argument is that the
boolean has live consumers (the health snapshot, named runbook steps). And as there, the composition
is *exactly what the boolean already meant* — `false` already meant "do not auto-pause", which is
`off` under the new name. No runbook step changes its effect.

Note what the ceiling does NOT do: it never *raises* a mode. `enabled=true` with `mode=shadow` is
shadow, not enforce. A boolean cannot promote a dial an operator left in observation, which is the
failure a naive `enabled || mode` disjunction would have introduced — the mirror image of the
`enabled && mode` conjunction ADR-0013 rejected.

**4. A CEILING here and a FLOOR in ADR-0013 are the same rule, not an inconsistency.**

*The boolean can only ever reduce what the system does, never increase it.* On the `dispatch_mode`
ladder "does less" means a **more** severe rung → the boolean is a floor. On the mutator ladder "does
less" means a **less** active mode → the boolean is a ceiling. One sentence, two scales.

**5. Because the two ladders run in opposite directions, they are separate machinery — deliberately
not shared.**

`internal/dispatch/mutator_mode.go` has its own type, its own ordering table (`mutatorActivity`), and
its own names, rather than reusing `rung.go`'s. Sharing one severity map would place `stopped` and
`enforce` at the same end of the scale, which is exactly backwards, and the composition rules would
then read as opposites when they are in fact identical. The duplication is the cheap half of that
trade.

**6. An unrecognized value falls back to `shadow` — never `enforce`, never `off`.**

`enforce` would let a typo silently gain the power to act, and for `retention_mode` the power to
`DELETE` — the precise defect a shadow ladder exists to prevent. `off` would silently disable a control
an operator believes is running, which is M1's audit finding D2 reproduced here. `shadow` is the only
value that both refuses to act AND keeps reporting. Logged at `level=error`; also rejected at the admin
`PUT` boundary, so a typo is reported to the operator rather than quietly demoting a control they
thought they had promoted. The most consequential typo is `enfroce` on `retention_mode`: under an
`enforce`-defaulting fallback it would delete; under this one it reports, unboundedly, until noticed.

**7. `shadow` must OBSERVE, not merely abstain — and the two are distinguished behaviourally, not by a
log line.**

`off` and `shadow` differ in exactly one way that matters: `shadow` evaluates the decision and records
it. If `shadow` skipped the evaluation, it would report nothing, and a dial that reports nothing is
indistinguishable from an off switch — an operator would read the silence as "nothing is tripping the
threshold" and promote a dial they had never actually observed. So:

- Auto-pause at `shadow` writes a `send_managed.account_events` observation row, using the same
  schema-neutral encoding `autoquarantine_mode`'s marker already established: `actor='system'`,
  `action='updated'` (an existing CHECK-allowed value), `from_flags == to_flags` — the equality is
  itself the signal that nothing was mutated — and `reason` starting `shadow_would_pause:`. Rule 5 is
  additive-only on those enums, so a new `would_pause` action value would be a schema change to record
  something the reason string already says unambiguously. It is a SEPARATE row from the quarantine
  marker, because one outcome can trip neither, either, or (across classes) both, and collapsing them
  would make "which mutator wanted to act" unrecoverable from the audit trail — the one question an
  operator running an observation period is trying to answer.
- The retention sweep at `shadow` runs a `count(*)` over the same predicate and the same index, and
  logs it. **Not** a `DELETE` in a rolled-back transaction: that would take real row locks, generate
  real WAL, and bloat the table on every tick for an observation — paying the write's full cost to
  throw it away. The count is deliberately **unbounded by `retention_delete_batch_size`**, because
  batch size is a rate control for the write and reporting "1000" every tick because that is the batch
  would be a meaningless number to size a retention window against.
- `off` is asserted to produce neither the write nor the observation row, so a mutation collapsing
  `shadow` into `off` cannot pass.

**8. Auto-UNPAUSE is NOT gated by `autopause_mode` at any value, and that exclusion is tested at all
three.**

Gating a reversal would leave real pauses in place forever, manufacturing exactly the stuck-account
incident the observation period exists to avoid — strictly worse than the enforcing behaviour the dial
was added to make safe. This is the general rule for this ladder: **a dial gates an ACTION, never its
REVERSAL.** The unpause is also still audited at every mode, because a silent unpause is as
unreviewable as a silent pause.

**9. The audit is a MECHANISM, not a paragraph.**

`TestMutatorMode_EveryAutonomousMutatorIsDialledOrExplicitlyExcluded` **discovers** every file in
`internal/dispatch` that writes to `send_managed.*` and requires each to be either enumerated as
dialled — with its dial actually *referenced* in that file — or enumerated as excluded, with a recorded
reason. A new autonomous mutator therefore cannot ship enforcing on day one: its file is in neither map
and the test goes red naming it. The dialled half is checked as well as the excluded half, so a site
LOSING its gate fails here rather than passing silently.

It carries an anti-vacuity floor (at least six discovered write sites) and stale-entry checks on both
maps, because "the check covers less than the property it claims to guard" is a failure this pass has
now found three times, most recently in a source-scanning assertion of its own — one that read the
whole FILE while claiming something about a FUNCTION, and stayed green through the mutation it existed
to catch.

Currently dialled: auto-quarantine, auto-pause, the retention sweep. Deliberately excluded, with the
reasons living in the test's `autonomousWriteExclusions` map rather than in prose:

- **the score recompute** — DERIVED STATE, not an action: it is the observation the quarantine and
  pause decisions then read. Shadowing it would store a stale number and make the dialled mutators act
  on a lie, which previews nothing.
- **auto-unpause** — a reversal (decision 8).
- **claim / lease / push / reclaim / rescan / exhaustion** — already dialled, by `dispatch_mode`'s
  four-rung ladder (ADR-0013). A different ladder, because a kill-switch is not a mutator.
- **outcome ingestion** — records what a worker REPORTED; never autonomous. ADR-0013 established that
  outcome ingestion is never gated at any rung, because refusing it destroys evidence rather than
  preventing an action.

**10. The retention sweep still touches `send_logs` ONLY, at every value of the dial.**

Job outcomes (`job_events` + final `jobs` states) are permanent by construction (Phase 7 §3.2). A mode
dial that could be promoted into deleting an outcome record would make this ladder a liability rather
than a safety net, so the guarantee is asserted against the **source of the sweep function**, scoped to
that function's body — not file-wide, and not only behaviourally over whichever rows a test happened to
seed.

## Consequences

- `send_managed_0026` adds two keys (55 → 57). `Docs/config-manifest.txt`,
  `Docs/config-consumers.txt` and `Docs/config-reference.md` moved in the same change, because
  `config_drift` compares all three against the live table.
- The `config_consumption` ledger entries name the **`Effective*` readers**
  (`AutopauseDecides|AutopauseWrites`, `RetentionCounts|RetentionDeletes`), not the struct fields. That
  is deliberate: it makes the gate prove each gate site goes through `mutator_mode.go`'s fail-safe
  normalization rather than reading `cfg.AutopauseMode` raw, which would honour half the control — the
  defect item 12 fixed on the dispatch side.
- **Existing tests needed updating, and the harness override is itself a hazard.** The Phase-5-era
  suites (`policy_test`, `shadowmode_test`'s `autopause_enabled` control) and the Phase 7 retention
  suite assert real ENFORCING mechanics, so `newDispatchEnv` pins both dials to `enforce` — the same
  precedent `autoquarantine_mode` set. That override could hide item 13's actual safety claim, so two
  mechanisms sit outside its reach: `TestMutatorMode_MigrationSeedsShadowNotEnforce` asserts
  `default_value`, which Rule 1 forbids any test from mutating, and the migrate tool's integration test
  asserts the seeded `value` against the raw migration chain with no harness at all.
- Down-migration: `0026`'s `.down.sql` deletes exactly two rows. It does **not** touch
  `autopause_enabled` or either retention rate key. Note the direction: reverting RESTORES enforcing
  behaviour — the *less* safe direction — because an absent `autopause_mode` row makes the boolean alone
  govern, which is the pre-`0026` behaviour exactly. And running the `.down.sql` standalone without
  reverting the source leaves a Control that will not boot, since `LoadConfig` treats an absent row as a
  hard failure rather than a default. The migrate tool's integration test asserts both halves.
- Ported to module 3 (`coldmail-send-byo`) in the same fix-plan item, on the same ladder with the same
  names and fallback. The modules do not share code, so each carries its own copy and its own discovery
  mechanism.
- **Cost flag:** item 13 was estimated MEDIUM in the fix plan and is HIGH. It is two new dials, a
  thirteen-file audit, a discovery mechanism, and a ladder that composes in the INVERSE direction to
  item 12's — times two repositories. Recorded here because the estimate being wrong is itself worth
  knowing next time.
- **A hidden test-time ceiling surfaced in the sibling module and was fixed in BOTH harnesses.** In
  `coldmail-send-byo`, `internal/dispatch` hit Go's DEFAULT 600s per-package timeout — which `go_suite`
  had never overridden — and that harness reported `FAIL … 600.007s` with every test PASSED and no
  `--- FAIL` line: a claimed bug that did not exist, arriving from the test infrastructure rather than
  from the code. The cause was measured rather than assumed, and it was *not* the new tests: the package
  needed 646s on an accumulated dev database and **113s on a clean slate** minutes later, a ~5.7x swing
  from dev-DB state behind every per-test template clone. Wall-clock there is a function of environment
  state, so an invisible fixed ceiling converts a dirty database or a loaded runner into a phantom
  failure. `tests/harness/run.sh` here now passes an explicit `-timeout "$GO_SUITE_TIMEOUT"` (default
  `40m`) and reports a breach as a distinct BUDGET failure instead of folding it into the test-failure
  branch. Applied PRE-emptively: this module's suite is the same shape on the same kind of dev database.

## Links

- CLAUDE.md Rule 1 (config-not-code), Rule 3 (real infra in tests), Rule 5 (additive-only — why
  `autopause_enabled` survives), Rule 7 (autonomous-subsystem safety controls — where all three dials
  are now listed).
- `tests/harness/run.sh` — `GO_SUITE_TIMEOUT` and `go_suite`'s separate timeout branch (last
  Consequence above).
- ADR-0013 — the `dispatch_mode` ladder, whose boolean composes as a FLOOR. Read together: the two
  ADRs state one rule on inverse scales.
- `Docs/config-reference.md` — "Fix-plan item 13 state" (both keys, the CEILING reasoning, the
  upgrade-behaviour warning, the named consumers, which mutators are excluded and why).
- `Docs/go-live-checklist.md` — the REQUIRED `retention_mode=enforce` promotion gate.
- `Docs/operator-runbook.md` — promoting and demoting each dial; reading the shadow markers.
- `Docs/security-posture.md` §"Message content at rest" — the retention promise that is not being kept while `retention_mode=shadow`, and why `debug_log_retention_days` is a privacy decision rather than a tuning one.
- `db/migrations/send_managed_0026_shadow_mode_for_every_autonomous_mutator.{up,down}.sql`.
- `services/control/internal/dispatch/mutator_mode.go` (authoritative) + `mutator_mode_test.go`;
  the gate sites in `account_effect.go` and `retention.go`; the validator case in
  `internal/adminapi/config_validate.go`.
