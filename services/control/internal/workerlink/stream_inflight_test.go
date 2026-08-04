package workerlink

// Phase 7 review gate R3 — the one-in-flight marker vs a REPLAYED report (§2.5 × §1.3/§2.3).
//
// Phase 7 turned `report` from a once-per-job frame into a REPEATABLE one: the worker replays every
// unacked outcome on each heartbeat tick and on each reconnect. dispatchFrame used to clear the worker's
// in-flight marker on ANY report, so a replay for an OLD job cleared the marker of the NEW job the worker
// was actually running — the dispatcher then saw the worker as idle and pushed a SECOND job (with a
// SECOND decrypted credential) to a worker already mid-SMTP. That breaks one-job-per-worker (§2.5) and, since
// the worker's #runJob is fire-and-forget, it genuinely runs two sessions holding two credentials.
//
// This exercises the REAL StreamServer state machine (real workerConn, real dispatchFrame, real PushJob,
// real AnyConnectedWorker) over a real in-process socket pair. No DB is needed: the marker lives entirely
// in memory and the frames under test (report) take no DB path when onReport is the identity handler.

import (
	"encoding/json"
	"net"
	"testing"

	"github.com/coldmail-send-managed/control/internal/workerproto"
)

// newMarkerServer builds a StreamServer with ONE registered worker, bypassing only the TLS listener and
// the register handshake (both proven by the live-stack mTLS suite): everything the test asserts on —
// the workers map, workerConn.inFlight, dispatchFrame, PushJob, AnyConnectedWorker — is the production code.
func newMarkerServer(t *testing.T, workerID string, onReport ReportHandler) (*StreamServer, net.Conn) {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	// Drain whatever the server writes so a frame write never blocks on the pipe.
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := client.Read(buf); err != nil {
				return
			}
		}
	}()
	s := &StreamServer{
		onReport: onReport,
		workers:    make(map[string]*workerConn),
		closed:   make(chan struct{}),
	}
	s.workers[workerID] = &workerConn{conn: server, enc: json.NewEncoder(server), workerID: workerID}
	return s, server
}

func frame(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	return b
}

