# ADR-0013: the dispatch kill-switch becomes a graduated rung ladder, and `dispatch_enabled` is composed into it rather than replaced

<!-- file: 0013-dispatch-kill-switch-becomes-a-graduated-rung-ladder.md -->


- **Status:** accepted
- **Date:** 2026-08-05
- **Supersedes:** —
- **Superseded-by:** —

## Context

The cross-module alignment pass (fix-plan item 12) found the same defect in all three modules: the
only dispatch kill-switch is a boolean, and a boolean answers the wrong question during an incident.

`dispatch_enabled=false` halts claiming. It does not distinguish the three genuinely different
things an operator needs to be able to do:

1. **Stop sending, keep accepting.** A provider incident or a credential rotation. Platform
   submissions must keep landing, because refusing them loses customer work permanently while
   letting them queue is recoverable. This is by far the most common halt.
2. **Stop sending AND stop accepting.** The queue is already over capacity, or the halt will outlast
   the campaign-expiry window, so accepting more work means accepting work that will expire unsent.
   A caller is better served by a clear refusal than by silent accumulation.
3. **Stop everything, including the reclaim sweep.** DB or worker-fleet surgery, where any
   background writer touching `send_managed.jobs` is the problem.

A boolean collapses (1) and (2) and cannot express (3) at all. Worse, on the reporting side
`dispatch_paused` has exactly one bit to say all of this with, so an operator reading the dashboard
mid-incident cannot tell whether intake is still open — and that ambiguity is precisely how people
stop trusting a health endpoint. This is M1's audit finding D2 in a different costume: a control that
looks normal while being partly ignored.

`send_managed_0025` therefore seeds one new key, `dispatch_mode`, with four rungs ordered by
severity:

| rung | claims | intake (`POST /v1/run`) | reclaim |
|---|---|---|---|
| `serving` (0, seeded) | ✅ | ✅ | ✅ |
| `accept_and_defer` (1) | ❌ | ✅ | ✅ |
| `refuse_new` (2) | ❌ | ❌ `503 paused` | ✅ |
| `stopped` (3) | ❌ | ❌ `503 paused` | ❌ |

## Decision

**1. `dispatch_enabled` is NOT dropped, and NOT reinterpreted. It is composed as a severity FLOOR of
`accept_and_defer`.**

Rule 5 is additive-only: a seeded config key is not removed because a newer one covers the same
ground. That constraint is not what carries this decision, though — the boolean has **many live
consumers** (the dispatch loop, the reclaim sweep, the health snapshot, the campaign report), so
even the weaker "nobody uses this anymore" argument is unavailable. Dropping it would be a breaking
change with nothing on the other side of the ledger.

So it stays, and the composition is `max(severity(dispatch_mode), dispatch_enabled ? 0 : 1)`. This is
not a compromise jammed in to satisfy Rule 5 — it is *exactly what the boolean already meant*. False
already halted claiming and left intake and reclaim running, which is `accept_and_defer` under the
new name. No existing operator runbook step changes its effect, and no existing consumer of the
boolean changes its behaviour.

Note what the floor does NOT do: it never lowers a rung. `dispatch_enabled=true` with
`dispatch_mode=stopped` is `stopped`. Jamming the boolean to `true` cannot un-pull a deeper rung,
which is the failure a naive `enabled && mode` conjunction would have introduced.

**2. `dispatch_paused` is retained, and its meaning is widened to "the effective rung is not
`serving`".**

Rule 5 forbids removing it. Leaving it as the raw negated boolean was the alternative, and it is
worse than removal: it would report `false` — "sending normally" — while the Control is halted by
`dispatch_mode`. A field that reads normal during an outage is the D2 defect itself. Every input that
made it true before `send_managed_0025` still makes it true, so no client breaks; what changed is
that it stopped lying in the new case. Both derivation sites were fixed: `/v1/admin/health`'s
snapshot and the campaign-scoped `/v1/report` shape.

**3. The rung STRING is published on the operator surface only.**

`/v1/admin/health` gains `dispatch.dispatch_rung`. `GET /v1/report`'s campaign shape deliberately
does **not** — the platform does not administer this module's rung, and the boolean is the right
granularity for "is my campaign moving". Both surfaces report the *composed* rung, never the raw row.

**4. An unrecognized or unreadable mode resolves to `accept_and_defer` — never `serving`, never
`stopped`.**

`serving` would make a typo silently un-halt a jobs-must-not-flow situation. `stopped` would make a
typo more destructive than the deepest rung an operator can deliberately choose, and would also stop
the reclaim sweep as a side effect of a jsonb jumble. `accept_and_defer` is the shallowest rung that
still stops mail, and it loses nothing. The Control logs the degradation at `level=error`.

**5. The two surfaces fail in deliberately OPPOSITE directions, and that asymmetry is the decision,
not an inconsistency.**

- `POST /v1/run` fails **OPEN**: an unreadable/absent `dispatch_mode` accepts the submission.
  Refusing may lose customer work permanently; accepting one that then waits is recoverable.
- `GET /v1/admin/health` fails **CLOSED**: it returns `500` rather than a guessed rung. Its entire
  job is stating the system's condition, and an operator acts on what it says. Guessing
  `accept_and_defer` here would be indistinguishable on the dashboard from an operator having
  actually set that rung. (An unrecognized *string* is different from an unreadable *envelope*: the
  Control has a defined answer for the former, so reporting that answer is accurate rather than a
  guess. The two cases are tested separately for exactly this reason.)

