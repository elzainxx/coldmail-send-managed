package harness

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestGrantsInvariant is a schema-wide, catalog-enumerated guard on the send_managed_app DML
// grants. It exists because of the ADR-0008 grants trap: Phase 0 pre-created the schema and
// feature objects are created as admin then ALTER OWNER TO send_managed_owner, so ALTER DEFAULT
// PRIVILEGES does NOT fire — every table needs an EXPLICIT grant (send_managed_0012). Forgetting that
// for a future table means it silently has zero rights for send_managed_app and fails at runtime.
//
// This test ENUMERATES tables dynamically from pg_tables, so a table added in a later phase
// WITHOUT grants FAILS here rather than being silently skipped. Each table's expected privilege
// set is derived from its class:
//   - append-only (job_events): INSERT, SELECT           (no UPDATE, no DELETE)
//   - write-once  (send_logs) : INSERT, SELECT, DELETE   (no UPDATE; DELETE = Phase 7 retention)
//   - bootstrap   (schema_migrations): SELECT             (owned by send_managed_0000; module reads it)
//   - normal      (everything else)  : SELECT, INSERT, UPDATE, DELETE
//
// If a later phase adds a table with a genuinely different privilege class, it updates the
// classifier below IN THE SAME CHANGE — that edit is the deliberate, reviewed acknowledgement
// that a new grant posture was intended (the test can't be satisfied by silence).
func TestGrantsInvariant(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "grantsinv")
	defer cleanup()

	c := Connect(t, cloneDSN)

	// Enumerate every base table in the send_managed schema from the catalog.
	rows, err := c.Query(ctx,
		"SELECT tablename FROM pg_tables WHERE schemaname='send_managed' ORDER BY tablename")
	if err != nil {
		t.Fatalf("enumerate send_managed tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var tn string
		if err := rows.Scan(&tn); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		tables = append(tables, tn)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	// Anti-vacuity: the schema must actually have the Phase 1 feature tables, or this test is
	// meaningless. Assert the known set is present (a superset check — later phases add more).
	mustHave := []string{"account_events", "accounts", "boxes", "config", "job_events", "jobs", "pools", "schema_migrations", "send_logs"}
	present := map[string]bool{}
	for _, tn := range tables {
		present[tn] = true
	}
	for _, tn := range mustHave {
		if !present[tn] {
			t.Fatalf("anti-vacuity: expected send_managed.%s to exist; enumerated %v", tn, tables)
		}
	}

	for _, tn := range tables {
		want := expectedPrivs(tn)
		got := grantsFor(t, c, tn)
		if !sameStringSet(got, want) {
			t.Fatalf("grants invariant FAILED for send_managed.%s: send_managed_app has {%s}, expected {%s}. "+
				"A new table needs EXPLICIT grants (ADR-0008); add them in the migration that creates it, "+
				"or update the classifier in this test IN THE SAME CHANGE if a new privilege class is intended.",
				tn, strings.Join(got, ","), strings.Join(want, ","))
		}
	}
}

// expectedPrivs returns the privilege set send_managed_app must hold on a send_managed.<table>, by class.
func expectedPrivs(table string) []string {
	switch table {
	case "job_events": // append-only audit log
		return []string{"INSERT", "SELECT"}
	case "account_events": // Phase 2 append-only account audit sink (send_managed_0014)
		return []string{"INSERT", "SELECT"}
	case "send_logs": // write-once, DELETE for the Phase 7 retention sweep
		return []string{"DELETE", "INSERT", "SELECT"}
	case "schema_migrations": // bootstrap-owned (send_managed_0000); module reads only
		return []string{"SELECT"}
	default: // normal mutable table
		return []string{"DELETE", "INSERT", "SELECT", "UPDATE"}
	}
}

func grantsFor(t *testing.T, c *pgx.Conn, table string) []string {
	t.Helper()
	rows, err := c.Query(context.Background(),
		`SELECT privilege_type FROM information_schema.role_table_grants
		 WHERE grantee='send_managed_app' AND table_schema='send_managed' AND table_name=$1`, table)
	if err != nil {
		t.Fatalf("grants lookup send_managed.%s: %v", table, err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		got = append(got, p)
	}
	sort.Strings(got)
	return got
}

func sameStringSet(a, b []string) bool {
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	if len(aa) != len(bb) {
		return false
	}
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

// TestSendLogsWriteOnceButRetentionCanDelete verifies the send_logs end state required by the
// Phase 1 closeout ruling 2 and the Phase 7 retention sweep: rows are IMMUTABLE (UPDATE
// rejected) but the retention path CAN delete them. Per Phase 7 §3.1, the sweep runs IN THE
// CONTROL as send_managed_app (it "uses the send_logs DELETE privilege Phase 1 send_managed_0009 reserved
// exactly for this") — so no separate role and no new migration are needed; send_managed_0012 already
// grants send_logs INSERT+SELECT+DELETE and revokes UPDATE. This test asserts that behaviorally
// as send_managed_app, with anti-vacuity positive controls (INSERT + SELECT succeed).
func TestSendLogsWriteOnceButRetentionCanDelete(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "sendlogret")
	defer cleanup()

	admin := Connect(t, cloneDSN)
	uid := SeedUser(t, admin, "sl@example.com")
	jobID := InsertQueuedJob(t, admin, uid, uid, "rcpt-sl@example.com")

	m := moduleConn(t, cloneDSN)

	// Positive control (anti-vacuity): send_managed_app CAN INSERT a send_logs row — an EXPIRED
	// one (ttl in the past) so the retention sweep query below is eligible to delete it.
	var logID string
	if err := m.QueryRow(ctx,
		`INSERT INTO send_managed.send_logs (job_id,user_id,recipient_email,subject,body,headers,smtp_response,ttl_expires_at)
		 VALUES ($1,$2,'r@x.co','s','b','{}'::jsonb,'250 OK', NOW() - INTERVAL '1 day') RETURNING id`,
		jobID, uid).Scan(&logID); err != nil {
		t.Fatalf("module INSERT send_logs failed (should succeed): %v", err)
	}
	// Positive control: SELECT succeeds.
	var seen int
	if err := m.QueryRow(ctx, "SELECT count(*) FROM send_managed.send_logs WHERE id=$1", logID).Scan(&seen); err != nil || seen != 1 {
		t.Fatalf("module SELECT send_logs: seen=%d err=%v (want 1, nil)", seen, err)
	}

	// UPDATE must be REJECTED (write-once: REVOKE UPDATE + BEFORE UPDATE reject-trigger).
	if _, err := m.Exec(ctx, "UPDATE send_managed.send_logs SET smtp_response='tampered' WHERE id=$1", logID); err == nil {
		t.Fatal("UPDATE send_managed.send_logs succeeded, want rejection (write-once)")
	}
	// Confirm the row is unchanged after the rejected UPDATE.
	var resp string
	if err := admin.QueryRow(ctx, "SELECT smtp_response FROM send_managed.send_logs WHERE id=$1", logID).Scan(&resp); err != nil {
		t.Fatalf("read smtp_response: %v", err)
	}
	if resp != "250 OK" {
		t.Fatalf("send_logs row mutated despite write-once (%q), want '250 OK'", resp)
	}

	// The retention sweep path (send_managed_app, Phase 7 §3.1) CAN DELETE expired rows. Use the
	// exact batched-DELETE shape from the Phase 7 spec.
	tag, err := m.Exec(ctx,
		"DELETE FROM send_managed.send_logs WHERE ctid IN (SELECT ctid FROM send_managed.send_logs WHERE ttl_expires_at < NOW() LIMIT 1000)")
	if err != nil {
		t.Fatalf("retention-path DELETE failed (should succeed as send_managed_app): %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("retention DELETE removed %d rows, want 1", tag.RowsAffected())
	}
	// Confirm the expired row is gone.
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM send_managed.send_logs WHERE id=$1", logID).Scan(&seen); err != nil {
		t.Fatalf("recount: %v", err)
	}
	if seen != 0 {
		t.Fatalf("retention DELETE left %d rows, want 0", seen)
	}
}

// TestGrantsInvariant_AntiVacuity proves the invariant test above has teeth: a NEW table created
// (as admin, then reowned to send_managed_owner, exactly like a real feature migration) WITHOUT any
// explicit grant must be caught — send_managed_app holds ZERO privileges on it, which is NOT any
// expected class, so the invariant check flags it. We assert that directly here (self-contained,
// reverts by dropping the clone), so a reviewer sees the negative control without editing the
// real schema.
func TestGrantsInvariant_AntiVacuity(t *testing.T) {
	adminDSN := AdminDSN(t)
	template := requireTemplate(t)
	ctx := context.Background()

	cloneDSN, cleanup := CloneDB(t, adminDSN, template, "grantsinvneg")
	defer cleanup()

	c := Connect(t, cloneDSN)

	// Simulate a phase author adding a table the ADR-0008 way but forgetting the explicit grant.
	if _, err := c.Exec(ctx, "CREATE TABLE send_managed.forgot_grants (id uuid primary key default gen_random_uuid())"); err != nil {
		t.Fatalf("create test table: %v", err)
	}
	if _, err := c.Exec(ctx, "ALTER TABLE send_managed.forgot_grants OWNER TO send_managed_owner"); err != nil {
		t.Fatalf("reown test table: %v", err)
	}

	// The forgotten table has zero send_managed_app grants (ADR-0008: default privileges don't fire
	// for reowned tables). Prove the invariant check would REJECT it.
	got := grantsFor(t, c, "forgot_grants")
	want := expectedPrivs("forgot_grants") // classified 'normal' => SIUD
	if len(got) != 0 {
		t.Fatalf("anti-vacuity precondition: expected the ungranted table to have ZERO grants, got {%s}", strings.Join(got, ","))
	}
	if sameStringSet(got, want) {
		t.Fatal("anti-vacuity BROKEN: the invariant check would PASS an ungranted table — it has no teeth")
	}
	// (Clone is dropped by cleanup — the real schema is never touched.)
}