// TestInFlightMarker_ReplayedReportDoesNotFreeALiveJob is the regression test for R3's HIGH finding.
//
// Sequence, exactly as production produces it: job-OLD's outcome went unacked (DB outage) and is
// buffered on the worker; the outage ends; the Control pushes job-NEW to that same worker; mid-SMTP the worker's
// heartbeat tick replays job-OLD's report. Asserts the replay does NOT make the worker push-eligible.
//
// Paired positive control: the report for the job the worker is ACTUALLY holding DOES clear the marker, so
// the guard cannot pass by simply never clearing (which would wedge every worker after its first job).
func TestInFlightMarker_ReplayedReportDoesNotFreeALiveJob(t *testing.T) {
	const workerID = "worker-marker"
	var reported []string
	s, _ := newMarkerServer(t, workerID, func(_ string, rep workerproto.Report) bool {
		reported = append(reported, rep.JobID)
		return true // persisted (the ack path is proven elsewhere; irrelevant to the marker)
	})

	// The worker is holding job-NEW (the real push path sets the marker).
	if err := s.PushJob(workerproto.PushJob{Type: workerproto.TypePushJob, RequestID: "req-new", JobID: "job-NEW"}, workerID); err != nil {
		t.Fatalf("push job-NEW: %v", err)
	}
	if got := s.AnyConnectedWorker(); got != "" {
		t.Fatalf("AnyConnectedWorker=%q while the worker holds job-NEW, want \"\" (one-in-flight, §2.5)", got)
	}

	// *** THE REPLAY ***: a buffered outcome for a DIFFERENT, older job arrives on the live stream.
	s.dispatchFrame(workerID, frame(t, workerproto.Report{
		Type: workerproto.TypeReport, RequestID: "req-old", JobID: "job-OLD", Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "pmid-old", SMTPResponse: "250 2.0.0 OK",
	}))

	// The replay must still reach the outcome handler (it is a real outcome that may need persisting —
	// dropping it would lose it, and the worker would replay forever).
	if len(reported) != 1 || reported[0] != "job-OLD" {
		t.Fatalf("replayed report reached the handler as %v, want [job-OLD] — the marker guard must not swallow the outcome", reported)
	}
	// ...but it must NOT have freed the live job's marker.
	s.mu.Lock()
	held := s.workers[workerID].inFlight
	s.mu.Unlock()
	if held != "job-NEW" {
		t.Fatalf("in-flight marker is %q after a replayed report for job-OLD, want job-NEW — a stale replay freed a LIVE job's slot", held)
	}
	if got := s.AnyConnectedWorker(); got != "" {
		t.Fatalf("AnyConnectedWorker=%q after a replayed report for an old job — the dispatcher now believes a worker mid-SMTP is idle", got)
	}
	// The concrete harm, asserted directly: a SECOND push (a second plaintext credential) would succeed.
	if err := s.PushJob(workerproto.PushJob{Type: workerproto.TypePushJob, RequestID: "req-2nd", JobID: "job-SECOND"}, workerID); err == nil {
		t.Fatal("PushJob ACCEPTED a second concurrent job after a replayed report — two jobs and TWO plaintext credentials on one worker at once (§2.5 broken)")
	}

	// ---- POSITIVE CONTROL: the report for the job actually held DOES clear the marker. ----
	s.dispatchFrame(workerID, frame(t, workerproto.Report{
		Type: workerproto.TypeReport, RequestID: "req-new", JobID: "job-NEW", Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "pmid-new", SMTPResponse: "250 2.0.0 OK",
	}))
	s.mu.Lock()
	held = s.workers[workerID].inFlight
	s.mu.Unlock()
	if held != "" {
		t.Fatalf("in-flight marker is %q after the OWNING job's report, want \"\" — the worker would never be pushed to again", held)
	}
	if got := s.AnyConnectedWorker(); got != workerID {
		t.Fatalf("AnyConnectedWorker=%q after the owning report, want %q — the worker must become eligible again", got, workerID)
	}
	if err := s.PushJob(workerproto.PushJob{Type: workerproto.TypePushJob, RequestID: "req-3rd", JobID: "job-THIRD"}, workerID); err != nil {
		t.Fatalf("PushJob refused the next job after a clean completion: %v", err)
	}
}

// TestInFlightMarker_ReplayOnAnIdleWorkerIsHarmless covers the other replay shape: the worker finished its
// job, was acked-less, and replays while IDLE. The marker is already "" and must stay "" — the worker
// remains eligible (a replay must not wedge a healthy idle worker out of the dispatch pool).
func TestInFlightMarker_ReplayOnAnIdleWorkerIsHarmless(t *testing.T) {
	const workerID = "worker-idle"
	s, _ := newMarkerServer(t, workerID, func(_ string, _ workerproto.Report) bool { return true })

	if got := s.AnyConnectedWorker(); got != workerID {
		t.Fatalf("precondition: AnyConnectedWorker=%q, want %q (an idle registered worker is eligible)", got, workerID)
	}
	s.dispatchFrame(workerID, frame(t, workerproto.Report{
		Type: workerproto.TypeReport, RequestID: "req-old", JobID: "job-OLD", Outcome: workerproto.OutcomeAccepted,
		ProviderMessageID: "pmid-old", SMTPResponse: "250 2.0.0 OK",
	}))
	s.mu.Lock()
	held := s.workers[workerID].inFlight
	s.mu.Unlock()
	if held != "" {
		t.Fatalf("in-flight marker became %q from a replay on an IDLE worker — the worker is now permanently ineligible", held)
	}
	if got := s.AnyConnectedWorker(); got != workerID {
		t.Fatalf("AnyConnectedWorker=%q after an idle-worker replay, want %q", got, workerID)
	}
}
