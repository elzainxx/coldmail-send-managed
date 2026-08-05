package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Real-infra integration test for the migration runner. Requires a real Postgres reachable
// via TEST_DATABASE_URL (an admin/owner-capable DSN) with public.users + public.job_items
// present. Skipped if TEST_DATABASE_URL is unset. NEVER mocks the DB (CLAUDE.md Rule 3).
func testDSN(t *testing.T) string {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-infra runner test")
	}
	return dsn
}

func mustExec(t *testing.T, c *pgx.Conn, sql string) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func schemaMigrationsExists(t *testing.T, c *pgx.Conn) bool {
	t.Helper()
	var ok bool
	err := c.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema='send_managed' AND table_name='schema_migrations')`).Scan(&ok)
	if err != nil {
		t.Fatalf("exists check: %v", err)
	}
	return ok
}

func versions(t *testing.T, c *pgx.Conn) []string {
	t.Helper()
	rows, err := c.Query(context.Background(), "SELECT version FROM send_managed.schema_migrations ORDER BY version")
	if err != nil {
		t.Fatalf("select versions: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

// TestRunnerLifecycle proves apply → idempotent re-run → down on ONE connection, so there
// is no cross-process timing ambiguity: the exact same session observes each state change.
func TestRunnerLifecycle(t *testing.T) {
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Admin connection used only to reset to a known-clean state and to observe truth.
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close(context.Background())

	// Clean slate on THIS connection. Drop only the SCHEMA (per-database); the cluster-global
	// roles send_managed_owner/send_managed_app are shared across databases (platform, template, this
	// one) and the bootstrap creates them idempotently (IF NOT EXISTS), so we do NOT drop
	// them here — dropping a role that still owns objects in another DB fails (2BP01). The
	// bootstrap's ownership/grant statements are idempotent against pre-existing roles.
	mustExec(t, admin, "DROP SCHEMA IF EXISTS send_managed CASCADE")

	// ANTI-VACUITY: assert the precondition. If the schema already existed here, the test
	// below would be meaningless — so prove it's genuinely gone before we apply.
	if schemaMigrationsExists(t, admin) {
		t.Fatal("precondition failed: send_managed.schema_migrations still exists after DROP — test would be vacuous")
	}

	r := &Runner{
		DSN:              dsn,
		Dir:              migrationsDirForTest(),
		PreflightPath:    preflightPathForTest(),
		LockTimeout:      "10s",
		StatementTimeout: "60s",
		AllowDown:        true,
	}

	// The full feature chain: bootstrap 0000, the locked Phase-1 data model send_managed_0001..0012,
	// then Phase 2's additive migrations send_managed_0013 (per-pool account uniqueness index) and
	// send_managed_0014 (the send_managed.account_events audit sink). This is the executable expectation of
	// the migration chain — it grows as later phases add 0015+.
	wantChain := []string{"0000", "0001", "0002", "0003", "0004", "0005", "0006", "0007",
		"0008", "0009", "0010", "0011", "0012", "0013", "0014", "0015", "0016", "0017", "0018", "0019",
		"0020", "0021", "0022", "0023", "0024", "0025", "0026"}

	// --- Apply ---
	if err := r.Up(ctx); err != nil {
		t.Fatalf("Up: %v", err)
	}
	if !schemaMigrationsExists(t, admin) {
		t.Fatal("after Up: send_managed.schema_migrations does not exist")
	}
	if got := versions(t, admin); !equalStrings(got, wantChain) {
		t.Fatalf("after Up: versions = %v, want %v", got, wantChain)
	}
	// Prove the real objects/roles/grants exist (not just the tracking rows). The roles,
	// schema ownership, and platform grants are established by the bootstrap (0000).
	assertRole(t, admin, "send_managed_owner", false /*login*/, false /*super*/)
	assertRole(t, admin, "send_managed_app", true /*login*/, false /*super*/)
	assertSchemaOwner(t, admin, "send_managed", "send_managed_owner")
	assertPlatformGrant(t, admin, "users")
	assertPlatformGrant(t, admin, "job_items")
	// Prove the Phase-1 feature objects exist and are owned by send_managed_owner (so FORCE RLS on
	// send_managed.accounts genuinely enforces — ADR-0004).
	for _, tbl := range []string{"pools", "accounts", "boxes", "jobs", "job_events", "send_logs", "config", "account_events"} {
		assertTableOwner(t, admin, "send_managed", tbl, "send_managed_owner")
	}
	// Config seeds the 14 foundation keys (send_managed_0010) + Phase 5's 3 (send_managed_0017) + Phase 6's 2
	// (send_managed_0018: reclaim_tick_interval_ms, smtp_operation_timeout_sec) + Phase 7's 6 (send_managed_0019:
	// startup_reconnect_window_sec, shutdown_drain_timeout_sec, retention_sweep_interval_ms,
	// retention_delete_batch_size, rollup_granularity, box_outcome_buffer_ttl_sec) + Phase 8's 17
	// (send_managed_0020, appended to the SAME version file as Chunk 1's attempt_token column — flagged
	// migration-filename deviation, Docs/history/open-decisions.md P8-OD-1; amended 2026-07-26 from 14 to 17,
	// P8-OD-1 RULED-WIDEN) + Phase 9.5's 11 (send_managed_0021: B1-B5 operability controls + S10/S11
	// completeness dials) + G11-OD-1's 1 (send_managed_0023: brain_singleton_reacquire_interval_sec) = 54.
	// PLUS ONE at send_managed_0025 (dispatch_mode, fix-plan item 12 — the graduated dispatch kill-switch
	// ladder; the first key added outside a numbered phase, and notably NOT paired with a drop of
	// dispatch_enabled: ADR-0013 records why the boolean is composed as a severity FLOOR instead of
	// removed, since it has many live consumers) = 55.
	// PLUS TWO at send_managed_0026 (autopause_mode, retention_mode, fix-plan item 13 — the same
	// off|shadow|enforce ladder extended to the two remaining undialled autonomous mutators; ADR-0014) = 57.
	// Phase 4 added NO config key (§0); Phase 7, Phase 9.5, send_managed_0023, send_managed_0025 and
	// send_managed_0026 are fully schema-neutral (config seeds only, no table).
	if n := configKeyCount(t, admin); n != 57 {
		t.Fatalf("after Up: send_managed.config has %d keys, want 57 (14 foundation + 3 Phase 5 + 2 Phase 6 + 6 Phase 7 + 17 Phase 8 + 11 Phase 9.5 + 1 send_managed_0023 + 1 send_managed_0025 + 2 send_managed_0026)", n)
	}
	// Phase 8 Chunk 5's seventeen content-variation config seeds — anti-vacuity that they landed
	// (amended from fourteen, P8-OD-1 RULED-WIDEN: css_magnitude_px/zero_width_rate/user_agent_rate).
	for _, key := range []string{
		"variation_html_minify", "variation_comment_injection", "variation_comment_intensity",
		"variation_css_micro", "variation_css_magnitude_px", "variation_unique_url_params",
		"variation_zero_width", "variation_zero_width_rate",
		"variation_encoding_rotation", "variation_mime_boundary_rotation", "variation_dynamic_preheader",
		"variation_preheader_templates", "variation_header_style", "variation_user_agent_injection",
		"variation_user_agent_rate", "variation_precedence", "variation_thread_index",
	} {
		if !configKeyExists(t, admin, key) {
			t.Fatalf("after Up: send_managed.config missing %q (send_managed_0020 Phase 8 seed not applied)", key)
		}
	}
	// Phase 4's ONE additive schema delta: the per-user claim index (send_managed_0015, CONCURRENTLY).
	// Proves the runner APPLIED a CONCURRENTLY migration (not a no-op) — anti-vacuity for the
	// non-transactional path fix.
	if !indexExists(t, admin, "jobs_claim_by_user_idx") {
		t.Fatal("after Up: send_managed.jobs_claim_by_user_idx missing (send_managed_0015 CONCURRENTLY not applied)")
	}
	// Phase 5's additive columns (send_managed_0016). Anti-vacuity that the ADD COLUMN migration ran.
	if !columnExists(t, admin, "accounts", "consecutive_failures") {
		t.Fatal("after Up: send_managed.accounts.consecutive_failures missing (send_managed_0016 not applied)")
	}
	if !columnExists(t, admin, "accounts", "throttle_paused_at") {
		t.Fatal("after Up: send_managed.accounts.throttle_paused_at missing (send_managed_0016 not applied)")
	}
	// Phase 8 Chunk 1's additive column (send_managed_0020): the per-attempt identity that lets the Control
	// reject a stale replay from a superseded attempt (P7-OD-6).
	if !columnExists(t, admin, "jobs", "attempt_token") {
		t.Fatal("after Up: send_managed.jobs.attempt_token missing (send_managed_0020 not applied)")
	}
	// Phase 9.5's eleven operability + completeness config seeds (send_managed_0021) — anti-vacuity.
	for _, key := range []string{
		"dispatch_enabled", "autoquarantine_mode", "autopause_enabled", "variation_enabled",
		"alert_webhook_url", "health_metrics_window_sec", "brain_singleton_acquire_timeout_sec",
		"unit_price_credits", "variation_comment_intensity_rates", "variation_zero_width_rate_pct",
		"variation_css_micro_params",
	} {
		if !configKeyExists(t, admin, key) {
			t.Fatalf("after Up: send_managed.config missing %q (send_managed_0021 Phase 9.5 seed not applied)", key)
		}
	}
	// send_managed_0022's shared error-transition predicate — the single definition both job_events
	// triggers gate their error context on. Anti-vacuity that the migration ran: it is a
	// CREATE OR REPLACE of two existing function BODIES, so no column/index/config check can see
	// it; the one NEW object it adds is this function.
	if !functionExists(t, admin, "fn_job_event_is_error_transition") {
		t.Fatal("after Up: send_managed.fn_job_event_is_error_transition missing (send_managed_0022 not applied)")
	}
	// ...and that the replaced bodies actually GATE on it, which is the behavior 0022 exists for.
	// The predicate is IMMUTABLE SQL, so it can be evaluated directly: a success transition
	// (sending -> sent) is not an error, a requeue (assigned -> queued) is. Checking the predicate
	// rather than driving a job keeps this a migration test; the end-to-end "a SENT row carries no
	// error_class" assertion lives in the behavioral harness.
	if isErrorTransition(t, admin, "sending", "sent") {
		t.Fatal("after Up: fn_job_event_is_error_transition('sending','sent') is TRUE — a success would be tagged as an error (send_managed_0022 predicate wrong)")
	}
	if !isErrorTransition(t, admin, "assigned", "queued") {
		t.Fatal("after Up: fn_job_event_is_error_transition('assigned','queued') is FALSE — a requeue would lose its error context (send_managed_0022 predicate wrong)")
	}
	// send_managed_0023 (G11-OD-1 / ADR-0011 option B): the mid-life singleton re-acquire cadence. Its ONLY
	// object is one send_managed.config row, so existence alone would be a weak check — assert the seeded
	// value AND default_value, because Rule 1 requires every key to ship a documented default_value
	// and the whole point of the key is that the cadence is configured, not hardcoded. A seed that
	// landed with a NULL/absent default would satisfy configKeyExists and still violate Rule 1.
	if !configKeyExists(t, admin, "brain_singleton_reacquire_interval_sec") {
		t.Fatal("after Up: send_managed.config missing \"brain_singleton_reacquire_interval_sec\" (send_managed_0023 seed not applied)")
	}
	if v, d := configValueAndDefault(t, admin, "brain_singleton_reacquire_interval_sec"); v != `{"v": 5}` || d != `{"v": 5}` {
		t.Fatalf("after Up: brain_singleton_reacquire_interval_sec value=%s default_value=%s, want both {\"v\": 5} (send_managed_0023 seeded the wrong shape — the reader in main.go unwraps {\"v\": ...} and a bare 5 would silently fall back to 0 = never re-acquire)", v, d)
	}
	// send_managed_0024 (G12-OD-1 / ADR-0012 option A): job_events' append-only trigger gains the narrow
	// per-nullable-FK-column carve-out, so a parent DELETE's own `SET <col> = NULL` maintenance update
	// is permitted and the documented hard delete works again. This migration replaces ONE function
	// BODY and adds no object, so it is invisible to every existence check above — it has to be DRIVEN.
	// PAIRED, per Rule 10: the permitted half alone would report "landed" for a tree with the trigger
	// DROPPED, which is a strictly worse bug (a mutable audit log) than the wedge 0024 fixes.
	if !jobEventsCarveoutActive(t, admin) {
		t.Fatal("after Up: an ON DELETE SET NULL-shaped UPDATE on send_managed.job_events (account_id -> NULL) was REJECTED — send_managed_0024's carve-out did not land, so DELETE /v1/admin/accounts/{id} is still wedged for any account with job history (ADR-0012)")
	}
	if !jobEventsContentUpdateRejected(t, admin) {
		t.Fatal("after Up: a CONTENT update on send_managed.job_events (actor -> 'admin') was PERMITTED — send_managed_0024's carve-out is too wide, or the append-only trigger is gone entirely; the audit log is mutable")
	}
	// send_managed_0025's one seed (fix-plan item 12, the graduated dispatch kill-switch ladder). A pure
	// INSERT into config leaves no schema object behind, so the row itself is the only observable
	// evidence the migration ran — and the evidence here is PRESENCE plus the seeded VALUE. The value
	// matters more than the key's existence: seeding any rung other than `serving` would ship a system
	// that starts up halted, and seeding a value outside the four-rung enum would make the Control
	// degrade to accept_and_defer on its first tick forever. Both failures are silent (no error, mail
	// simply does not move), which is exactly the class this test exists to catch. Both value and
	// default_value are pinned: an operator PUT moves `value`, and `default_value` is what a future
	// reseed or a config-reference cross-check reads, so a migration that seeded only one of them
	// correctly would leave the other as the lie.
	if val, def := configValueAndDefault(t, admin, "dispatch_mode"); val != `{"v": "serving"}` || def != `{"v": "serving"}` {
		t.Fatalf("after Up: dispatch_mode value=%s default_value=%s, want both {\"v\": \"serving\"} (send_managed_0025 either did not run or seeded a rung that starts the system halted)", val, def)
	}
	// ...and the companion that 0025 did NOT drop dispatch_enabled. ADR-0013 records why: the boolean has
	// many live consumers (the dispatch loop, the health snapshot, the campaign report), so it is
	// RETAINED and composed as a severity floor of accept_and_defer rather than replaced. A future edit
	// that "tidies up" the superseded-looking boolean would break Rule 5 and every existing runbook step
	// that names it; it fails here.
	if !configKeyExists(t, admin, "dispatch_enabled") {
		t.Fatal("after Up: send_managed.config has lost dispatch_enabled — 0025 adds the rung ladder and must RETAIN the boolean (Rule 5; ADR-0013: it is composed as a floor, not replaced, because it has live consumers)")
	}
	// send_managed_0026's two seeds (fix-plan item 13, ADR-0014: the off|shadow|enforce ladder extended to
	// the last two undialled autonomous mutators). This assertion is the one place in the tree where the
	// seeded SAFETY DEFAULT is checked with no test harness in the picture, and that is why it is load-
	// bearing rather than decorative: the behavioral harness (newDispatchEnv) pins both dials to "enforce"
	// so the Phase-5-era and Phase-7 suites keep asserting real enforcing mechanics, which means a
	// migration that seeded "enforce" instead of "shadow" would be INVISIBLE to every test in
	// internal/dispatch except the one that reads default_value. Here `value` is read against the raw
	// chain, so both halves of the safety claim are pinned by something the harness cannot reach.
	//
	// Seeding "enforce" is the failure that matters, and it is silent in the worst possible direction:
	// retention_mode=enforce on a fresh migration means the sweep starts DELETING send_logs on its first
	// tick, with no observation period, in a deployment whose operator has not yet been told the dial
	// exists. Seeding "off" is the other silent failure — a control an operator believes is running while
	// nothing is evaluated at all. Both must fail here.
	for _, key := range []string{"autopause_mode", "retention_mode"} {
		if !configKeyExists(t, admin, key) {
			t.Fatalf("after Up: send_managed.config missing %q (send_managed_0026 seed not applied)", key)
		}
		if val, def := configValueAndDefault(t, admin, key); val != `{"v": "shadow"}` || def != `{"v": "shadow"}` {
			t.Fatalf("after Up: %s value=%s default_value=%s, want both {\"v\": \"shadow\"} — a mutator that starts ENFORCING on a fresh migration has no observation period, and for retention_mode that means deleting send_logs on the first tick (ADR-0014)", key, val, def)
		}
		// value_type is not cosmetic: the admin PUT validator dispatches on it, so a row typed anything
		// other than "string" would take the wrong validation path and could accept a value the ladder
		// cannot read — which then resolves to shadow forever while the operator believes they promoted it.
		if vt := configValueType(t, admin, key); vt != "string" {
			t.Fatalf("after Up: %s value_type=%q, want \"string\" (the admin config validator dispatches on value_type; a mistyped row validates down the wrong path)", key, vt)
		}
	}
	// ...and the companion, exactly as with 0025/dispatch_enabled: 0026 must RETAIN autopause_enabled.
	// It is composed as a severity CEILING of autopause_mode (false caps the effective mode at "off"),
	// which is what the boolean already meant, and it has live consumers (the health snapshot, named
	// runbook steps). An over-tidy migration that dropped the "superseded-looking" boolean would break
	// Rule 5 and silence every runbook step that names it.
	if !configKeyExists(t, admin, "autopause_enabled") {
		t.Fatal("after Up: send_managed.config has lost autopause_enabled — 0026 adds the mode ladder and must RETAIN the boolean (Rule 5; ADR-0014: it is composed as a CEILING, not replaced)")
	}
	// The two retention RATE controls are a different axis from the new MODE dial and 0026 must not touch
	// them. Folding rate into mode would silently change sweep throughput on upgrade for anyone who had
	// tuned them, on top of the mode change that upgrade already makes.
	for _, key := range []string{"retention_sweep_interval_ms", "retention_delete_batch_size"} {
		if !configKeyExists(t, admin, key) {
			t.Fatalf("after Up: send_managed.config has lost %q — 0026 adds retention_mode and must leave Phase 7's retention RATE controls alone (they are a separate axis from the mode dial)", key)
		}
	}
	// FORCE RLS is set on the crown-jewel credential table.
	if !accountsForceRLS(t, admin) {
		t.Fatal("after Up: send_managed.accounts does not have FORCE ROW LEVEL SECURITY")
	}

	// --- Idempotent re-run: applying again changes nothing, still exactly the full chain ---
	if err := r.Up(ctx); err != nil {
		t.Fatalf("Up (2nd): %v", err)
	}
	if got := versions(t, admin); !equalStrings(got, wantChain) {
		t.Fatalf("after idempotent Up: versions = %v, want %v", got, wantChain)
	}

	// --- Down: reverse the feature chain in strict reverse order (0015 -> 0001), proving each
	// migration's DOWN is real, then finally the bootstrap 0000 which drops the schema. The first
	// step (0015) exercises the runner's non-transactional DROP INDEX CONCURRENTLY down path. ---
	for i := len(wantChain) - 1; i >= 1; i-- {
		v := wantChain[i]
		if err := r.Down(ctx, v); err != nil {
			t.Fatalf("Down %s: %v", v, err)
		}
		if v == "0015" && indexExists(t, admin, "jobs_claim_by_user_idx") {
			t.Fatal("after Down 0015: jobs_claim_by_user_idx still present (CONCURRENTLY down did not drop it)")
		}
		if v == "0016" && (columnExists(t, admin, "accounts", "consecutive_failures") ||
			columnExists(t, admin, "accounts", "throttle_paused_at")) {
			t.Fatal("after Down 0016: a Phase 5 additive column is still present (down did not drop it)")
		}
		if v == "0018" && (configKeyExists(t, admin, "reclaim_tick_interval_ms") ||
			configKeyExists(t, admin, "smtp_operation_timeout_sec")) {
			t.Fatal("after Down 0018: a Phase 6 config seed is still present (down did not delete it)")
		}
		if v == "0019" && (configKeyExists(t, admin, "startup_reconnect_window_sec") ||
			configKeyExists(t, admin, "box_outcome_buffer_ttl_sec")) {
			t.Fatal("after Down 0019: a Phase 7 config seed is still present (down did not delete it)")
		}
		if v == "0020" && columnExists(t, admin, "jobs", "attempt_token") {
			t.Fatal("after Down 0020: send_managed.jobs.attempt_token is still present (down did not drop it)")
		}
		if v == "0020" && configKeyExists(t, admin, "variation_html_minify") {
			t.Fatal("after Down 0020: a Phase 8 content-variation config seed is still present (down did not delete it)")
		}
		if v == "0021" && configKeyExists(t, admin, "dispatch_enabled") {
			t.Fatal("after Down 0021: a Phase 9.5 operability config seed is still present (down did not delete it)")
		}
		if v == "0022" && functionExists(t, admin, "fn_job_event_is_error_transition") {
			t.Fatal("after Down 0022: send_managed.fn_job_event_is_error_transition is still present (down did not drop it)")
		}
		if v == "0023" && configKeyExists(t, admin, "brain_singleton_reacquire_interval_sec") {
			t.Fatal("after Down 0023: brain_singleton_reacquire_interval_sec is still present (down did not delete the seed)")
		}
		// 0024's down restores send_managed_0008's UNCONDITIONAL body, deliberately reinstating the wedge —
		// that is what "reversible" means here. Driving the probe is the only way to see it, and this
		// assertion is what proves the down is real rather than a version-row deletion.
		if v == "0024" && jobEventsCarveoutActive(t, admin) {
			t.Fatal("after Down 0024: an account_id -> NULL update on send_managed.job_events is STILL permitted (down did not restore send_managed_0008's unconditional reject-trigger)")
		}
		// The paired half of 0025's up-assertion. A seed migration reverses by DELETing its row, and
		// nothing else about it is observable, so without this a 0025 down that only removed its own
		// schema_migrations entry would pass every other check in this loop — leaving a key in the live
		// config table that the manifest (post-revert) no longer lists, which is precisely the state
		// config_drift exists to catch and would then catch on a clean restore instead of here.
		//
		// The over-delete half matters too, and for a sharper reason than usual: dispatch_enabled sits
		// right next to this row conceptually, is the thing 0025 supersedes in spirit, and is EXACTLY
		// what an over-broad down file would sweep up ("remove the dispatch switches this migration
		// introduced"). Deleting it would leave a system with no kill-switch at all after a revert.
		if v == "0025" {
			if configKeyExists(t, admin, "dispatch_mode") {
				t.Fatal("after Down 0025: send_managed.config still has dispatch_mode (down did not delete its seeded row)")
			}
			if !configKeyExists(t, admin, "dispatch_enabled") {
				t.Fatal("after Down 0025: dispatch_enabled is GONE — 0025's down must remove exactly its own row; sweeping up the retained boolean leaves a reverted system with no dispatch kill-switch at all")
			}
		}
		// 0026's down, same shape and the same two halves. Note the DIRECTION this revert moves safety in,
		// because it is the unusual one and ADR-0014 records it deliberately: deleting autopause_mode
		// RESTORES enforcing auto-pause (with autopause_enabled alone governing again, which is exactly the
		// pre-0026 behaviour), and deleting retention_mode restores enforcing deletion. A revert here is the
		// LESS safe direction, not the more safe one — which is precisely why the down must delete its rows
		// and nothing else, and must be run together with a source revert. Run standalone against unreverted
		// source it leaves a Control that will not boot at all, since LoadConfig treats an absent row as a
		// hard failure rather than a default; that is the intended loud failure, not a defect.
		if v == "0026" {
			for _, key := range []string{"autopause_mode", "retention_mode"} {
				if configKeyExists(t, admin, key) {
					t.Fatalf("after Down 0026: send_managed.config still has %q (down did not delete its seeded row)", key)
				}
			}
			// The over-delete half. autopause_enabled and the two retention rate keys are what an
			// over-broad down file would sweep up ("remove the pause/retention knobs this migration
			// introduced") — none of the three is 0026's to delete, and losing autopause_enabled on a
			// revert leaves a reverted system with no auto-pause switch at all.
			for _, key := range []string{"autopause_enabled", "retention_sweep_interval_ms", "retention_delete_batch_size"} {
				if !configKeyExists(t, admin, key) {
					t.Fatalf("after Down 0026: %q is GONE — 0026's down must remove exactly its own two rows; sweeping up a retained boolean or a Phase 7 rate control breaks a reverted system", key)
				}
			}
			// And autoquarantine_mode, which is on the SAME ladder but predates 0026 (send_managed_0021).
			// A down written by pattern-matching on "the mode dials" rather than on this migration's own
			// INSERTs would take it, silently removing the one autonomous-mutator control that shipped in
			// Phase 9.5 — and it would look correct, because the row it deleted has the same shape.
			if !configKeyExists(t, admin, "autoquarantine_mode") {
				t.Fatal("after Down 0026: autoquarantine_mode is GONE — it is on the same ladder but belongs to send_managed_0021; 0026's down must delete its own two rows and no other mode dial")
			}
		}
	}
	// After reversing 0001..0012 the schema is still present (owned by the bootstrap) with
	// only version 0000 recorded — proving the feature-chain downs un-record without dropping
	// the bootstrap's schema/roles.
	if got := versions(t, admin); len(got) != 1 || got[0] != "0000" {
		t.Fatalf("after feature-chain down: versions = %v, want [0000]", got)
	}
	// Finally reverse the bootstrap itself: schema gone.
	if err := r.Down(ctx, "0000"); err != nil {
		t.Fatalf("Down 0000: %v", err)
	}
	if schemaMigrationsExists(t, admin) {
		t.Fatal("after Down 0000: send_managed schema still present")
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func assertTableOwner(t *testing.T, c *pgx.Conn, schema, table, owner string) {
	t.Helper()
	var got string
	err := c.QueryRow(context.Background(),
		`SELECT tableowner FROM pg_tables WHERE schemaname=$1 AND tablename=$2`, schema, table).Scan(&got)
	if err != nil {
		t.Fatalf("owner lookup %s.%s: %v", schema, table, err)
	}
	if got != owner {
		t.Fatalf("table %s.%s owner = %s, want %s", schema, table, got, owner)
	}
}

func configKeyCount(t *testing.T, c *pgx.Conn) int {
	t.Helper()
	var n int
	if err := c.QueryRow(context.Background(), "SELECT count(*) FROM send_managed.config").Scan(&n); err != nil {
		t.Fatalf("config count: %v", err)
	}
	return n
}

// columnExists reports whether send_managed.<table> has a column of the given name.
func columnExists(t *testing.T, c *pgx.Conn, table, column string) bool {
	t.Helper()
	var ok bool
	if err := c.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM information_schema.columns
		   WHERE table_schema='send_managed' AND table_name=$1 AND column_name=$2)`, table, column).Scan(&ok); err != nil {
		t.Fatalf("column exists lookup %q.%q: %v", table, column, err)
	}
	return ok
}

