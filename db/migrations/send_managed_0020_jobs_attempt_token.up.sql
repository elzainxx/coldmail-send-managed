-- send_managed_0020_jobs_attempt_token.up.sql — Phase 8 CHUNK 1 (P7-OD-6, RULED-ADOPT 2026-07-26).
-- ONE additive column on the EXISTING send_managed.jobs table: the per-attempt identity the Control has
-- lacked on the report path. Additive-only (CLAUDE.md Rule 5): no existing column, constraint, enum,
-- index, or trigger is changed, so the locked jobs_attempt_count_check, jobs_lease_valid_check, the
-- job_state enum, and the state machine are all untouched.
--
-- THE HOLE THIS CLOSES (Docs/history/open-decisions.md P7-OD-6). The worker's outcome buffer deliberately
-- outlives its own attempt (box_outcome_buffer_ttl_sec 900s >> job_max_processing_time_sec 120s), so
-- a job whose lease expires can be reclaimed, requeued, and RE-DISPATCHED while an older worker still
-- holds and replays the superseded attempt's outcome. Phase 7's attribution guard rejects that replay
-- only when the reporting worker DIFFERS from assigned_box_id; a re-dispatch to the SAME worker was
-- indistinguishable from the live attempt's report and WAS applied — a stale SUCCESS could mark the
-- job sent while attempt 2 is mid-flight, and a stale FAILURE could requeue the job out from under a
-- live attempt (which then gets dispatched AGAIN → a real duplicate SMTP send). Distinct from the
-- accepted crash-after-250 window: that one is unavoidable, this one is fixable.
--
--   attempt_token : MONOTONIC, NON-SATURATING per-job attempt counter. The Control increments it in the
--                   SAME guarded assigned→sending UPDATE that dispatches an attempt (dispatch.go
--                   markSending, `attempt_token = attempt_token + 1 ... RETURNING attempt_token`), so
--                   the value is minted exactly once per attempt and is unforgeable by the worker. It is
--                   echoed on push_job.attempt_token, returned verbatim in report.attempt_token, and
--                   verified in OutcomeHandler.Handle: a report whose token does not match the job's
--                   CURRENT token is a stale replay from a superseded attempt — rejected, logged, and
--                   applied to NOTHING (no job-row write, no account scoring, no send_logs row).
--
-- WHY NOT attempt_count (explicitly rejected): Phase 5 made attempt_count SATURATE at max_attempts
-- (`LEAST(attempt_count + 1, max_attempts)` in both dispatch.go requeue and outcome.go
-- applyClassifiedFailure), and requeue is unbounded until pool exhaustion — so a long-lived job REUSES
-- the same attempt_count value across many attempts. It cannot be an identity. attempt_token has no
-- cap and no CHECK bounding it: BIGINT, incremented forever, deliberately allowed to exceed
-- max_attempts. That divergence is the whole point and is asserted behaviorally
-- (TestAttemptToken_MonotonicAndNonSaturating).
--
-- Grants (ADR-0008 trap): the column lands on send_managed.jobs, which send_managed_app already holds
-- SELECT/INSERT/UPDATE/DELETE on (send_managed_0012). Table-level DML covers every column, present and
-- future, so NO new GRANT is required and none is issued — and the schema-wide TestGrantsInvariant
-- stays green (an ADD COLUMN changes no table-class privilege). No new table, no new grant, no
-- privilege drift.
--
-- Population / rewrite note: `BIGINT NOT NULL DEFAULT 0` is metadata-only on Postgres 11+ (no table
-- rewrite, no full scan — deliberately NO added CHECK constraint, which WOULD require a validating
-- scan under ACCESS EXCLUSIVE for zero benefit: the column is monotonic by construction, written only
-- by the one `+ 1` in markSending). Existing rows read 0, which is the correct neutral start:
--   * a job that has never been dispatched has had no attempt, and
--   * an attempt already in flight ACROSS the migration was pushed WITHOUT a token, so its report
--     carries none — and the Handle guard rejects only a PROVABLE mismatch (both ids known and
--     different), so that one-time in-flight window degrades to exactly Phase 7 behavior (the
--     different-worker guard still applies) rather than rejecting a legitimate outcome. The very next
--     dispatch of that job mints token 1.
--
-- Lock ordering (CLAUDE.md Rule 9 / ADR-0010): this migration adds NO account/job locking path. The
-- one writer (markSending) already UPDATEs the job row and already changes `state` + assigned_account_id,
-- so it already fires the job_events trigger and already takes the account's implicit FK KEY SHARE;
-- adding one more column to that same SET list changes nothing about which locks it takes or in what
-- order. markSending takes no exclusive account lock, so Rule 9's precondition does not apply to it
-- (unchanged from Phase 3). The new Handle guard is a pure READ on the path that already read the job.