**6. `503 paused` is a new published error code, not a reuse of `409 conflict`.**

A conflict is about *this request* and retrying it unchanged will keep failing. `paused` is about the
*module's condition* and the identical request succeeds once the rung is raised. `503` is the status
whose retry semantics are "later, unchanged". `codeStatus` now holds seven codes; `describe`'s
`error_codes[]` derives from that same map, and `contract_drift` checks the count against the
spelled-out literal in `api-contract.md` — which is how this ADR's own change was caught the moment
it landed. That literal moved **six → seven**; it had already been stale once before the gate learned
to read it.

**7. `/v1/quote` is never gated by the rung, at any depth, in combination with any value of
`dispatch_enabled`.**

A cost question must stay answerable during an incident; it reserves nothing, creates no job, and is
the step a platform-side preview/confirm flow depends on. Refusing it would break cost display for
work the module would happily accept one rung up.

**8. The intake gate sits AFTER field validation and BEFORE job creation.**

After validation, so a malformed body still gets `400 validation_failed` rather than having its own
bug masked by the pause — a caller must be able to fix their request during a halt. Before creation,
so a refusal is genuinely lossless rather than a row written and then rejected.

**9. Three copies of the ladder exist, and drift is prevented by a MECHANISM, not by a comment.**

`internal/dispatch`, `internal/platformapi`, and `internal/adminapi` each declare the rung constants
and the severity map, because OD-7's package isolation forbids the latter two importing the first
(the same reason each has its own config readers). A comment asking future editors to keep them in
sync would be the "a warning is not a control" failure this pass keeps finding. Instead:

- `internal/dispatch`'s `TestDispatchRung_EveryCopyOfTheLadderMatchesThisOne` compares the source
  text of every enumerated copy, **and** independently discovers ladder declarations under
  `internal/` and fails if it finds one that is not enumerated — so a FOURTH copy cannot appear
  silently. "The check covers less than the property it claims to guard" is itself one of the
  failure modes this pass catalogued.
- Each of `platformapi` and `adminapi` additionally carries a LOCAL
  `TestDispatchRung_LadderMatchesTheDispatchPackageExactly`, because a guard that only fires when you
  test a *different* package is a guard production discovers first.

The ORDERING is compared, not just the names. Both drift shapes compile, but a drifted ordering is
the worse one: "`refuse_new` and below" would silently mean two different things on two surfaces, and
the composed floor would land somewhere different on each.

## Consequences

- `send_managed_0025` adds one key (54 → 55). `config_drift` and `config_consumption` both check the
  new count; `Docs/config-manifest.txt`, `Docs/config-consumers.txt`, and `Docs/config-reference.md`
  moved in the same change, because `config_drift` compares all three against the live table.
- **This is the first config key added outside a numbered phase.** It lands in the Pass-2 alignment
  sweep, whose subject is closing gaps found across all three modules rather than delivering a
  phase. Recorded explicitly in `config-manifest.txt` so a later reader does not go looking for the
  phase document that would have justified it.
- Down-migration: `0025`'s `.down.sql` deletes exactly one row, and `dispatch_enabled` is not it.
  Nothing else needs reverting — the composition treats an absent `dispatch_mode` as unreadable and
  the boolean alone then governs, which is the pre-`0025` behaviour exactly. The migrate tool's
  integration test asserts both halves: that the down removes `dispatch_mode`, and that it does NOT
  sweep up the retained boolean (which would leave a reverted system with no dispatch kill-switch at
  all).
- `api-contract.md`'s spelled-out error-code count moved from **six** to **seven**, and the
  `contract_drift` gate is what forces that.
- Ported from module 3 (`coldmail-send-byo`, ADR-0017 there, `send_byo_0052`) in the same fix-plan
  item, on the same ladder with the same names and ordering. The modules do not share code, so each
  carries its own copy and its own drift mechanism.
- M1 (`coldmail-verify`) already published a `paused` code; its `internal/api/errors.go` comment
  calling that code "M1-ONLY" became false with this change and was corrected in the same item.

## Links

- CLAUDE.md Rule 5 (additive-only — why `dispatch_enabled` and `dispatch_paused` both survive),
  Rule 1 (config-not-code), Rule 3 (real infra in tests), Rule 7 (autonomous-subsystem safety
  controls — where both dials are now listed).
- `Docs/config-reference.md` — the "Fix-plan item 12 state" section (all four rungs, the FLOOR
  reasoning, the named consumers, which surfaces report it).
- `Docs/api-contract.md` — `503 paused` in the union enum, the `/v1/run` row, `dispatch_rung` and
  `dispatch_paused` in the health field table, `dispatch_paused` in the campaign report shape.
- `Docs/operator-runbook.md` §"Is dispatch switched off, and at WHICH rung?" — the rung table and
  which one to pull.
- `Docs/disaster-recovery.md` §2 (restore), §3 (key rotation), §4 (key compromise), §4a (worker
  compromise) — each names a specific rung and, where it matters, says which rung NOT to use.
- `db/migrations/send_managed_0025_dispatch_mode_ladder.{up,down}.sql`.
- `services/control/internal/dispatch/rung.go` (authoritative) + `rung_test.go`;
  `internal/platformapi/dispatch_rung.go` + `dispatch_rung_test.go`;
  `internal/adminapi/dispatch_rung.go` + `dispatch_rung_test.go`.
