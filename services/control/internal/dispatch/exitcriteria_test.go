package dispatch

import (
	"context"
	"strings"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// TestExit_AuthFailureThenSendsViaDifferentAccount (T2, exit criterion) — a job whose first attempt
// lands on AccBad (credential rejected, 535) quarantines AccBad + requeues; the NEXT dispatch skips
// the quarantined account and sends via AccGood; the job reaches sent; AccBad STAYS quarantined
// (no auto-recovery). The genuine end-to-end proof that quarantine sheds the bad account for the
// job's retry without losing the send.
func TestExit_AuthFailureThenSendsViaDifferentAccount(t *testing.T) {
	e := newDispatchEnv(t, "authreroute")
	ctx := context.Background()
	pool := e.seedPool(t)
	accBad := e.seedAccount(t, pool, "bad-cred")
	accGood := e.seedAccount(t, pool, "good-cred")
	// Force the first claim onto AccBad by making AccGood momentarily ineligible via a HIGHER id is
	// not reliable; instead drive the outcome for whichever account was actually used, then assert
	// the SECOND send used a DIFFERENT account. Determine which account got the first push.
	job := e.seedQueuedJob(t, "reroute@example.com")
	e.dispatchToSending(t, job)
	pj1, ok := e.px.lastPush()
	if !ok {
		t.Fatal("no first push")
	}
	firstAcct := accountIDFromUsername(t, pj1.SMTP.Username, accBad, accGood)

	// The worker reports a 535 auth_failure for the first attempt → that account quarantines + requeue.
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
		SMTPResponse: "535 5.7.8 Authentication credentials invalid"})
	e.px.freeAllWorkers()

	// The account used first is now quarantined; the job is queued again.
	var quar bool
	if err := e.admin.QueryRow(ctx, `SELECT quarantined FROM send_managed.accounts WHERE id=$1`, firstAcct).Scan(&quar); err != nil {
		t.Fatalf("quar read: %v", err)
	}
	if !quar {
		t.Fatalf("first account %s not quarantined after 535", firstAcct)
	}
	if s := e.jobStateStr(t, job); s != "queued" {
		t.Fatalf("job state=%s after auth_failure, want queued", s)
	}

	// Next dispatch: routing skips the quarantined account and uses the OTHER one.
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
		t.Fatalf("reset next: %v", err)
	}
	e.dispatchToSending(t, job)
	pj2, ok := e.px.lastPush()
	if !ok {
		t.Fatal("no second push")
	}
	secondAcct := accountIDFromUsername(t, pj2.SMTP.Username, accBad, accGood)
	if secondAcct == firstAcct {
		t.Fatalf("second attempt reused the quarantined account %s — routing must skip it", firstAcct)
	}

	// Complete the send on the good account → job sent.
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-REROUTE-1", SMTPResponse: "250 2.0.0 Ok"})
	if s := e.jobStateStr(t, job); s != "sent" {
		t.Fatalf("job state=%s, want sent (via the non-quarantined account)", s)
	}
	// AccBad stays quarantined (no auto-recovery).
	if err := e.admin.QueryRow(ctx, `SELECT quarantined FROM send_managed.accounts WHERE id=$1`, firstAcct).Scan(&quar); err != nil {
		t.Fatalf("quar recheck: %v", err)
	}
	if !quar {
		t.Fatalf("quarantined account auto-recovered — recovery must be manual only (§4.2)")
	}
}

// accountIDFromUsername maps a pushed SMTP username back to the seeded account id. seedAccount
// derives smtp_username as "u-<id>@ourdomain.com", so the id is embedded in the username.
func accountIDFromUsername(t *testing.T, username string, candidates ...string) string {
	t.Helper()
	for _, id := range candidates {
		if strings.Contains(username, id) {
			return id
		}
	}
	t.Fatalf("pushed username %q matches none of the seeded accounts %v", username, candidates)
	return ""
}

