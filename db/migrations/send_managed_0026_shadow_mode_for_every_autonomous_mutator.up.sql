-- send_managed_0026_shadow_mode_for_every_autonomous_mutator.up.sql — cross-module alignment
-- fix-plan item 13 (classification 13.3, canonical-module-contract.md §1.11.3).
--
-- WHAT THIS ADDS. Two string-enum config keys, each seeded at `shadow`:
--
--   autopause_mode   off | shadow | enforce   (seeded shadow)
--   retention_mode   off | shadow | enforce   (seeded shadow)
--
-- THE FINDING. M1 (coldmail-verify) has four independent autonomous mutators and EVERY one of them
-- defaults to `recommend_only`: it computes the decision, records the recommendation, and mutates
-- nothing until a human promotes the dial. This module retrofitted exactly ONE shadow dial
-- (`autoquarantine_mode`, send_managed_0021) and left its other autonomous writers ungated. The audit
-- that produced this migration walked every UPDATE/DELETE against send_managed.* outside the operator
-- surface and classified each one; the two above are the writers that both (a) act on their own,
-- without an operator or a caller asking, and (b) had no way to be observed before acting.
--
-- THE FULL AUDIT, INCLUDING WHAT IS DELIBERATELY *NOT* DIALLED. Recorded here because "we checked
-- everything" is worthless without the list, and because the exclusions are the part a later reader
-- will want to challenge:
--
--   GAINS A DIAL HERE
--   * auto-pause (dispatch/account_effect.go) — sets accounts.paused/pause_reason='throttle' on a
--     throttle outcome. Autonomous, and previously all-or-nothing via `autopause_enabled`.
--   * retention sweep (dispatch/retention.go) — DELETEs expired send_managed.send_logs rows. The only
--     unconditional, IRREVERSIBLE autonomous write in the module: it had no mode dial of any kind,
--     just an interval and a batch size, and a deleted row cannot be un-deleted by flipping a switch.
--
--   ALREADY DIALLED — no change
--   * auto-quarantine (dispatch/account_effect.go) — `autoquarantine_mode`, already off/shadow/enforce
--     seeded shadow. This item's target state, reached early; it is the pattern the two new dials copy.
--   * claim / push / reclaim / rescan / lease writes (dispatch/*.go, workerlink/stream.go) —
--     governed by `dispatch_mode`'s four-rung ladder (send_managed_0025, fix-plan item 12) plus
--     `dispatch_enabled`. A rung already stops each of them.
--
--   DELIBERATELY NOT DIALLED, with the reasoning
--   * score recompute (dispatch/account_score.go) — it writes accounts.score /
--     consecutive_failures, but a score is DERIVED STATE, not an action: it is the observation the
--     quarantine decision then reads. Shadowing it would mean "compute the number but store a stale
--     one", which does not preview anything — it just makes the dial above act on a lie. The action is
--     quarantine, and quarantine already has its ladder.
--   * auto-unpause (dispatch/eligibility.go) — self-clears a throttle pause once
--     throttle_pause_seconds has elapsed. It is the REVERSAL half of auto-pause, and shadowing a
--     reversal is actively harmful: it would leave real pauses in place forever, manufacturing the
--     stuck-account incident the observation period exists to avoid. A mutator whose only effect is
--     to undo an earlier mutation does not need an observation period; it needs to not be blocked.
--   * every write in internal/adminapi — operator-initiated, not autonomous. An operator asking for a
--     change does not need a mode dial to preview their own request.
--
-- THAT LIST IS NOT LOAD-BEARING PROSE. A comment claiming an audit was complete is exactly the
-- "warning instead of a mechanism" defect this alignment pass keeps finding, so the audit is also
-- enforced by a TEST:
--   dispatch.TestMutatorMode_EveryAutonomousMutatorIsDialledOrExplicitlyExcluded
-- (services/control/internal/dispatch/mutator_mode_test.go). It discovers every FILE under
-- internal/dispatch that writes to send_managed.* — matching UPDATE/DELETE FROM against the schema —
-- and requires each to be either enumerated as dialled, with its dial actually REFERENCED in that
-- file, or enumerated as excluded with a recorded reason. Both halves are checked, so a site LOSING
-- its gate fails there too rather than passing silently, and both maps are checked for stale entries.
-- A new autonomous mutator cannot ship enforcing on day one without that test going red naming its
-- file.
--
-- SCOPE OF THAT MECHANISM, STATED HONESTLY: it scans internal/dispatch only, NOT internal/workerlink.
-- workerlink's writes are lease/push bookkeeping already governed by dispatch_mode's rung ladder
-- (send_managed_0025) and it is listed under ALREADY DIALLED above — but that half rests on the audit
-- below rather than on the discovery test, and saying otherwise would be the same defect this
-- paragraph exists to avoid.
--
-- SEEDED `shadow`, WHICH CHANGES BEHAVIOUR ON UPGRADE, DELIBERATELY AND WITH A FLAG.
--
-- Item 13's instruction is to adopt M1's default wholesale, and M1's default is the safe one. So:
--   * autopause_mode=shadow — a throttle outcome no longer pauses the account; it writes a
--     would-pause observation row instead. Accounts keep sending through a throttle until an operator
--     promotes the dial.
--   * retention_mode=shadow — the sweep no longer deletes; it COUNTS what it would have deleted and
--     logs that count. send_managed.send_logs therefore GROWS WITHOUT BOUND until an operator
--     promotes the dial to `enforce`.
--
-- The second one is a real operational hazard, not a theoretical one, and it is registered as such in
-- Docs/go-live-checklist.md as a REQUIRED promotion step rather than left for someone to discover
-- from disk-usage alerts. It is called out in Docs/operator-runbook.md and Docs/config-reference.md in
-- the same words. An unpromoted safety dial that quietly costs you disk is a worse outcome than the
-- unconditional sweep it replaced, and pretending otherwise in a migration header would repeat this
-- pass's central failure: prose standing in for a mechanism.
--
-- HOW EACH NEW MODE COMPOSES WITH THE BOOLEAN IT JOINS — A CEILING, NOT A FLOOR.
--
-- `autopause_enabled` is RETAINED (Rule 5, and it has live consumers: account_effect.go's gate,
-- health, and the operator docs). It composes as a CEILING: `autopause_enabled=false` CAPS the
-- effective mode at `off`.
--
-- THIS IS THE OPPOSITE DIRECTION FROM send_managed_0025's FLOOR, AND THAT IS NOT AN INCONSISTENCY —
-- it is the same principle applied to the opposite kind of control. For a KILL-SWITCH (dispatch_mode)
-- `false` means "do LESS sending", so the boolean must be able to make the effective state MORE
-- severe: a floor. For a MUTATOR ENABLE (autopause_enabled) `false` means "take FEWER actions", so the
-- boolean must be able to make the effective state LESS active: a ceiling. In both cases the
-- composition rule is identical once stated properly — THE BOOLEAN CAN ONLY EVER REDUCE WHAT THE
-- SYSTEM DOES, NEVER INCREASE IT. A floor on a severity scale and a ceiling on an activity scale are
-- the same sentence. Getting this backwards is the bug worth guarding: a `false` boolean that let
-- `enforce` through would mean an operator who disabled auto-pause still got auto-pauses.
--
-- retention_mode joins no boolean; `retention_delete_batch_size` and `retention_sweep_interval_ms`
-- are rate controls, not enables, and are untouched here.
--
-- FAIL-SAFE ON AN UNRECOGNIZED VALUE: both resolve to `shadow`, never `enforce`, never `off`.
-- `enforce` would let a typo silently gain the power to act — the exact defect the ladder exists to
-- prevent. `off` would silently disable a control an operator believes is running, which is M1's D2
-- finding (a control that looks normal while being ignored). `shadow` is the only value that both
-- refuses to act and keeps reporting, so it is the only correct fallback. Logged at level=error.
-- This matches autoquarantine_mode's existing fallback exactly.
--
-- WHY value_type='string' AND NOT A pg ENUM: identical reasoning to autoquarantine_mode and
-- dispatch_mode. send_managed.config is a generic key/value table with value_type as the
-- discriminator; the admin PUT validator enforces the accepted set at write time; the loader fails
-- safe regardless. Minting a pg enum per config row puts a value set that may gain a rung behind a
-- DDL migration.
--
-- NO SCHEMA CHANGE. Two INSERTs into an existing table. No column, no index, no constraint, no grant
-- change. Nothing in send_managed.jobs or job_events is touched: these dials change what the Control
-- DOES, never what is recorded.

INSERT INTO send_managed.config (key, value, default_value, description, value_type) VALUES
('autopause_mode', '{"v":"shadow"}', '{"v":"shadow"}',
 'Shadow ladder for auto-pause, the throttle-response mutator (fix-plan item 13, contract §1.11.3).
One of off | shadow | enforce. enforce = the pre-0026 behaviour: a throttle outcome sets
accounts.paused with pause_reason=''throttle''. shadow (DEFAULT, launch-safety, adopted from module
1''s recommend_only) = the decision is made and recorded as a would-pause account_events observation
row, but the account is NOT paused and keeps sending. off = the decision is not evaluated at all and
no row is written. Composes with the RETAINED autopause_enabled boolean as a CEILING: false caps the
effective mode at off, so an operator who disabled auto-pause never gets an auto-pause. (Note the
direction: dispatch_mode composes with its boolean as a severity FLOOR, this one as an activity
CEILING — both are the same rule, that the boolean can only ever reduce what the system does.) An
unrecognized value falls back to shadow (level=error), never to enforce and never to off. Manual
operator pause/unpause is unaffected at every mode, and auto-unpause is NOT gated by this dial — it
is the reversal half, and blocking a reversal would leave real pauses in place forever.', 'string'),

('retention_mode', '{"v":"shadow"}', '{"v":"shadow"}',
 'Shadow ladder for the send_logs retention sweep (fix-plan item 13, contract §1.11.3). One of
off | shadow | enforce. enforce = the pre-0026 behaviour: expired send_managed.send_logs rows are
DELETEd in bounded batches. shadow (DEFAULT, launch-safety) = the sweep COUNTS the rows it would have
deleted and logs that count, deleting nothing. off = no sweep and no count. OPERATIONAL WARNING, and
the reason this key is on the go-live checklist: at the seeded default NOTHING IS EVER DELETED, so
send_managed.send_logs grows without bound until an operator promotes this to enforce. That promotion
is a REQUIRED go-live step, not optional. This dial governs ONLY send_logs (debug content); jobs and
job_events are never touched by the sweep at any mode, so no outcome record is ever at risk. An
unrecognized value falls back to shadow (level=error) — a typo must not gain the power to delete, and
must not silently disable a sweep an operator believes is running. retention_sweep_interval_ms and
retention_delete_batch_size are rate controls and are unaffected by this key.', 'string');

INSERT INTO send_managed.schema_migrations (version) VALUES ('0026');
