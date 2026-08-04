// stream.go — the Phase-3 persistent worker↔Control control channel (§2.1). A worker dials this mTLS
// listener and holds ONE long-lived stream carrying register → register_ack, heartbeat,
// push_job (Control→worker), and report (worker→Control) as newline-delimited JSON frames. This is the
// channel the credential travels on, so it is the strictest transport in the system: mutual TLS
// (RequireAndVerifyClientCert on the listener; the worker verifies the Control's cert on its side),
// token-gated registration bound to a send_managed.boxes row, and stream-close on any auth failure —
// NO job or credential is ever written to an unregistered/unauthenticated stream (§3.1/§3.3, S2).
//
// Phase 3 is one-job-in-flight-per-worker (§2.5). The StreamServer tracks each registered worker's
// live connection so the dispatcher can push to it; a report is routed to a handler the
// dispatcher installs. There is NO background scan/reclaim here (that is Phase 6): the server
// only ever updates last_heartbeat_at on register/heartbeat and marks the worker's job reclaimable
// via its lease if the stream drops (the dispatcher owns that transition).
package workerlink

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/coldmail-send-managed/control/internal/tlsutil"
	"github.com/coldmail-send-managed/control/internal/workerproto"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// ReportHandler is invoked for each worker `report` frame. The dispatcher installs it; it must not
// block the read loop for long (it does a bounded DB transaction). It is called with the worker id
// that produced the report so the outcome handler can attribute provenance.
//
// It RETURNS whether the outcome is durably resolved (Phase 7 §1.3, P7-4): true = committed to the
// DB (or a clean idempotent no-op the Control owns) ⇒ the stream server sends a report_ack so the worker
// may DROP its buffered copy; false = a real error prevented persistence ⇒ NO ack, the worker retains
// and replays later. This is the persist-BEFORE-ack contract: the ack is emitted only after the
// handler confirms durability, never optimistically.
type ReportHandler func(workerID string, rep workerproto.Report) (resolved bool)

// StreamServer accepts worker connections on the mTLS workerlink stream port.
type StreamServer struct {
	ln         net.Listener
	db         *pgxpool.Pool
	onReport   ReportHandler
	onRegister func(workerID string) // optional: kick the dispatcher when a worker comes online

	mu      sync.Mutex
	workers map[string]*workerConn // worker id -> its live connection (one in-flight job per worker)

	closed chan struct{}
}

// SetOnRegister installs a callback fired after a worker successfully registers. The dispatcher uses
// it to kick a dispatch attempt when a worker comes online (event-driven — NOT a background timer),
// so a job queued before any worker connected is picked up on connect. Must not block.
func (s *StreamServer) SetOnRegister(fn func(workerID string)) { s.onRegister = fn }

// workerConn is one registered worker's held stream. writes are serialized by wmu so a push_job frame
// is never interleaved with an ack.
type workerConn struct {
	conn     net.Conn
	enc      *json.Encoder
	wmu      sync.Mutex
	workerID string
	inFlight string // job id currently pushed to this worker ("" = idle); guarded by StreamServer.mu
}

// NewStreamServer builds the stream listener. db is the send_managed_app pool (token verification +
// heartbeat updates). It does not start accepting until Serve is called.
func NewStreamServer(addr string, tlsCfg tlsutil.ServerConfig, db *pgxpool.Pool, onReport ReportHandler) (*StreamServer, error) {
	tc, err := tlsCfg.Build()
	if err != nil {
		return nil, err
	}
	ln, err := tls.Listen("tcp", addr, tc)
	if err != nil {
		return nil, fmt.Errorf("workerlink stream listen %s: %w", addr, err)
	}
	return &StreamServer{
		ln:       ln,
		db:       db,
		onReport: onReport,
		workers:  make(map[string]*workerConn),
		closed:   make(chan struct{}),
	}, nil
}

// Addr returns the bound address (useful when addr was ":0" in tests).
func (s *StreamServer) Addr() net.Addr { return s.ln.Addr() }

// Serve accepts connections until Close. Each connection is handled in its own goroutine.
func (s *StreamServer) Serve() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return nil
			default:
				return err
			}
		}
		go s.handle(conn)
	}
}

