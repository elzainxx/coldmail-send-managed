package dispatch

// Phase 7 §2 (P7-3) — Supabase-down behavioral tests: "the Control invents nothing".
//
// REAL failure injection, no mocks (CLAUDE.md Rule 3). The database is a REAL Postgres clone and the
// driver is the REAL pgxpool; the outage is produced by putting a REAL TCP proxy in front of Postgres
// and genuinely severing it — every live connection is closed and new ones are refused, exactly as a
// Supabase restart looks to the client (broken pipe / connection refused / EOF mid-query). Nothing
// about the DB, the driver, the dispatcher, the reclaim loop, or the outcome handler is stubbed. The
// proxy is the failure INJECTOR (the equivalent of `docker stop postgres`, but scoped to one test and
// safe to run in parallel with the rest of the suite), not a stand-in for any component under test.
//
// What these prove, in the spec's own terms:
//   §2.2  the dispatch loop and the reclaim loop PAUSE, and the Control writes NO job state;
//   §2.2  no crash-loop: the loops stay alive on bounded exponential backoff;
//   §2.3  a worker's outcome produced during the outage is NOT acked (so the worker retains it);
//   §2.4  on recovery the loops resume from STORED state and the replayed outcome persists ONCE.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coldmail-send-managed/control/internal/workerproto"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// --- the outage injector -----------------------------------------------------------------------

// pgProxy is a real TCP proxy in front of the real Postgres server. While up it forwards bytes
// verbatim (the pool is talking to genuine Postgres through it). cut() severs it: every live
// connection is closed and subsequent connections are accepted then immediately dropped, which is
// what a real DB outage looks like on the wire. restore() puts it back.
type pgProxy struct {
	ln     net.Listener
	target string

	down atomic.Bool

	mu    sync.Mutex
	conns []net.Conn
	dials atomic.Int64 // connection attempts observed while down (bounded-backoff evidence)
}

// startPGProxy binds a loopback listener forwarding to the Postgres host:port encoded in dsn, and
// returns the proxy plus a DSN pointing at it (same credentials/database, proxied address).
func startPGProxy(t *testing.T, dsn string) (*pgProxy, string) {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &pgProxy{ln: ln, target: net.JoinHostPort(host, port)}
	go p.serve()
	t.Cleanup(p.close)

	pu := *u
	pu.Host = ln.Addr().String()
	// sslmode=disable: the hop to the proxy is loopback, and the proxy→Postgres hop uses whatever the
	// original DSN did. (The dev DSN is already non-TLS loopback; this only makes it explicit.)
	q := pu.Query()
	q.Set("sslmode", "disable")
	pu.RawQuery = q.Encode()
	return p, pu.String()
}

func (p *pgProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		if p.down.Load() {
			p.dials.Add(1)
			_ = c.Close() // refuse: a real "cannot connect" for the client
			continue
		}
		go p.pipe(c)
	}
}

func (p *pgProxy) pipe(client net.Conn) {
	server, err := net.DialTimeout("tcp", p.target, 5*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	p.mu.Lock()
	p.conns = append(p.conns, client, server)
	p.mu.Unlock()
	go func() { _, _ = io.Copy(server, client); _ = server.Close() }()
	go func() { _, _ = io.Copy(client, server); _ = client.Close() }()
}

// cut severs the link: a REAL outage. Live connections are closed mid-flight (a query in progress
// fails with a transport error) and new connections are refused.
func (p *pgProxy) cut() {
	p.down.Store(true)
	p.dials.Store(0)
	p.mu.Lock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
	p.mu.Unlock()
}

// restore brings the link back up (the DB "returns").
func (p *pgProxy) restore() { p.down.Store(false) }

func (p *pgProxy) close() {
	_ = p.ln.Close()
	p.mu.Lock()
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
	p.mu.Unlock()
}

// proxiedPool opens a REAL pgxpool through the proxy. MaxConnLifetime/HealthCheck are left at their
// defaults — we want the driver's genuine behavior against a severed socket.
func proxiedPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("proxied pool: %v", err)
	}
	t.Cleanup(pool.Close)
	// Prove the proxy path genuinely reaches Postgres before any test asserts on its failure.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var one int
	if err := pool.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("proxied pool cannot reach Postgres (proxy misconfigured): %v", err)
	}
	return pool
}