// TestExit_RetryNeverDoubleSends (T7, exit criterion) — a job that obtained a provider_message_id
// (a real 250 that was persisted) is NEVER re-sent even if a duplicate accepted report arrives or a
// re-dispatch is attempted; the Phase-3 idempotency gate short-circuits. Guards that Phase 5's
// aggressive requeue policy did not weaken the idempotency guarantee.
func TestExit_RetryNeverDoubleSends(t *testing.T) {
	e := newDispatchEnv(t, "noresend")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	job := e.seedQueuedJob(t, "once@example.com")
	e.dispatchToSending(t, job)

	// First accept persists the provider_message_id + one send_log.
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-ONCE-1", SMTPResponse: "250 2.0.0 Ok"})
	if n := e.sendLogCount(t, job); n != 1 {
		t.Fatalf("send_log after first accept=%d, want 1", n)
	}

	// A DUPLICATE accepted report (replay) is a clean no-op: no second send_log, id unchanged.
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "PMID-DIFFERENT-2", SMTPResponse: "250 2.0.0 Ok again"})
	if n := e.sendLogCount(t, job); n != 1 {
		t.Fatalf("send_log after duplicate accept=%d, want 1 (idempotent no-op §5.3)", n)
	}
	var pmid string
	if err := e.admin.QueryRow(ctx, `SELECT provider_message_id FROM send_managed.jobs WHERE id=$1`, job).Scan(&pmid); err != nil {
		t.Fatalf("pmid: %v", err)
	}
	if pmid != "PMID-ONCE-1" {
		t.Fatalf("provider_message_id changed to %q on replay — idempotency broken", pmid)
	}

	// Even a re-dispatch attempt cannot re-send: the claim WHERE provider_message_id IS NULL skips
	// a job that already has one. Reset next_attempt_at and try — nothing is pushed for this job.
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.jobs SET next_attempt_at=NOW() WHERE id=$1`, job); err != nil {
		t.Fatalf("reset: %v", err)
	}
	before := e.px.pushCount()
	if _, err := e.d.TryDispatch(ctx); err != nil {
		t.Fatalf("re-dispatch: %v", err)
	}
	if e.px.pushCount() != before {
		t.Fatalf("a job with a provider_message_id was re-dispatched — idempotency gate failed")
	}
}

// TestExit_ActorVocabulariesNeverMix (T8, exit criterion) — automatic transitions write
// account_events with actor='system'; manual (admin) transitions write actor='admin'; the two
// never mix. Here: an auto quarantine (system) followed by a manual unquarantine (simulating the
// Phase 2 admin path via a real audited transition) — assert each row carries the correct actor.
func TestExit_ActorVocabulariesNeverMix(t *testing.T) {
	e := newDispatchEnv(t, "actors")
	ctx := context.Background()
	pool := e.seedPool(t)
	acct := e.seedAccount(t, pool, "sec")
	job := e.seedQueuedJob(t, "act@example.com")
	e.dispatchToSending(t, job)

	// Auto quarantine via a 535 auth_failure → account_events(system, quarantine).
	e.oh.Handle(e.px.workerID, workerproto.Report{JobID: job, Outcome: workerproto.OutcomeError,
		SMTPResponse: "535 5.7.8 auth failed"})

	// Every automatic event is actor='system'; none is 'admin'.
	var sysCount, admCount int
	if err := e.admin.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE actor='system'), count(*) FILTER (WHERE actor='admin')
		 FROM send_managed.account_events WHERE account_id=$1`, acct).Scan(&sysCount, &admCount); err != nil {
		t.Fatalf("actor counts: %v", err)
	}
	if sysCount < 1 {
		t.Fatalf("no actor='system' account_events after an automatic quarantine")
	}
	if admCount != 0 {
		t.Fatalf("an automatic transition wrote actor='admin' (%d) — vocabularies must not mix (§4.2/T8)", admCount)
	}

	// Now a MANUAL admin transition (unquarantine) via a real audited write with actor='admin'
	// (mirrors the Phase 2 admin lifecycle path). Assert it lands as 'admin', distinct from system.
	if _, err := e.admin.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := e.admin.Exec(ctx,
		`INSERT INTO send_managed.account_events (account_id, user_id, actor, action, from_flags, to_flags, reason)
		 SELECT id, user_id, 'admin', 'unquarantine',
		        '{"paused":false,"pause_reason":null,"quarantined":true,"disabled":false}'::jsonb,
		        '{"paused":false,"pause_reason":null,"quarantined":false,"disabled":false}'::jsonb,
		        'operator recovered the account'
		 FROM send_managed.accounts WHERE id=$1`, acct); err != nil {
		t.Fatalf("admin event: %v", err)
	}
	if _, err := e.admin.Exec(ctx, `UPDATE send_managed.accounts SET quarantined=FALSE WHERE id=$1`, acct); err != nil {
		t.Fatalf("unquarantine: %v", err)
	}
	if _, err := e.admin.Exec(ctx, `COMMIT`); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The system quarantine event and the admin unquarantine event coexist with distinct actors.
	rows, err := e.admin.Query(ctx,
		`SELECT actor::text, action FROM send_managed.account_events WHERE account_id=$1 ORDER BY created_at`, acct)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()
	var sawSystemQuar, sawAdminUnquar bool
	for rows.Next() {
		var actor, action string
		if err := rows.Scan(&actor, &action); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if actor == "system" && action == "quarantine" {
			sawSystemQuar = true
		}
		if actor == "admin" && action == "unquarantine" {
			sawAdminUnquar = true
		}
		// An auto action is never 'admin' and a manual 'unquarantine' is never 'system'.
		if action == "quarantine" && actor == "admin" {
			t.Fatalf("quarantine event stamped admin — auto action must be system")
		}
	}
	if !sawSystemQuar || !sawAdminUnquar {
		t.Fatalf("expected both a system quarantine and an admin unquarantine event (system=%v admin=%v)", sawSystemQuar, sawAdminUnquar)
	}
}