// configKeyExists reports whether a send_managed.config row with the given key is present (for the
// 0018-down anti-vacuity check: a Phase 6 seed must be gone after its down migration).
func configKeyExists(t *testing.T, c *pgx.Conn, key string) bool {
	t.Helper()
	var ok bool
	if err := c.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM send_managed.config WHERE key=$1)`, key).Scan(&ok); err != nil {
		t.Fatalf("config key exists lookup %q: %v", key, err)
	}
	return ok
}

// configValueAndDefault returns a config row's value and default_value as text. Used by the
// send_managed_0023 anti-vacuity check: that migration's only object is a config row, so proving it
// "landed" means proving the JSONB {"v": ...} wrapper and the Rule-1 default_value are both right —
// not merely that a row with the key exists.
func configValueAndDefault(t *testing.T, c *pgx.Conn, key string) (string, string) {
	t.Helper()
	var value, def string
	if err := c.QueryRow(context.Background(),
		`SELECT value::text, default_value::text FROM send_managed.config WHERE key=$1`, key).Scan(&value, &def); err != nil {
		t.Fatalf("config value lookup %q: %v", key, err)
	}
	return value, def
}

// configValueType returns a config row's value_type. The admin PUT validator dispatches on this
// column, so a seed with the wrong value_type validates incoming writes down the wrong path even
// though its value and default_value read correctly.
func configValueType(t *testing.T, c *pgx.Conn, key string) string {
	t.Helper()
	var vt string
	if err := c.QueryRow(context.Background(),
		`SELECT value_type FROM send_managed.config WHERE key=$1`, key).Scan(&vt); err != nil {
		t.Fatalf("config value_type lookup %q: %v", key, err)
	}
	return vt
}

// indexExists reports whether an index of the given name exists in the send_managed schema.
func indexExists(t *testing.T, c *pgx.Conn, name string) bool {
	t.Helper()
	var ok bool
	if err := c.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM pg_indexes WHERE schemaname='send_managed' AND indexname=$1)`, name).Scan(&ok); err != nil {
		t.Fatalf("index exists lookup %q: %v", name, err)
	}
	return ok
}