ALTER TABLE send_managed.jobs
    ADD COLUMN attempt_token BIGINT NOT NULL DEFAULT 0;  -- 0 = no attempt has been dispatched yet

COMMENT ON COLUMN send_managed.jobs.attempt_token IS
    'Monotonic, NON-saturating per-attempt identity (P7-OD-6). Incremented by the Control in the guarded assigned->sending UPDATE; echoed on push_job and returned in report; a mismatching report is a stale replay and is rejected without mutating state or scoring the account. Deliberately uncapped — unlike attempt_count, which saturates at max_attempts and therefore cannot identify an attempt.';

-- ---------------------------------------------------------------------------------------------
-- Phase 8 CHUNK 5 — content-variation config seeds (§6.1/§6.2, LOCKED — fourteen keys exactly).
--
-- FLAGGED DEVIATION (recorded, not silent): the locked spec names this seed migration
-- `send_managed_0020_content_variation_config`, but that version number was already spent by Chunk 1's
-- `send_managed_0020_jobs_attempt_token` above (the P7-OD-6 scope addition, ruled: "Phase 8 also
-- implements the P7-OD-6 per-attempt identity fix... because Phase 8 is the only remaining
-- pre-9.5 phase that already touches the push_job wire contract and owns migration 0020"). Rather
-- than open a second `send_managed_0020` (impossible — versions are unique) or renumber a committed,
-- self-tracking migration (forbidden — migrations are immutable once applied/committed, and
-- Docs/history/phase-9-polish-and-documentation.md §6.7/§6.8 + config-reference.md already cite
-- `send_managed_0020` as Phase 8's ONE migration), these fourteen INSERTs are appended to THIS SAME
-- file. The chain stays contiguous (...0019 -> 0020 -> 0021 for Phase 9.5) and every doc that
-- already says "Phase 8 = send_managed_0020" remains true.
--
-- SEVENTEEN KEYS (amended 2026-07-26 from fourteen — Docs/history/open-decisions.md P8-OD-1, RULED-WIDEN).
-- The build mandate asked to seed "the intensities/rates too" (a ZWJ rate, a CSS magnitude, a
-- UA-injection rate). This originally contradicted the LOCKED 14-key enumeration in this spec's
-- §6.1 and Phase 9 §4.5, so it was filed as P8-OD-1 (CLAUDE.md Rule 2) rather than resolved
-- unilaterally. The user RULED to widen: an on/off toggle whose strength is hardcoded is only half
-- a dial (config-not-code, Rule 1). Both locked specs were amended (additively — see
-- phase-8-content-variation.md's "Post-Chunk-5 amendment" section and phase-9-polish-and-
-- documentation.md §4.5) to add variation_css_magnitude_px / variation_zero_width_rate /
-- variation_user_agent_rate. The worker's former literal constants (ZWJ_RATE, the CSS +/-1px spread,
-- UA_INJECT_RATE) are REMOVED, not merely overridden, so there is exactly one source of truth.
--
-- Grants (ADR-0008 trap): all seventeen rows land in send_managed.config, which send_managed_app already
-- holds SELECT on (send_managed_0010). No new table, no new grant; the schema-wide TestGrantsInvariant
-- stays green (a config INSERT changes no table-class privilege).
INSERT INTO send_managed.config (key, value, default_value, description, value_type) VALUES
('variation_html_minify', '{"v":false}', '{"v":false}',
 'Phase 8 toggle 1: minify HTML body at worker-side MIME assembly (strip comments/whitespace; preserve Outlook conditionals). Off = Phase 3 body. Worker-consumed via push_job.variation.', 'boolean'),
('variation_comment_injection', '{"v":false}', '{"v":false}',
 'Phase 8 toggle 2: inject invisible HTML comments after a fraction of closing tags. Off = no comments. Worker-consumed.', 'boolean'),
('variation_comment_intensity', '{"v":"low"}', '{"v":"low"}',
 'Parameter of variation_comment_injection: low=5%/medium=15%/high=30% of closing tags. Ignored when the toggle is off.', 'string'),
('variation_css_micro', '{"v":false}', '{"v":false}',
 'Phase 8 toggle 3: vary inline padding/margin px by +/- variation_css_magnitude_px (skip <3px, never <0). Off = CSS untouched. Worker-consumed.', 'boolean'),
('variation_css_magnitude_px', '{"v":1}', '{"v":1}',
 'Parameter of variation_css_micro (amended 2026-07-26, P8-OD-1 RULED-WIDEN): max +/-px spread for the seeded CSS variation. Was a fixed +/-1 in code; promoted to a dial. Ignored when the toggle is off.', 'integer'),
('variation_unique_url_params', '{"v":false}', '{"v":false}',
 'Phase 8 toggle 4: append inert cache-busting _ref=/_v= param (seeded from job_id) to author links/images. NOT tracking: no callback, no redirect, no tracking domain. Off = URLs untouched. Worker-consumed.', 'boolean'),
('variation_zero_width', '{"v":false}', '{"v":false}',
 'Phase 8 toggle 5: insert U+200D (ZWJ) at a variation_zero_width_rate fraction of word boundaries in visible text. DEFAULT OFF (some filters flag zero-width abuse). Worker-consumed.', 'boolean'),
('variation_zero_width_rate', '{"v":0.05}', '{"v":0.05}',
 'Parameter of variation_zero_width (amended 2026-07-26, P8-OD-1 RULED-WIDEN): fraction (0-1) of word-boundary spaces that get a ZWJ. Was a fixed 0.05 in code; promoted to a dial. Ignored when the toggle is off.', 'number'),
('variation_encoding_rotation', '{"v":false}', '{"v":false}',
 'Phase 8 toggle 6: rotate body Content-Transfer-Encoding between quoted-printable and base64 (seeded). Decodes identically. Off = SMTP-library default (= Phase 3). Worker-consumed.', 'boolean'),
('variation_mime_boundary_rotation', '{"v":false}', '{"v":false}',
 'Phase 8 toggle 7: vary the MIME multipart boundary delimiter style (seeded). Structure identical. Off = library default (= Phase 3). Worker-consumed.', 'boolean'),
('variation_dynamic_preheader', '{"v":false}', '{"v":false}',
 'Phase 8 toggle 8: inject a hidden preheader span (seeded template) after <body>. V1 = no merge personalization (no contact data worker-side; tokens cleaned to empty). Off = none. Worker-consumed.', 'boolean'),
('variation_preheader_templates',
 '{"v":["A brief note for you","A quick read for you","Quick update for you","Worth a look","A quick note for you","Thought you might find this useful","Something relevant for you"]}',
 '{"v":["A brief note for you","A quick read for you","Quick update for you","Worth a look","A quick note for you","Thought you might find this useful","Something relevant for you"]}',
 'Parameter of variation_dynamic_preheader: candidate templates (V1 token-free per P8-7). Ignored when the toggle is off.', 'array<string>'),
('variation_header_style', '{"v":"none"}', '{"v":"none"}',
 'Phase 8 toggle 9: reorder headers to a client style (none/gmail/outlook/apple/thunderbird/random). Reorders only; changes no value. none = preserve Phase 3 order (the byte-identical identity). Worker-consumed.', 'string'),
('variation_user_agent_injection', '{"v":false}', '{"v":false}',
 'Phase 8 toggle 10: inject a seeded User-Agent header for a variation_user_agent_rate fraction of jobs. Decoupled from variation_header_style (P8-6). Off = no User-Agent. Worker-consumed.', 'boolean'),
('variation_user_agent_rate', '{"v":0.3}', '{"v":0.3}',
 'Parameter of variation_user_agent_injection (amended 2026-07-26, P8-OD-1 RULED-WIDEN): fraction (0-1) of jobs that get an injected User-Agent. Was a fixed 0.3 in code; promoted to a dial. Ignored when the toggle is off.', 'number'),
('variation_precedence', '{"v":"auto"}', '{"v":"auto"}',
 'Phase 8 toggle 11: Precedence header control (auto/marketing/personal). marketing = add Precedence: bulk; auto/personal = omit. auto = Phase 3 (omit). Worker-consumed.', 'string'),
('variation_thread_index', '{"v":false}', '{"v":false}',
 'Phase 8 toggle 12: Thread-Index header. INERT in V1 (no sequences => no thread to index); emits nothing whether on or off. Kept for a future sequences phase. Worker-consumed.', 'boolean');

INSERT INTO send_managed.schema_migrations (version) VALUES ('0020');
