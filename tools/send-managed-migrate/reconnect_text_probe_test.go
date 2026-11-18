package main

// reconnect_text_probe_test.go — the lifecycle test's helpers for send_managed_0029 (the three worker reconnect
// dials, Phase 3 close-out B1) and send_managed_0030 (the nullable send_managed.jobs.rendered_text column,
// close-out B3), plus the pre-flight's new required column. Real Postgres, never a mock (CLAUDE.md Rule 3).

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// assertReconnectAndTextMigrations is the "after Up" half for 0029 and 0030. Anti-vacuity for each object, and
// it DRIVES the CHECK rather than reading its definition back (a pg_get_constraintdef match would assert the
// migration's SQL against itself).
func assertReconnectAndTextMigrations(t *testing.T, c *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	// 0029: three integer dials at their documented production defaults, JSONB-wrapped, value == default_value.
	for key, def := range map[string]string{
		"worker_reconnect_base_ms":    `{"v": 1000}`,
		"worker_reconnect_cap_ms":     `{"v": 30000}`,
		"worker_reconnect_jitter_pct": `{"v": 20}`,
	} {
		val, dflt := configValueAndDefault(t, c, key)
		if val != def || dflt != def {
			t.Fatalf("after Up: %s value=%s default_value=%s, want both %s (the documented default, JSONB-wrapped)", key, val, dflt, def)
		}
		if vt := configValueType(t, c, key); vt != "integer" {
			t.Fatalf("after Up: %s value_type = %q, want integer", key, vt)
		}
	}
	// 0030: the column exists, is TEXT, nullable, with NO default (so every existing row reads as absent).
	var dataType, nullable string
	var dflt *string
	if err := c.QueryRow(ctx,
		`SELECT data_type, is_nullable, column_default FROM information_schema.columns
		  WHERE table_schema='send_managed' AND table_name='jobs' AND column_name='rendered_text'`).Scan(&dataType, &nullable, &dflt); err != nil {
		t.Fatalf("after Up: send_managed.jobs.rendered_text missing (send_managed_0030 not applied): %v", err)
	}
	if dataType != "text" || nullable != "YES" || dflt != nil {
		t.Fatalf("after Up: jobs.rendered_text is %s nullable=%s default=%v, want text / YES / no default", dataType, nullable, dflt)
	}
	// Drive the size CHECK in ONE rolled-back transaction: NULL, a short text and exactly 1 MiB are accepted, 1 MiB + 1 byte
	// is refused with 23514 (check_violation) naming the constraint. The paired control is the at-limit insert.
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatalf("begin rendered_text probe: %v", err)
	}
	defer tx.Rollback(ctx)
	var userID string
	if err := tx.QueryRow(ctx, `INSERT INTO public.users (email, is_active) VALUES ('rendered-text-probe@sender.local', true) RETURNING id`).Scan(&userID); err != nil {
		t.Fatalf("rendered_text probe: seed user: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('send_managed.actor','system',true)`); err != nil {
		t.Fatalf("rendered_text probe: set actor: %v", err)
	}
	insert := func(label string, text *string, big int) error {
		var itemID string
		if err := tx.QueryRow(ctx,
			`INSERT INTO public.job_items (campaign_id, user_id, recipient_email, rendered_subject, rendered_body, status)
			 VALUES ($1,$1,$2,'S','<p>b</p>','pending') RETURNING id`, userID, label+"@example.com").Scan(&itemID); err != nil {
			t.Fatalf("rendered_text probe: seed job_item %s: %v", label, err)
		}
		if _, err := tx.Exec(ctx, `SAVEPOINT sp`); err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		var err error
		if big > 0 {
			_, err = tx.Exec(ctx,
				`INSERT INTO send_managed.jobs (job_item_id, campaign_id, user_id, recipient_email, rendered_subject, rendered_body, rendered_text, state, next_attempt_at)
				 VALUES ($1,$2,$2,$3,'S','<p>b</p>', repeat('x', $4), 'queued', NOW())`, itemID, userID, label+"@example.com", big)
		} else {
			_, err = tx.Exec(ctx,
				`INSERT INTO send_managed.jobs (job_item_id, campaign_id, user_id, recipient_email, rendered_subject, rendered_body, rendered_text, state, next_attempt_at)
				 VALUES ($1,$2,$2,$3,'S','<p>b</p>', $4, 'queued', NOW())`, itemID, userID, label+"@example.com", text)
		}
		if err != nil {
			_, _ = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT sp`)
			return err
		}
		_, _ = tx.Exec(ctx, `RELEASE SAVEPOINT sp`)
		return nil
	}
	short := "hello"
	for label, text := range map[string]*string{"rt-null": nil, "rt-short": &short} {
		if err := insert(label, text, 0); err != nil {
			t.Fatalf("after Up: a job with rendered_text %v was refused: %v", text, err)
		}
	}
	if err := insert("rt-atlimit", nil, 1048576); err != nil {
		t.Fatalf("after Up: a job with rendered_text of exactly 1 MiB (1048576 bytes) was refused: %v — the CHECK is tighter than documented", err)
	}
	err = insert("rt-overlimit", nil, 1048577)
	if err == nil {
		t.Fatal("after Up: a job with rendered_text of 1 MiB + 1 byte was ACCEPTED — jobs_rendered_text_size_check is not enforcing the documented limit")
	}
	if !strings.Contains(err.Error(), "jobs_rendered_text_size_check") {
		t.Fatalf("after Up: the over-limit insert failed for the wrong reason (want jobs_rendered_text_size_check): %v", err)
	}
}

// TestPreflight_RequiresRenderedText: the pre-flight refuses a public.job_items that has no rendered_text
// column, NAMING it, and accepts the same table once the column exists. Its own scratch database, so the
// lifecycle database is untouched. The paired control is the second half: the SAME script passes when the column
// is there, so the first half cannot be "the script always fails".
func TestPreflight_RequiresRenderedText(t *testing.T) {
	dsn := testDSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	base, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	sqlBytes, err := os.ReadFile(preflightPathForTest())
	if err != nil {
		t.Fatalf("read preflight: %v", err)
	}
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(context.Background())
	const scratch = "send_managed_preflight_probe"
	mustExec(t, admin, "DROP DATABASE IF EXISTS "+scratch)
	mustExec(t, admin, "CREATE DATABASE "+scratch)
	defer func() {
		c, cerr := pgx.Connect(context.Background(), dsn)
		if cerr == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+scratch)
			c.Close(context.Background())
		}
	}()
	cfg := base.Copy()
	cfg.Database = scratch
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	defer conn.Close(context.Background())

	mustExec(t, conn, `CREATE TABLE public.users(id uuid primary key default gen_random_uuid(), email text, is_active boolean default true, created_at timestamptz default now())`)
	mustExec(t, conn, `CREATE TABLE public.job_items(id uuid primary key default gen_random_uuid(), campaign_id uuid, user_id uuid, recipient_email text, rendered_subject text, rendered_body text, reply_to text, status text, created_at timestamptz default now())`)

	_, err = conn.Exec(ctx, string(sqlBytes))
	if err == nil {
		t.Fatal("the pre-flight ACCEPTED a public.job_items without rendered_text — a platform that was not migrated first would make every POST /v1/run answer 500")
	}
	if !strings.Contains(err.Error(), "rendered_text") {
		t.Fatalf("the pre-flight refused for the wrong reason (it must name rendered_text): %v", err)
	}

	mustExec(t, conn, `ALTER TABLE public.job_items ADD COLUMN rendered_text text`)
	if _, err := conn.Exec(ctx, string(sqlBytes)); err != nil {
		t.Fatalf("control: the pre-flight REFUSED a public.job_items that has rendered_text: %v", err)
	}
}
