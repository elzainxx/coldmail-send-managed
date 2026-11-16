package main

// cancel_cap_probe_test.go — the lifecycle test's helpers for send_managed_0027 / send_managed_0028 (the
// `cancelled` state, the attempt cap, the cancel guard). They live beside runner_integration_test.go (same
// package, same TEST_DATABASE_URL gating through its callers) rather than inside it so the additions to that
// file stay a handful of call sites.

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// enumLabels returns a send_managed enum type's labels in enumsortorder.
func enumLabels(t *testing.T, c *pgx.Conn, typ string) []string {
	t.Helper()
	rows, err := c.Query(context.Background(),
		`SELECT e.enumlabel FROM pg_enum e
		   JOIN pg_type ty ON ty.oid = e.enumtypid
		   JOIN pg_namespace n ON n.oid = ty.typnamespace
		  WHERE n.nspname='send_managed' AND ty.typname=$1 ORDER BY e.enumsortorder`, typ)
	if err != nil {
		t.Fatalf("enum labels %s: %v", typ, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatalf("scan enum label: %v", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("enum labels rows: %v", err)
	}
	return out
}

// triggerExists reports whether a trigger of the given name exists on any send_managed table.
func triggerExists(t *testing.T, c *pgx.Conn, name string) bool {
	t.Helper()
	var ok bool
	if err := c.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM pg_trigger tg JOIN pg_class cl ON cl.oid = tg.tgrelid
		                JOIN pg_namespace n ON n.oid = cl.relnamespace
		               WHERE n.nspname='send_managed' AND tg.tgname=$1 AND NOT tg.tgisinternal)`, name).Scan(&ok); err != nil {
		t.Fatalf("trigger exists lookup %q: %v", name, err)
	}
	return ok
}

// constraintDef returns pg_get_constraintdef for a named constraint on a send_managed table ("" if absent).
func constraintDef(t *testing.T, c *pgx.Conn, table, name string) string {
	t.Helper()
	var def *string
	err := c.QueryRow(context.Background(),
		`SELECT pg_get_constraintdef(co.oid) FROM pg_constraint co
		   JOIN pg_class cl ON cl.oid = co.conrelid
		   JOIN pg_namespace n ON n.oid = cl.relnamespace
		  WHERE n.nspname='send_managed' AND cl.relname=$1 AND co.conname=$2`, table, name).Scan(&def)
	if err != nil && err != pgx.ErrNoRows {
		t.Fatalf("constraint def %s.%s: %v", table, name, err)
	}
	if def == nil {
		return ""
	}
	return *def
}

// assertCancelAndAttemptCapMigrations is the "after Up" half for send_managed_0027 and send_managed_0028. It is
// anti-vacuity for each object the pair adds, and it DRIVES the two behaviours that no existence check can
// see (a CHECK widened to a new enum value; a BEFORE trigger that rewrites NEW) the way
// jobEventsCarveoutActive drives 0024's — a pg_get_constraintdef/pg_get_functiondef match would assert the
// migration's own SQL against itself.
func assertCancelAndAttemptCapMigrations(t *testing.T, c *pgx.Conn) {
	t.Helper()
	// 0027: ONE value appended LAST, the six existing values exactly where they were (additive-only).
	want := []string{"queued", "assigned", "sending", "sent", "failed", "bounced", "cancelled"}
	if got := enumLabels(t, c, "job_state"); !equalStrings(got, want) {
		t.Fatalf("after Up: job_state labels = %v, want %v (send_managed_0027 appends exactly `cancelled`, last)", got, want)
	}
	// 0028: the four columns, the two dials with their documented defaults, the guard.
	for _, col := range []string{"failed_attempt_count", "first_failed_at", "cancel_requested_at", "terminal_reason"} {
		if !columnExists(t, c, "jobs", col) {
			t.Fatalf("after Up: send_managed.jobs.%s missing (send_managed_0028 not applied)", col)
		}
	}
	for key, def := range map[string]string{"job_max_failed_attempts": `{"v": 6}`, "job_failed_max_age_sec": `{"v": 86400}`} {
		var val, dflt string
		if err := c.QueryRow(context.Background(),
			`SELECT value::text, default_value::text FROM send_managed.config WHERE key=$1`, key).Scan(&val, &dflt); err != nil {
			t.Fatalf("after Up: read %s: %v (send_managed_0028 seed not applied)", key, err)
		}
		if val != def || dflt != def {
			t.Fatalf("after Up: %s value=%s default_value=%s, want both %s (the documented default, JSONB-wrapped)", key, val, dflt, def)
		}
	}
	if !triggerExists(t, c, "trg_jobs_cancel_guard") || !functionExists(t, c, "fn_jobs_cancel_guard") {
		t.Fatal("after Up: the cancel guard trigger/function is missing (send_managed_0028 not applied)")
	}
	p := cancelGuardProbe(t, c)
	if p.unflagged != "queued" {
		t.Fatalf("after Up: an UNFLAGGED in-flight job moved back to queued reads %q, want queued — the guard must not touch an ordinary requeue", p.unflagged)
	}
	if p.flagged != "cancelled" || !p.flaggedCompleted || p.flaggedReason != "cancelled" {
		t.Fatalf("after Up: a FLAGGED in-flight job moved back to queued reads state=%q completed=%v terminal_reason=%q, want cancelled/true/cancelled — a cancel must stick for an item that was in flight",
			p.flagged, p.flaggedCompleted, p.flaggedReason)
	}
	if !p.checkRejects || !p.checkAccepts {
		t.Fatalf("after Up: jobs_completed_at_valid_check rejects a cancelled row without completed_at = %v, accepts one with it = %v, want true/true (the CHECK must treat cancelled as terminal)",
			p.checkRejects, p.checkAccepts)
	}
}

// guardProbeResult is what cancelGuardProbe observed.
type guardProbeResult struct {
	unflagged        string
	flagged          string
	flaggedCompleted bool
	flaggedReason    string
	checkRejects     bool
	checkAccepts     bool
}

// cancelGuardProbe drives the two behaviours in ONE rolled-back transaction on the admin connection (so the
// lifecycle database is unchanged): a requeue of an unflagged in-flight job, a requeue of a flagged one, and
// the widened terminal-state CHECK in both directions (the rejecting probe under a savepoint, since a failed
// statement aborts the transaction).
func cancelGuardProbe(t *testing.T, c *pgx.Conn) guardProbeResult {
	t.Helper()
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatalf("begin cancel-guard probe: %v", err)
	}
	defer tx.Rollback(ctx)

	var userID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO public.users (email, is_active) VALUES ('cancel-probe@sender.local', true) RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("cancel-guard probe: seed user: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('send_managed.actor','system',true)`); err != nil {
		t.Fatalf("cancel-guard probe: set actor: %v", err)
	}
	newJob := func(label string) string {
		var itemID, jobID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status)
			 VALUES ($1,$1,$2,'S','<p>b</p>','pending') RETURNING id`, userID, label+"@example.com").Scan(&itemID); err != nil {
			t.Fatalf("cancel-guard probe: seed job_item %s: %v", label, err)
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO send_managed.jobs (job_item_id, campaign_id, user_id, recipient_email, rendered_subject, rendered_body, state, next_attempt_at)
			 VALUES ($1,$2,$2,$3,'S','<p>b</p>','queued',NOW()) RETURNING id`, itemID, userID, label+"@example.com").Scan(&jobID); err != nil {
			t.Fatalf("cancel-guard probe: seed job %s: %v", label, err)
		}
		return jobID
	}
	toAssigned := func(id string) {
		if _, err := tx.Exec(ctx,
			`UPDATE send_managed.jobs SET state='assigned', lease_expires_at=NOW()+INTERVAL '120 seconds' WHERE id=$1`, id); err != nil {
			t.Fatalf("cancel-guard probe: assign %s: %v", id, err)
		}
	}
	requeue := func(id string) {
		if _, err := tx.Exec(ctx,
			`UPDATE send_managed.jobs SET state='queued', lease_expires_at=NULL WHERE id=$1`, id); err != nil {
			t.Fatalf("cancel-guard probe: requeue %s: %v", id, err)
		}
	}

	var res guardProbeResult

	// An ordinary (unflagged) in-flight job is requeued as always.
	a := newJob("probe-unflagged")
	toAssigned(a)
	requeue(a)
	if err := tx.QueryRow(ctx, `SELECT state::text FROM send_managed.jobs WHERE id=$1`, a).Scan(&res.unflagged); err != nil {
		t.Fatalf("cancel-guard probe: read unflagged: %v", err)
	}

	// A FLAGGED in-flight job that is requeued becomes cancelled.
	b := newJob("probe-flagged")
	toAssigned(b)
	if _, err := tx.Exec(ctx, `UPDATE send_managed.jobs SET cancel_requested_at=NOW() WHERE id=$1`, b); err != nil {
		t.Fatalf("cancel-guard probe: flag: %v", err)
	}
	requeue(b)
	var reason *string
	if err := tx.QueryRow(ctx,
		`SELECT state::text, completed_at IS NOT NULL, terminal_reason FROM send_managed.jobs WHERE id=$1`, b).
		Scan(&res.flagged, &res.flaggedCompleted, &reason); err != nil {
		t.Fatalf("cancel-guard probe: read flagged: %v", err)
	}
	if reason != nil {
		res.flaggedReason = *reason
	}

	// The widened terminal-state CHECK, both directions.
	d := newJob("probe-check")
	if _, err := tx.Exec(ctx, `SAVEPOINT chk_reject`); err != nil {
		t.Fatalf("cancel-guard probe: savepoint: %v", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE send_managed.jobs SET state='cancelled' WHERE id=$1`, d); err != nil {
		res.checkRejects = strings.Contains(err.Error(), "jobs_completed_at_valid_check")
		if _, rerr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT chk_reject`); rerr != nil {
			t.Fatalf("cancel-guard probe: rollback to savepoint: %v", rerr)
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE send_managed.jobs SET state='cancelled', completed_at=NOW() WHERE id=$1`, d); err == nil {
		res.checkAccepts = true
	}
	return res
}