// Close stops accepting and drops all held connections.
func (s *StreamServer) Close() error {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
	err := s.ln.Close()
	s.mu.Lock()
	for _, bc := range s.workers {
		_ = bc.conn.Close()
	}
	s.workers = make(map[string]*workerConn)
	s.mu.Unlock()
	return err
}

// handle runs one worker connection: TLS handshake (implicit on first read), then the register
// handshake, then the read loop. On ANY auth failure the stream is closed and no work flows.
func (s *StreamServer) handle(conn net.Conn) {
	defer conn.Close()

	// SAFETY BOUND, not a config-not-code violation (Phase 4 closeout triage, Docs/history/open-decisions.md):
	// bounds an UNAUTHENTICATED peer's register handshake so a silent connection cannot hold a slot
	// forever. The register protocol has no legitimate variable-length case; this guards against
	// abuse, not a real workload an operator would ever want to tune.
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	br := bufio.NewReader(conn)
	enc := json.NewEncoder(conn)

	// First frame MUST be register (§3.1).
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	var reg workerproto.Register
	if err := json.Unmarshal(line, &reg); err != nil || reg.Type != workerproto.TypeRegister {
		_ = enc.Encode(workerproto.RegisterAck{Type: workerproto.TypeRegisterAck, RequestID: reg.RequestID, Accepted: false, Code: "bad_request"})
		return
	}

	if !s.authorizeWorker(reg.WorkerID, reg.Token) {
		// Two-vocabulary rule: the worker learns only the clean code; the reason is logged internally.
		log.Printf("workerlink stream: register REJECTED id=%q (token/cert mismatch) — stream closed, no job/credential pushed", reg.WorkerID)
		_ = enc.Encode(workerproto.RegisterAck{Type: workerproto.TypeRegisterAck, RequestID: reg.RequestID, Accepted: false, Code: "unauthenticated"})
		return
	}

	// Upsert-by-identity (§3.1/P3-4): status→active, last_heartbeat_at=NOW(), merge box_metadata.
	if err := s.markActive(reg); err != nil {
		log.Printf("workerlink stream: register accepted but DB update failed for id=%q: %v — closing", reg.WorkerID, err)
		_ = enc.Encode(workerproto.RegisterAck{Type: workerproto.TypeRegisterAck, RequestID: reg.RequestID, Accepted: false, Code: "internal_error"})
		return
	}

	bc := &workerConn{conn: conn, enc: enc, workerID: reg.WorkerID}
	// Register the live connection (replacing a stale one for the same id — a reconnect).
	s.mu.Lock()
	if old := s.workers[reg.WorkerID]; old != nil && old.conn != conn {
		_ = old.conn.Close()
	}
	s.workers[reg.WorkerID] = bc
	s.mu.Unlock()
	defer s.dropConn(reg.WorkerID, bc)

	// Deliver the seeded box_outcome_buffer_ttl_sec on accept (Phase 7 §1.3, P7-4): the DB-less worker
	// consumes it for its outcome-buffer bound (config-not-code). A read failure is non-fatal — the
	// worker falls back to its bootstrap default; we never block registration on it.
	ttlSec := s.readOutcomeBufferTTLSec()
	if err := s.writeFrame(bc, workerproto.RegisterAck{Type: workerproto.TypeRegisterAck, RequestID: reg.RequestID, Accepted: true, OutcomeBufferTTLSec: ttlSec}); err != nil {
		return
	}
	log.Printf("workerlink stream: worker registered id=%q (token verified, mTLS client cert)", reg.WorkerID)
	if s.onRegister != nil {
		go s.onRegister(reg.WorkerID) // kick the dispatcher (a queued job may be waiting for a worker)
	}

	// Read loop: heartbeats + reports. No further register expected. Clear the handshake deadline
	// (a healthy held stream is the liveness signal; idle is fine, heartbeats refresh the row).
	_ = conn.SetReadDeadline(time.Time{})
	for {
		lineBytes, err := br.ReadBytes('\n')
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("workerlink stream: worker id=%q stream closed (%v)", reg.WorkerID, err)
			}
			return
		}
		s.dispatchFrame(reg.WorkerID, lineBytes)
	}
}