// functionExists reports whether a function of the given name exists in the send_managed schema (for
// send_managed_0022, whose only NEW object is a function — its other two changes are CREATE OR REPLACE
// of bodies that already existed).
func functionExists(t *testing.T, c *pgx.Conn, name string) bool {
	t.Helper()
	var ok bool
	if err := c.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid=p.pronamespace
		   WHERE n.nspname='send_managed' AND p.proname=$1)`, name).Scan(&ok); err != nil {
		t.Fatalf("function exists lookup %q: %v", name, err)
	}
	return ok
}

// jobEventsCarveoutActive reports whether send_managed_0024's FK-aware carve-out is in force, by DRIVING
// the trigger rather than by reading its source. send_managed_0024 replaces one function BODY and adds no
// object at all, so functionExists/columnExists/configKeyExists are all blind to it and a
// pg_get_functiondef text match would assert the migration's own SQL against itself (it would pass
// against a carve-out that was syntactically present but logically wrong).
//
// So: insert a throwaway job_events row with account_id set, attempt the exact mutation the FK's
// ON DELETE SET NULL maintenance statement performs, and report whether it was permitted. Wrapped in
// a transaction that is always ROLLED BACK, so the lifecycle DB is unchanged either way. Runs as the
// admin/superuser connection deliberately — send_managed_app has UPDATE revoked (send_managed_0008), so a
// rejection seen as that role would prove the REVOKE, not the trigger.
func jobEventsCarveoutActive(t *testing.T, c *pgx.Conn) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatalf("begin carve-out probe: %v", err)
	}
	defer tx.Rollback(ctx)

	// The FK chain job_events requires: a users row, a pool, an account, a job_item and a job. The
	// probe event row is INSERTed directly (not taken from the genesis trigger) because it needs
	// account_id already NON-NULL — the genesis row is born with it NULL, the job not yet being
	// assigned — and job_events rows cannot be updated into shape, which is the whole point.
	var userID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO public.users (email, is_active) VALUES ('carveout-probe@sender.local', true) RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("carve-out probe: seed user: %v", err)
	}
	var poolID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO send_managed.pools (user_id, kind, name) VALUES ($1,'shared','carveout-probe') RETURNING id`,
		userID).Scan(&poolID); err != nil {
		t.Fatalf("carve-out probe: seed pool: %v", err)
	}
	var acctID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO send_managed.accounts
		   (user_id, pool_id, smtp_host, smtp_port, smtp_username,
		    credential_ciphertext, credential_key_id, credential_nonce, from_address)
		 VALUES ($1,$2,'probe.invalid',587,'probe@ourdomain.com','\x00','k','\x00','probe@ourdomain.com')
		 RETURNING id`, userID, poolID).Scan(&acctID); err != nil {
		t.Fatalf("carve-out probe: seed account: %v", err)
	}
	var jobItemID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status)
		 VALUES ($1,$1,'probe@example.com','S','<p>b</p>','pending') RETURNING id`, userID).Scan(&jobItemID); err != nil {
		t.Fatalf("carve-out probe: seed job_item: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('send_managed.actor','system',true)`); err != nil {
		t.Fatalf("carve-out probe: set actor: %v", err)
	}
	var jobID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO send_managed.jobs (job_item_id, campaign_id, user_id, recipient_email, rendered_subject, rendered_body, state, next_attempt_at)
		 VALUES ($1,$2,$2,'probe@example.com','S','<p>b</p>','queued',NOW()) RETURNING id`,
		jobItemID, userID).Scan(&jobID); err != nil {
		t.Fatalf("carve-out probe: seed job: %v", err)
	}
	var evID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO send_managed.job_events (job_id, user_id, from_state, to_state, actor, account_id)
		 VALUES ($1,$2,'queued','assigned','dispatcher',$3) RETURNING id`,
		jobID, userID, acctID).Scan(&evID); err != nil {
		t.Fatalf("carve-out probe: seed job_events row: %v", err)
	}

	// THE PROBE: exactly what `ON DELETE SET NULL` issues. Permitted only under send_managed_0024.
	permitted := true
	if _, err := tx.Exec(ctx, `UPDATE send_managed.job_events SET account_id=NULL WHERE id=$1`, evID); err != nil {
		permitted = false
		// A rejection must come from the append-only trigger; anything else means the probe itself is
		// broken and the answer would be meaningless.
		if !strings.Contains(err.Error(), "append-only: UPDATE forbidden") {
			t.Fatalf("carve-out probe: UPDATE failed for an unexpected reason (probe is broken, not the schema): %v", err)
		}
		// The transaction is now aborted; the deferred Rollback handles it.
	}
	return permitted
}

