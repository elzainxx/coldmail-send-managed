-- send_managed_0025_dispatch_mode_ladder.up.sql — cross-module alignment fix-plan item 12
-- (classification 13.1, module-comparison-report §7.4, canonical-module-contract.md §1.11.1).
--
-- WHAT THIS ADDS. One string-enum config key, `dispatch_mode`, seeded at `serving`:
--
--   serving | accept_and_defer | refuse_new | stopped
--
-- It is the graduated form of a control that was binary. `dispatch_enabled` halted every tenant's
-- claim path or none, so a partial incident (one provider degrading, a suspected bad config, a
-- shared pool misbehaving) had exactly one available response: stop everything.
--
-- THE SAME KEY, THE SAME FOUR RUNGS, THE SAME ORDERING AS MODULE 3's send_byo_0052. The two modules
-- share no code (they are separate repos and separate Go modules by design), so each carries its own
-- ladder and its own drift mechanism. What they DO share is the wire contract: `503 paused` means the
-- same thing on both, which is what lets a platform client hold one switch instead of two. Module 1
-- publishes the same code for its own Rung 4. See ADR-0013 in this repo and ADR-0017 in module 3.
--
-- SEEDED WITH ITS CONSUMER, IN THIS SAME CHANGE — never before. The consumer is
-- internal/dispatch/rung.go (resolveRung + the three predicates), read at two gate sites
-- (loop.go's Tick, dispatch.go's TryDispatch), one reclaim site (reclaim.go's ReclaimTick) and one
-- intake site (platformapi handlers.go's handleRun). Docs/config-consumers.txt records it with a grep
-- pattern, so the BLOCKING config_consumption gate proves the consumer exists on every CI run rather
-- than taking this header's word for it. A seeded key whose only evidence of use is a comment in the
-- migration that seeded it is the config-that-does-nothing defect this repo's gates exist to catch.
--
-- WHY `dispatch_enabled` IS NOT DROPPED, though this key supersedes its role.
--
-- Module 3's ADR-0016 set the two-condition test for dropping a seeded config row, and module 3's
-- ADR-0017 applied it to this exact key. Condition (1) — "the key never had a genuine consumer at ANY
-- point" — FAILS here just as plainly:
--
--   dispatch/loop.go:67 and dispatch/dispatch.go:248 both gate on it; adminapi/health_store.go
--   derives the §3.2 health snapshot's `dispatch_paused` from it; platformapi/campaign_report.go
--   reads it live for the campaign report's `dispatch_paused`; and Docs/disaster-recovery.md
--   instructs an operator to set it in more than one procedure. This is a live control with live
--   consumers, not dead inheritance.
--
-- Nothing is dropped, renamed, or reinterpreted. `dispatch_enabled` keeps its name, its default, its
-- type and its exact meaning.
--
-- SO HOW DO TWO DIALS GOVERN ONE CONTROL WITHOUT HALF-PULLING IT? They are not independent. The
-- effective rung is the MORE SEVERE of the two (rung.go's resolveRung), with `dispatch_enabled=false`
-- acting as a SEVERITY FLOOR of accept_and_defer.
--
-- That floor is not a compatibility shim — it is a restatement of what the boolean already did.
-- Verified against this tree, `dispatch_enabled=false` gates only the two CLAIM sites: intake keeps
-- accepting, reclaim/outcome/heartbeat keep running (its own throttled log line says so in those
-- words). That is accept-and-defer precisely. So every existing runbook step, DR step and test that
-- sets `dispatch_enabled=false` remains true to the letter, and no pair of values can leave a pulled
-- rung half-effective, because a maximum has no such state.
--
-- The alternative shape — two independent levers, one for intake and one for the drainer — is the
-- specific thing M1's ADR-0015 §1 identifies as "exactly how one half stays running." Avoided by
-- construction here, and asserted over the full 4x2 matrix by
-- TestDispatchRung_EnabledFalseIsAFloorNotASecondLever.
--
-- WHY value_type='string' AND NOT A pg ENUM. Same reasoning autoquarantine_mode already settled for
-- this table: send_managed.config is a generic key/value table whose value_type column is the
-- discriminator, the admin PUT validator enforces the accepted set at write time (config_validate.go,
-- alongside autoquarantine_mode's), and the Control-side loader fails safe on an unrecognized value
-- regardless. Minting a pg enum for one config row would put a value set that may gain a rung behind
-- a DDL migration, and Rule 5's additive-only discipline is easier to honour in the validator than in
-- a type.
--
-- NO SCHEMA CHANGE. One INSERT into an existing table. No column, no index, no constraint, no grant
-- change, and nothing in send_managed.jobs or job_events is touched — the ladder changes what the
-- Control DOES, never what is recorded (Rule 5: outcomes stay permanent by construction).

INSERT INTO send_managed.config (key, value, default_value, description, value_type) VALUES
('dispatch_mode', '{"v":"serving"}', '{"v":"serving"}',
 'Graduated dispatch kill-switch ladder (fix-plan item 12, contract §1.11.1). One of
serving | accept_and_defer | refuse_new | stopped, in increasing severity. serving = normal
operation. accept_and_defer = POST /v1/run still accepts and queues, but nothing is claimed or
pushed; reclaim, outcome ingestion and heartbeats continue. refuse_new = as accept_and_defer, and
POST /v1/run is refused 503 paused so no backlog accumulates; in-flight work still drains and is
still reclaimed. stopped = as refuse_new, and the reclaim sweep is frozen too, so an expired lease is
left in place rather than requeued (nothing is stranded: reclaim''s predicate is a DB read, so the
lease is still expired on the first tick after release). Outcome ingestion is NEVER gated at any rung
— an outcome is the record of a mail that has already left, and discarding it would destroy evidence
rather than prevent a send. Composes with dispatch_enabled as a MAXIMUM, never as a second
independent lever: dispatch_enabled=false is a severity floor of accept_and_defer, which is exactly
what that boolean already did. An unrecognized value falls back to accept_and_defer (level=error) —
no mail leaves under an unreadable policy, and one typo does not freeze the reclaim sweep.', 'string');

INSERT INTO send_managed.schema_migrations (version) VALUES ('0025');