// dispatchFrame decodes one inbound frame and routes it (heartbeat | report).
func (s *StreamServer) dispatchFrame(workerID string, line []byte) {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(line, &probe) != nil {
		return // ignore garbage frames; never crash the loop
	}
	switch probe.Type {
	case workerproto.TypeHeartbeat:
		if err := s.touchHeartbeat(workerID); err != nil {
			log.Printf("workerlink stream: heartbeat DB update failed for id=%q: %v", workerID, err)
		}
	case workerproto.TypeReport:
		var rep workerproto.Report
		if json.Unmarshal(line, &rep) != nil {
			return
		}
		// Clear the in-flight marker ONLY when this report is for the job the worker is actually holding.
		// Phase 7 §1.3/§2.3 made a `report` REPEATABLE and no longer necessarily about the current job:
		// an unacked outcome is replayed on every heartbeat tick and on every reconnect, so a replay for
		// an OLD job can arrive while the worker is mid-SMTP on a NEW one. Clearing unconditionally there
		// would tell the dispatcher the worker is idle (AnyConnectedWorker) and let it push a SECOND job to a
		// worker already running one — breaking one-job-in-flight-per-worker (§2.5) and putting TWO plaintext
		// credentials on one worker at once. A mismatched report leaves the live marker alone; the marker is
		// per-connection, so a genuinely stale one is cleared by the reconnect that replaces workerConn.
		s.mu.Lock()
		bc := s.workers[workerID]
		if bc != nil && bc.inFlight == rep.JobID {
			bc.inFlight = ""
		}
		s.mu.Unlock()
		if s.onReport != nil {
			// PERSIST, then ACK (Phase 7 §1.3, P7-4). onReport does the bounded DB transaction and
			// returns whether the outcome is durably resolved. We send report_ack ONLY when it is —
			// so the worker drops its buffered copy only after a real DB commit; on a non-resolved result
			// (DB error) we send NO ack and the worker retains + replays. The ack carries job_id so the worker
			// can match it across a reconnect (where request_ids differ). A best-effort write: if the
			// stream just dropped, the worker will replay on reconnect and be acked then.
			if resolved := s.onReport(workerID, rep); resolved && bc != nil {
				_ = s.writeFrame(bc, workerproto.ReportAck{
					Type: workerproto.TypeReportAck, RequestID: rep.RequestID, JobID: rep.JobID, Accepted: true,
				})
			}
		}
	default:
		// Unknown/`register`-after-handshake: ignore (defensive; never leak, never crash).
	}
}

// PushJob writes a push_job frame to the given worker on its held stream (§2.3). It fails if the worker
// is not currently connected/registered or already has a job in flight (one-in-flight, §2.5) — the
// dispatcher treats either as "cannot push now" and leaves the job claimable. The push is the
// point at which the credential leaves the Control; it is only ever written to a registered stream.
func (s *StreamServer) PushJob(pj workerproto.PushJob, workerID string) error {
	s.mu.Lock()
	bc := s.workers[workerID]
	if bc == nil {
		s.mu.Unlock()
		return fmt.Errorf("worker %q not connected", workerID)
	}
	if bc.inFlight != "" {
		s.mu.Unlock()
		return fmt.Errorf("worker %q already has job %q in flight", workerID, bc.inFlight)
	}
	bc.inFlight = pj.JobID
	s.mu.Unlock()

	if err := s.writeFrame(bc, pj); err != nil {
		// Failed to hand off: clear the marker so a later attempt can retry.
		s.mu.Lock()
		if bc2 := s.workers[workerID]; bc2 == bc {
			bc.inFlight = ""
		}
		s.mu.Unlock()
		return err
	}
	return nil
}

// Connected reports whether a worker currently holds a registered stream (used by the dispatcher to
// pick a worker and by tests).
func (s *StreamServer) Connected(workerID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	bc := s.workers[workerID]
	return bc != nil
}

// AnyConnectedWorker returns the id of some currently-connected worker, or "" if none. Phase 3's trivial
// dispatcher uses this to find a worker to push to (workers are interchangeable — §2.2).
func (s *StreamServer) AnyConnectedWorker() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, bc := range s.workers {
		if bc.inFlight == "" {
			return id
		}
	}
	return ""
}