// jobEventsContentUpdateRejected is the paired NEGATIVE half of the probe above: with the carve-out
// active, a CONTENT mutation must STILL raise. Without this, jobEventsCarveoutActive alone would
// report "true" for a tree with the trigger dropped entirely — the exact regression that would turn
// an append-only audit log into a mutable one.
func jobEventsContentUpdateRejected(t *testing.T, c *pgx.Conn) bool {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatalf("begin content-reject probe: %v", err)
	}
	defer tx.Rollback(ctx)

	var userID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO public.users (email, is_active) VALUES ('carveout-probe2@sender.local', true) RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("content-reject probe: seed user: %v", err)
	}
	var jobItemID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status)
		 VALUES ($1,$1,'probe2@example.com','S','<p>b</p>','pending') RETURNING id`, userID).Scan(&jobItemID); err != nil {
		t.Fatalf("content-reject probe: seed job_item: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('send_managed.actor','system',true)`); err != nil {
		t.Fatalf("content-reject probe: set actor: %v", err)
	}
	var jobID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO send_managed.jobs (job_item_id, campaign_id, user_id, recipient_email, rendered_subject, rendered_body, state, next_attempt_at)
		 VALUES ($1,$2,$2,'probe2@example.com','S','<p>b</p>','queued',NOW()) RETURNING id`,
		jobItemID, userID).Scan(&jobID); err != nil {
		t.Fatalf("content-reject probe: seed job: %v", err)
	}
	var evID string
	if err := tx.QueryRow(ctx,
		`SELECT id FROM send_managed.job_events WHERE job_id=$1 ORDER BY seq LIMIT 1`, jobID).Scan(&evID); err != nil {
		t.Fatalf("content-reject probe: read genesis event: %v", err)
	}
	_, err = tx.Exec(ctx, `UPDATE send_managed.job_events SET actor='admin' WHERE id=$1`, evID)
	return err != nil && strings.Contains(err.Error(), "append-only: UPDATE forbidden")
}

// isErrorTransition evaluates send_managed_0022's predicate directly. It is IMMUTABLE SQL over the
// job_state enum, so the migration's actual decision can be asserted without driving a job.
func isErrorTransition(t *testing.T, c *pgx.Conn, from, to string) bool {
	t.Helper()
	var ok bool
	if err := c.QueryRow(context.Background(),
		`SELECT send_managed.fn_job_event_is_error_transition($1::send_managed.job_state, $2::send_managed.job_state)`,
		from, to).Scan(&ok); err != nil {
		t.Fatalf("is_error_transition(%q,%q): %v", from, to, err)
	}
	return ok
}

func accountsForceRLS(t *testing.T, c *pgx.Conn) bool {
	t.Helper()
	var relforcerowsecurity bool
	err := c.QueryRow(context.Background(),
		"SELECT relforcerowsecurity FROM pg_class WHERE oid='send_managed.accounts'::regclass").Scan(&relforcerowsecurity)
	if err != nil {
		t.Fatalf("force rls lookup: %v", err)
	}
	return relforcerowsecurity
}

func assertRole(t *testing.T, c *pgx.Conn, name string, wantLogin, wantSuper bool) {
	t.Helper()
	var login, super bool
	err := c.QueryRow(context.Background(),
		"SELECT rolcanlogin, rolsuper FROM pg_roles WHERE rolname=$1", name).Scan(&login, &super)
	if err != nil {
		t.Fatalf("role %s not found: %v", name, err)
	}
	if login != wantLogin || super != wantSuper {
		t.Fatalf("role %s: login=%v super=%v, want login=%v super=%v", name, login, super, wantLogin, wantSuper)
	}
}

func assertSchemaOwner(t *testing.T, c *pgx.Conn, schema, owner string) {
	t.Helper()
	var got string
	err := c.QueryRow(context.Background(),
		"SELECT r.rolname FROM pg_namespace n JOIN pg_roles r ON n.nspowner=r.oid WHERE n.nspname=$1", schema).Scan(&got)
	if err != nil {
		t.Fatalf("schema %s owner lookup: %v", schema, err)
	}
	if got != owner {
		t.Fatalf("schema %s owner = %s, want %s", schema, got, owner)
	}
}

func assertPlatformGrant(t *testing.T, c *pgx.Conn, table string) {
	t.Helper()
	var priv string
	err := c.QueryRow(context.Background(),
		`SELECT string_agg(privilege_type, ',') FROM information_schema.role_table_grants
		 WHERE grantee='send_managed_app' AND table_schema='public' AND table_name=$1`, table).Scan(&priv)
	if err != nil {
		t.Fatalf("grant lookup public.%s: %v", table, err)
	}
	if priv != "SELECT" {
		t.Fatalf("public.%s grants to send_managed_app = %q, want exactly SELECT", table, priv)
	}
}

func migrationsDirForTest() string {
	if v := os.Getenv("MIGRATIONS_DIR"); v != "" {
		return v
	}
	return "../../db/migrations"
}
func preflightPathForTest() string {
	if v := os.Getenv("PREFLIGHT_SQL"); v != "" {
		return v
	}
	return "../../db/preflight/check_platform_tables.sql"
}