// --- T2 core: loops pause, nothing is invented, resume needs no reconciliation -------------------

// TestOutage_LoopsPauseAndInventNothing (§2.2 + §2.4 — the phase's most important property).
// A job is in-flight (`sending`) when the DB drops. We then drive BOTH loops' ticks against the
// severed pool and assert:
//
//	(1) both ticks report an OUTAGE (not an application error) and the shared gate halts — the loops
//	    are paused, not spinning;
//	(2) the in-flight job is byte-unchanged: still `sending`, same lease, same attempt_count, NOT
//	    marked sent/failed/bounced/queued, and NOT ONE new job_events row anywhere — the Control
//	    invented nothing;
//	(3) a `queued` job is NOT claimed during the outage (dispatch really did stop claiming);
//	(4) on restore the gate clears on its next probe and the very same loops resume from stored
//	    state — the queued job is claimed+pushed, the in-flight job is STILL `sending` (resume does
//	    not fabricate its outcome either).
//
// Anti-vacuity: (3) is paired with (4) — the same queued job that stayed put during the outage is
// claimed immediately after resume, so the assertion is about the outage, not about a job that could
// never be claimed.
func TestOutage_LoopsPauseAndInventNothing(t *testing.T) {
	e := newDispatchEnv(t, "outagepause")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	// One job in-flight when the outage begins (the job whose fate must NOT be invented).
	inflight := e.seedQueuedJob(t, "inflight@example.com")
	e.dispatchToSending(t, inflight)
	e.px.freeAllWorkers()

	stBefore, pmidBefore, leaseNullBefore, completedBefore, attemptBefore := e.jobState(t, inflight)
	if stBefore != "sending" {
		t.Fatalf("precondition: in-flight job state=%s, want sending", stBefore)
	}
	eventsBefore := e.eventCount(t, inflight)

	// A second job left queued: the dispatch loop must not claim it while the DB is down.
	queued := e.seedQueuedJob(t, "queued@example.com")

	// Build a dispatcher whose pool runs through the injector. Everything else is the real thing.
	proxy, proxyDSN := startPGProxy(t, e.moduleDSN)
	ppool := proxiedPool(t, proxyDSN)
	d := NewDispatcher(ppool, e.dec, e.px)

	// Sanity: through the proxy, while UP, the loops work normally (so a later "no writes" assertion
	// is about the outage, not about a dispatcher that never worked).
	if _, _, err := d.ReclaimTick(ctx); err != nil {
		t.Fatalf("precondition: ReclaimTick through the live proxy failed: %v", err)
	}
	if d.Health().Halted() {
		t.Fatal("precondition: gate halted while the DB is reachable")
	}

	// ---- THE OUTAGE ----
	proxy.cut()

	_, _, dispErr := d.Tick(ctx)
	if dispErr == nil {
		t.Fatal("dispatch Tick SUCCEEDED with the DB severed — the outage was not injected")
	}
	if !IsOutage(dispErr) {
		t.Fatalf("dispatch Tick error classified as application, not outage: %v", dispErr)
	}
	if !d.Health().Note(dispErr) {
		t.Fatal("gate did not treat the dispatch failure as an outage")
	}
	if !d.Health().Halted() {
		t.Fatal("gate is not halted after an outage was noted — loops would keep hammering a dead socket")
	}

	_, _, reclErr := d.ReclaimTick(ctx)
	if reclErr == nil {
		t.Fatal("reclaim ReclaimTick SUCCEEDED with the DB severed — the reclaim loop did not pause")
	}
	if !IsOutage(reclErr) {
		t.Fatalf("reclaim error classified as application, not outage: %v", reclErr)
	}

	// While halted, Ready() must refuse to run work and hand out a bounded backoff (this is what
	// "paused" means: one cheap probe per tick, not a whole tick of failing queries).
	ok, wait := d.Health().Ready(ctx)
	if ok {
		t.Fatal("Ready() said go while the DB is severed")
	}
	if wait <= 0 || wait > outageBackoffMax {
		t.Fatalf("backoff %s out of bounds (0, %s]", wait, outageBackoffMax)
	}

	// (2) NOTHING was invented: the in-flight job is byte-identical, and no event was written.
	// Read via the ADMIN connection, which does not go through the proxy — so this asserts the real
	// stored state, not a cached view.
	stAfter, pmidAfter, leaseNullAfter, completedAfter, attemptAfter := e.jobState(t, inflight)
	if stAfter != "sending" {
		t.Fatalf("IN-FLIGHT JOB WAS MARKED %q DURING THE OUTAGE — the Control invented an outcome (§2.2 violated)", stAfter)
	}
	if pmidAfter != pmidBefore || leaseNullAfter != leaseNullBefore || completedAfter != completedBefore || attemptAfter != attemptBefore {
		t.Fatalf("in-flight job mutated during the outage: pmid %v->%v leaseNull %v->%v completed %v->%v attempt %d->%d",
			pmidBefore, pmidAfter, leaseNullBefore, leaseNullAfter, completedBefore, completedAfter, attemptBefore, attemptAfter)
	}
	if got := e.eventCount(t, inflight); got != eventsBefore {
		t.Fatalf("outage wrote %d job_events row(s) for the in-flight job, want 0", got-eventsBefore)
	}
	// (3) the queued job was NOT claimed.
	if st, _, _, _, _ := e.jobState(t, queued); st != "queued" {
		t.Fatalf("queued job became %q during the outage — dispatch did not stop claiming", st)
	}

	// ---- RECOVERY ----
	proxy.restore()

	// The gate clears on its next probe (no restart, no reconciliation pass).
	deadline := time.Now().Add(20 * time.Second)
	for {
		if ok, _ := d.Health().Ready(ctx); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("gate never cleared after the DB returned")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if d.Health().Halted() {
		t.Fatal("gate still halted after a successful probe")
	}

	// (4) The loops resume from STORED state: the previously-untouched queued job is now claimed and
	// pushed (anti-vacuity for (3)), while the in-flight job is STILL sending — resume invents no
	// outcome for it either; only a real worker report (or the §2.4 reclaim) may move it.
	pushesBefore := e.px.pushCount()
	if _, _, err := d.Tick(ctx); err != nil {
		t.Fatalf("dispatch Tick after recovery failed: %v", err)
	}
	if st, _, _, _, _ := e.jobState(t, queued); st != "sending" && st != "assigned" {
		t.Fatalf("queued job state=%q after recovery, want assigned/sending (the loop did not resume)", st)
	}
	if e.px.pushCount() <= pushesBefore {
		t.Fatal("no push after recovery — dispatch did not resume from stored state")
	}
	if st, _, _, _, _ := e.jobState(t, inflight); st != "sending" {
		t.Fatalf("in-flight job became %q on resume, want still sending (no invented outcome on recovery)", st)
	}
}

// --- T2's persist-ack half: the outage outcome is retained, then persists exactly once ----------

// TestOutage_ReportDuringOutageNotAckedThenReplaysOnce (§2.3 + §2.4 step 2 — the no-duplicate proof
// for the outage path). A worker completes its send DURING the outage (SMTP is independent of the DB).
// Its report reaches the Control, which CANNOT persist it:
//
//	(1) Handle returns resolved=false ⇒ NO report_ack ⇒ the worker retains the outcome (P7-4);
//	(2) the job stays exactly `sending` — not sent, not failed, no event, no send_logs row;
//	(3) after recovery the worker REPLAYS the identical report: it now persists, Handle returns true
//	    (the ack the worker needs to drop it), the job is `sent` with the worker's OWN
//	    provider_message_id, and there is EXACTLY ONE send_logs row — no double application;
//	(4) a further replay after that is a clean idempotent no-op (still one send_logs row).
func TestOutage_ReportDuringOutageNotAckedThenReplaysOnce(t *testing.T) {
	e := newDispatchEnv(t, "outagereport")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")

	job := e.seedQueuedJob(t, "outage-send@example.com")
	e.dispatchToSending(t, job)
	e.px.freeAllWorkers()
	eventsBefore := e.eventCount(t, job)

	proxy, proxyDSN := startPGProxy(t, e.moduleDSN)
	ppool := proxiedPool(t, proxyDSN)
	health := NewDBHealth(ppool)
	oh := NewOutcomeHandler(ppool, e.cfg).WithHealth(health)

	// The real outcome the worker produced (a genuine 250 with a provider id) — the worker holds this in
	// its P7-4 buffer and will keep replaying it until acked.
	report := workerproto.Report{
		Type: workerproto.TypeReport, JobID: job, Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "outage-pmid-0001", SMTPResponse: "250 2.0.0 OK queued as outage-pmid-0001",
	}

	// ---- THE OUTAGE ----
	proxy.cut()

	if resolved := oh.Handle(e.px.workerID, report); resolved {
		t.Fatal("Handle ACKED an outcome it could not persist — the worker would drop it and the outcome would be LOST (Rule 5 violated)")
	}
	if !health.Halted() {
		t.Fatal("a report that failed on transport did not trip the shared gate (loops would not pause)")
	}
	// (2) the job is untouched: still sending, no pmid, no event, no send_log.
	if st, hasPMID, _, _, _ := e.jobState(t, job); st != "sending" || hasPMID {
		t.Fatalf("job state=%q hasPMID=%v during the outage, want sending/false (nothing invented)", st, hasPMID)
	}
	if got := e.eventCount(t, job); got != eventsBefore {
		t.Fatalf("outage report wrote %d job_events row(s), want 0", got-eventsBefore)
	}
	if got := e.sendLogCount(t, job); got != 0 {
		t.Fatalf("outage report wrote %d send_logs row(s), want 0", got)
	}

	// ---- RECOVERY: the worker replays the SAME buffered outcome ----
	proxy.restore()

	var resolved bool
	deadline := time.Now().Add(20 * time.Second)
	for {
		resolved = oh.Handle(e.px.workerID, report)
		if resolved {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replayed outcome never persisted after the DB returned")
		}
		time.Sleep(200 * time.Millisecond)
	}
	// (3) it resolved with the WORKER's real outcome — not a Control-invented one.
	st, hasPMID, leaseNull, completedSet, _ := e.jobState(t, job)
	if st != "sent" || !hasPMID || !leaseNull || !completedSet {
		t.Fatalf("after replay: state=%q hasPMID=%v leaseNull=%v completed=%v, want sent/true/true/true", st, hasPMID, leaseNull, completedSet)
	}
	var pmid string
	if err := e.admin.QueryRow(ctx, `SELECT provider_message_id FROM send_managed.jobs WHERE id=$1`, job).Scan(&pmid); err != nil {
		t.Fatalf("read pmid: %v", err)
	}
	if pmid != report.ProviderMessageID {
		t.Fatalf("provider_message_id=%q, want the worker's own %q (the Control must not substitute one)", pmid, report.ProviderMessageID)
	}
	if got := e.sendLogCount(t, job); got != 1 {
		t.Fatalf("send_logs rows=%d after the replay, want exactly 1 (no double application)", got)
	}
	if health.Halted() {
		t.Fatal("gate still halted after a successful commit")
	}

	// (4) one more replay (a worker that missed the ack) stays a clean no-op.
	if !oh.Handle(e.px.workerID, report) {
		t.Fatal("second replay was not acked — the worker would replay forever")
	}
	if got := e.sendLogCount(t, job); got != 1 {
		t.Fatalf("send_logs rows=%d after a second replay, want still 1 (idempotent)", got)
	}
}

// --- §2.2 "no crash-loop": the real Runner survives a sustained outage and resumes ---------------

// TestOutage_RunnersSurviveSustainedOutageAndResume (§2.2 no-crash-loop + §2.4 resume). The REAL
// dispatch Runner and reclaim ReclaimRunner loops are started against the severed pool and left to
// run through a sustained outage. Assert:
//
//	(1) neither loop goroutine exits (no crash-loop, no panic escaping the tick) while the DB is down;
//	(2) the retry cadence is BACKED OFF, not a tight spin — the gate's advertised wait grows past the
//	    base, and the number of connection attempts the injector observes over the outage stays far
//	    below a tight-spin count;
//	(3) NOTHING is written during the outage (the queued job stays queued);
//	(4) when the DB returns, the SAME still-running loops resume and claim the job — recovery does
//	    not require a restart.
func TestOutage_RunnersSurviveSustainedOutageAndResume(t *testing.T) {
	e := newDispatchEnv(t, "outageloops")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	// Fast ticks so the outage window covers many loop iterations (config-not-code: the real seeded
	// key, set for the test — exactly how the Phase 4/6 loop tests drive the cadence).
	e.setConfigInt(t, "dispatch_tick_interval_ms", 50)
	e.setConfigInt(t, "reclaim_tick_interval_ms", 50)

	queued := e.seedQueuedJob(t, "loops@example.com")

	proxy, proxyDSN := startPGProxy(t, e.moduleDSN)
	ppool := proxiedPool(t, proxyDSN)
	d := NewDispatcher(ppool, e.dec, e.px)

	runner := NewRunner(d)
	reclaimer := NewReclaimRunner(d)
	loopCtx, cancelLoops := context.WithCancel(ctx)
	defer cancelLoops()

	dispatchDone := make(chan struct{})
	reclaimDone := make(chan struct{})

	// ---- start the loops with the DB ALREADY severed (the harshest case: they must not die on their
	// very first tick either) ----
	proxy.cut()
	go func() { defer close(dispatchDone); runner.Run(loopCtx) }()
	go func() { defer close(reclaimDone); reclaimer.Run(loopCtx) }()

	// Let the outage persist across many tick intervals.
	const outageFor = 3 * time.Second
	time.Sleep(outageFor)

	// (1) both loops are STILL RUNNING (they did not exit/crash).
	select {
	case <-dispatchDone:
		t.Fatal("dispatch Runner EXITED during the outage — that is the crash-loop §2.2 forbids")
	case <-reclaimDone:
		t.Fatal("reclaim Runner EXITED during the outage — that is the crash-loop §2.2 forbids")
	default:
	}
	if !d.Health().Halted() {
		t.Fatal("gate not halted after a sustained outage across many ticks")
	}
	// (2) backed off, not spinning. With a 50ms tick, a tight spin over 3s would be ~60 attempts per
	// loop; the bounded ladder must keep the real count far below that.
	if got := d.Health().RetryIn(); got <= outageBackoffBase {
		t.Fatalf("retry wait %s did not grow beyond the base %s — the loops are spinning, not backing off", got, outageBackoffBase)
	}
	const tightSpin = int64(2 * (outageFor / (50 * time.Millisecond))) // both loops, one dial per tick
	if dials := proxy.dials.Load(); dials >= tightSpin {
		t.Fatalf("injector saw %d connection attempts over %s — that is a tight spin (tight-spin bound %d), not bounded backoff", dials, outageFor, tightSpin)
	}
	// (3) nothing was written.
	if st, _, _, _, _ := e.jobState(t, queued); st != "queued" {
		t.Fatalf("queued job became %q while the DB was down — a paused loop wrote state", st)
	}

	// ---- RECOVERY: the same running loops must pick up ----
	proxy.restore()
	// Generous deadline: the loops re-probe on the BOUNDED BACKOFF ladder (capped at
	// outageBackoffMax), so recovery is detected within one backoff step, not instantly — that
	// latency is the no-crash-loop mechanism working, not a failure.
	deadline := time.Now().Add(2*outageBackoffMax + 15*time.Second)
	for {
		st, _, _, _, _ := e.jobState(t, queued)
		if st == "assigned" || st == "sending" {
			break // (4) resumed without any restart
		}
		if time.Now().After(deadline) {
			t.Fatalf("job still %q long after the DB returned — the loops did not resume", st)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if d.Health().Halted() {
		t.Fatal("gate still halted after the loops resumed working")
	}

	// Clean shutdown of the loops (also proves ctx cancellation still ends them normally).
	cancelLoops()
	for _, ch := range []chan struct{}{dispatchDone, reclaimDone} {
		select {
		case <-ch:
		case <-time.After(10 * time.Second):
			t.Fatal("a loop did not exit after ctx cancel")
		}
	}
}

// --- a CONFIG fault is not a DB outage (review gate R2 finding 4) --------------------------------

// logCapture is a concurrency-safe sink for the standard logger, so a test can assert on what the
// REAL loops logged. Needed because the loops log from their own goroutines.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *logCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *logCapture) count(sub string) int {
	return strings.Count(c.String(), sub)
}

// captureLogs redirects the standard logger into a capture sink for the duration of one test.
func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	c := &logCapture{}
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(io.MultiWriter(prevOut, c))
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return c
}

// TestConfigOrdering_IsNotADBOutage (§2.1 classification × §5.3). A config-ordering violation and a DB
// outage are DIFFERENT faults with different correct responses, and the §2.1 gate must not confuse
// them. The DB is perfectly healthy here: it answered the config read completely and the Control then
// REFUSED to act on an unsafe pair (Rule 1 — reject, never clamp). That is an application-level fault:
// log it and retry on the normal cadence, with every loop still correctly declining to act.
//
// Misclassifying it as transport unavailability is not cosmetic. `Note` would trip the halt and log
// "db outage DETECTED" naming a DB that is up; the very next tick's `Ready` probe (`SELECT 1`) would
// succeed against that healthy DB and `clear()` it — which RESETS `failures` to 0. So the §2.2 bounded
// exponential ladder could never grow past its base and all three loops would flap DETECTED/RESOLVED
// at 500ms forever, burying the one log line an operator needs (the offending config pair) under noise
// about an outage that is not happening.
//
// Real infra throughout: a real Postgres clone, a real violating row in `send_managed.config`, the REAL
// dispatch Runner goroutine, and the real standard logger captured verbatim. Paired controls:
//   - NEGATIVE (the behavior under test): a config violation must NOT halt the gate and must log NO
//     outage line;
//   - POSITIVE (proves the assertion can fail): a real severed connection on the same gate DOES halt
//     and DOES log the outage line;
//   - ANTI-VACUITY: the loop that claimed nothing during the violation claims immediately once the
//     config pair is fixed — so the non-claim was the reject, not a broken fixture.
func TestConfigOrdering_IsNotADBOutage(t *testing.T) {
	e := newDispatchEnv(t, "cfgnotoutage")
	ctx := context.Background()
	pool := e.seedPool(t)
	e.seedAccount(t, pool, "sec")
	queued := e.seedQueuedJob(t, "cfgfault@example.com")

	// A REAL violating pair (buffer TTL 900 < window 901) → a real *configOrderingError from LoadConfig.
	e.setConfigInt(t, "startup_reconnect_window_sec", 901)
	_, cfgErr := LoadConfig(ctx, e.pool)
	if cfgErr == nil {
		t.Fatal("precondition: LoadConfig accepted the violating pair — cannot test the classification")
	}
	var ordErr *configOrderingError
	if !errors.As(cfgErr, &ordErr) {
		t.Fatalf("precondition: wanted a *configOrderingError, got %T (%v)", cfgErr, cfgErr)
	}

	// (a) CLASSIFICATION. Not transport unavailability.
	if IsOutage(cfgErr) {
		t.Fatalf("a config-ordering violation was classified as a DB OUTAGE (%v) — the DB answered fine; this is a configuration fault, and calling it an outage flaps the §2.2 backoff at its base forever", cfgErr)
	}
	h := NewDBHealth(e.pool)
	if h.Note(cfgErr) || h.Halted() {
		t.Fatal("the gate halted on a config-ordering violation — the loops would report a DB outage for a healthy DB")
	}
	if got := h.RetryIn(); got != 0 {
		t.Fatalf("gate advertises a %s outage backoff after a config fault, want 0 (it is not halted)", got)
	}

	// (b) THE REAL LOOP, over ~3 ticks of the violation. Fast tick so several land in the window.
	e.setConfigInt(t, "dispatch_tick_interval_ms", 50)
	logs := captureLogs(t)
	runner := NewRunner(e.d)
	loopCtx, cancelLoop := context.WithCancel(ctx)
	loopDone := make(chan struct{})
	go func() { defer close(loopDone); runner.Run(loopCtx) }()
	time.Sleep(2500 * time.Millisecond)

	// The loop is alive and did notice the fault repeatedly (anti-vacuity for the log assertions below:
	// if the loop had died or never ticked, "no outage line" would be trivially true).
	select {
	case <-loopDone:
		cancelLoop()
		t.Fatal("the dispatch Runner EXITED on a config-ordering violation — a bad config must not kill the loop")
	default:
	}
	if n := logs.count("config_ordering_violation"); n < 2 {
		cancelLoop()
		<-loopDone
		t.Fatalf("the loop logged the config violation %d time(s) over 2.5s, want >=2 — it is not actually retrying against the violating pair, so this test would be vacuous.\nlog:\n%s", n, logs.String())
	}
	// THE POINT: no outage was ever declared for a healthy DB, and therefore no DETECTED/RESOLVED flap.
	if n := logs.count("db outage DETECTED"); n != 0 {
		cancelLoop()
		<-loopDone
		t.Fatalf("the loop logged %d \"db outage DETECTED\" line(s) while the DB was HEALTHY and only the config was unsafe — a config fault must never be reported as transport unavailability.\nlog:\n%s", n, logs.String())
	}
	if n := logs.count("db outage RESOLVED"); n != 0 {
		cancelLoop()
		<-loopDone
		t.Fatalf("the loop logged %d \"db outage RESOLVED\" line(s) — the DETECTED/RESOLVED flap that pins the §2.2 backoff at its base.\nlog:\n%s", n, logs.String())
	}
	if e.d.Health().Halted() {
		cancelLoop()
		<-loopDone
		t.Fatal("the shared gate is HALTED because of a config fault — dispatch, reclaim AND retention would all be paused by a mislabeled outage")
	}
	// Nothing was written: a loop that cannot load a safe config claims nothing (§2 invent-nothing).
	if st, _, leaseNull, _, attempt := e.jobState(t, queued); st != "queued" || !leaseNull || attempt != 0 {
		cancelLoop()
		<-loopDone
		t.Fatalf("job is state=%s leaseNull=%v attempt=%d during a config-ordering violation, want queued/true/0 — nothing may be claimed on unsafe values", st, leaseNull, attempt)
	}

	// (c) ANTI-VACUITY: fix the pair and the SAME still-running loop claims the job immediately.
	e.setConfigInt(t, "startup_reconnect_window_sec", 120)
	deadline := time.Now().Add(15 * time.Second)
	for {
		st, _, _, _, _ := e.jobState(t, queued)
		if st == "assigned" || st == "sending" {
			break
		}
		if time.Now().After(deadline) {
			cancelLoop()
			<-loopDone
			t.Fatalf("job still %q after the config pair was fixed — the non-claim above was not caused by the reject (vacuous test)", st)
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancelLoop()
	select {
	case <-loopDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop did not exit after ctx cancel")
	}

	// (d) POSITIVE CONTROL for the log assertions: a REAL outage on a REAL severed connection DOES halt
	// and DOES log the DETECTED line — so "no outage line" above is a real observation, not a logger
	// that was never wired up.
	proxy, proxyDSN := startPGProxy(t, e.moduleDSN)
	ppool := proxiedPool(t, proxyDSN)
	proxy.cut()
	var one int
	transportErr := ppool.QueryRow(ctx, `SELECT 1`).Scan(&one)
	if transportErr == nil {
		t.Fatal("precondition: SELECT 1 succeeded on a severed connection")
	}
	before := logs.count("db outage DETECTED")
	h2 := NewDBHealth(ppool)
	if !h2.Note(transportErr) || !h2.Halted() {
		t.Fatal("control: the gate did not halt on a REAL transport error")
	}
	if got := logs.count("db outage DETECTED"); got != before+1 {
		t.Fatalf("control: a REAL outage logged %d DETECTED line(s) (was %d) — the capture is not observing the logger, so the zero-count assertions above prove nothing", got, before)
	}
}

// --- the detection boundary: transport vs application (paired positive/negative control) --------

// TestIsOutage_TransportVsApplicationError (§2.1). The gate MUST distinguish "the DB is unreachable"
// (pause everything) from "a reachable server rejected this statement" (handle inline). Both errors
// here are REAL errors from the REAL driver:
//   - NEGATIVE control: a genuine CHECK violation (23514) raised by real Postgres over a live
//     connection is NOT an outage — if it were, one bad row would pause the whole Control;
//   - POSITIVE control: a genuine query against a severed connection IS an outage.
//
// Also asserts the backoff ladder is bounded (grows, then caps) — the "no crash-loop" mechanism.
func TestIsOutage_TransportVsApplicationError(t *testing.T) {
	e := newDispatchEnv(t, "outageclassify")
	ctx := context.Background()

	// NEGATIVE control — a real 23514 from a real server. jobs_attempt_count_check bounds
	// attempt_count <= max_attempts; violate it deliberately on a real row.
	job := e.seedQueuedJob(t, "classify@example.com")
	_, appErr := e.pool.Exec(ctx, `UPDATE send_managed.jobs SET attempt_count = max_attempts + 5 WHERE id=$1`, job)
	if appErr == nil {
		t.Fatal("precondition: the CHECK did not fire — cannot test the application-error branch")
	}
	var pgErr *pgconn.PgError
	if !errors.As(appErr, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("precondition: wanted a real 23514 PgError, got %v", appErr)
	}
	if IsOutage(appErr) {
		t.Fatalf("a real CHECK violation (%s) was classified as an OUTAGE — one bad row would pause the whole Control", pgErr.Code)
	}
	// A gate told about it must NOT halt.
	h := NewDBHealth(e.pool)
	if h.Note(appErr) || h.Halted() {
		t.Fatal("gate halted on an application error (loops would pause for a CHECK violation)")
	}

	// POSITIVE control — a real query on a severed connection.
	proxy, proxyDSN := startPGProxy(t, e.moduleDSN)
	ppool := proxiedPool(t, proxyDSN)
	proxy.cut()
	var one int
	transportErr := ppool.QueryRow(ctx, `SELECT 1`).Scan(&one)
	if transportErr == nil {
		t.Fatal("precondition: SELECT 1 succeeded on a severed connection")
	}
	if !IsOutage(transportErr) {
		t.Fatalf("a real severed-connection error was NOT classified as an outage: %v", transportErr)
	}
	h2 := NewDBHealth(ppool)
	if !h2.Note(transportErr) || !h2.Halted() {
		t.Fatal("gate did not halt on a real transport error")
	}
	// context.Canceled is deliberate shutdown (Chunk 5), never an outage.
	if IsOutage(context.Canceled) {
		t.Fatal("context.Canceled classified as an outage — a graceful shutdown would look like a DB failure")
	}

	// Bounded backoff: strictly increasing from the base, then pinned at the cap — never unbounded,
	// never zero (a zero would be the tight spin §2.2 forbids).
	prev := time.Duration(0)
	for n := 1; n <= 40; n++ {
		got := backoffFor(n)
		if got <= 0 || got > outageBackoffMax {
			t.Fatalf("backoffFor(%d)=%s out of bounds (0, %s]", n, got, outageBackoffMax)
		}
		if got < prev {
			t.Fatalf("backoffFor(%d)=%s decreased from %s", n, got, prev)
		}
		prev = got
	}
	if backoffFor(40) != outageBackoffMax {
		t.Fatalf("backoff did not reach the cap after 40 failures: %s", backoffFor(40))
	}
}