func (s *StreamServer) dropConn(workerID string, bc *workerConn) {
	s.mu.Lock()
	if cur := s.workers[workerID]; cur == bc {
		delete(s.workers, workerID)
	}
	s.mu.Unlock()
}

// writeFrame serializes one frame to the worker, serialized per-connection. A short write deadline
// stops a wedged peer from blocking the pusher forever. SAFETY BOUND (Phase 4 closeout triage): a
// wire-write timeout on an already-authenticated connection, not a tunable send-behavior dial.
func (s *StreamServer) writeFrame(bc *workerConn, v any) error {
	bc.wmu.Lock()
	defer bc.wmu.Unlock()
	_ = bc.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := bc.enc.Encode(v); err != nil {
		return err
	}
	_ = bc.conn.SetWriteDeadline(time.Time{})
	return nil
}

// authorizeWorker verifies (id, token) against send_managed.boxes: the token is the authorization
// AUTHORITY (bcrypt vs token_hash), the client cert is transport (already verified by TLS). A
// missing/disabled worker, an empty token_hash (decommissioned), or a token mismatch → reject.
func (s *StreamServer) authorizeWorker(workerID, token string) bool {
	if workerID == "" || token == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var hash, status string
	err := s.db.QueryRow(ctx,
		`SELECT token_hash, status::text FROM send_managed.boxes WHERE id = $1`, workerID).Scan(&hash, &status)
	if err != nil {
		return false // unknown worker id (or DB error): fail closed
	}
	if status == "disabled" || hash == "" {
		return false // decommissioned: refused on next connect regardless of cert
	}
	// The id is a PK lookup; bcrypt.CompareHashAndPassword is constant-time against the stored
	// hash, so a wrong token for a real worker id does not leak timing.
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(token)) == nil
}

// markActive upserts the worker's liveness on register (§3.1 step 3). box_metadata is merged from
// the optional diagnostics fields (§3.5); nothing here is read by dispatch.
func (s *StreamServer) markActive(reg workerproto.Register) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	meta := workerMetadata(reg)
	_, err := s.db.Exec(ctx,
		`UPDATE send_managed.boxes
		   SET status = 'active',
		       last_heartbeat_at = NOW(),
		       box_metadata = COALESCE($2, box_metadata)
		 WHERE id = $1`, reg.WorkerID, meta)
	return err
}

// readOutcomeBufferTTLSec reads the seeded box_outcome_buffer_ttl_sec ({"v":N}) to deliver to the
// DB-less worker on register_ack (Phase 7 §1.3, P7-4 — config-not-code). Returns 0 on any read/parse
// failure; the worker then falls back to its bootstrap default (never blocks registration).
func (s *StreamServer) readOutcomeBufferTTLSec() int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var raw []byte
	if err := s.db.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key='box_outcome_buffer_ttl_sec'`).Scan(&raw); err != nil {
		log.Printf("workerlink stream: box_outcome_buffer_ttl_sec unreadable (%v) — worker uses its bootstrap default", err)
		return 0
	}
	var wrap struct {
		V *int `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil {
		return 0
	}
	return *wrap.V
}

// touchHeartbeat refreshes last_heartbeat_at and returns a stale worker to active (§3.2).
func (s *StreamServer) touchHeartbeat(workerID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.db.Exec(ctx,
		`UPDATE send_managed.boxes
		   SET last_heartbeat_at = NOW(),
		       status = CASE WHEN status = 'stale' THEN 'active'::send_managed.box_status ELSE status END
		 WHERE id = $1`, workerID)
	return err
}

// workerMetadata builds the optional diagnostics JSON (§3.5) or returns nil if the worker sent none.
func workerMetadata(reg workerproto.Register) []byte {
	if reg.WorkerVersion == "" && reg.Hostname == "" && reg.StartedAt == "" {
		return nil
	}
	m := map[string]string{}
	if reg.WorkerVersion != "" {
		m["box_version"] = reg.WorkerVersion
	}
	if reg.Hostname != "" {
		m["hostname"] = reg.Hostname
	}
	if reg.StartedAt != "" {
		m["started_at"] = reg.StartedAt
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return b
}
