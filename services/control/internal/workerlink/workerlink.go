// Package workerlink is the Control side of the worker<->Control channel (mTLS). Phase-0 scope: a worker
// REGISTERS (presents id + one-time token) and HEARTBEATS. The token is the authorization
// authority (verified against its stored hash); the client cert is transport auth/
// confidentiality, already verified by the TLS layer (RequireAndVerifyClientCert). There is
// NO job stream / dispatch here — that is Phase 3+.
package workerlink

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/coldmail-send-managed/control/internal/tlsutil"
	"golang.org/x/crypto/bcrypt"
)

// Registry is the Phase-0 worker authorization source: one dev worker id + its token hash (from
// env; Phase 1 moves this to send_managed.boxes.token_hash). token=authority (P9-A).
type Registry struct {
	WorkerID     string
	TokenHash string
}

// authorize reports whether (id, token) matches the registry. Fail-closed: empty registry
// or empty token hash rejects everything. Constant-time on the id; bcrypt on the token.
func (r Registry) authorize(id, token string) bool {
	if r.WorkerID == "" || r.TokenHash == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(id), []byte(r.WorkerID)) != 1 {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(r.TokenHash), []byte(token)) == nil
}

// Server is the mTLS worker-link HTTP server.
type Server struct {
	http     *http.Server
	registry Registry

	mu         sync.Mutex
	lastBeat   map[string]time.Time // worker id -> last heartbeat (in-memory; Phase 0 only)
	registered map[string]bool
}

type registerReq struct {
	WorkerID string `json:"box_id"`
	Token string `json:"token"`
}
type registerResp struct {
	OK bool `json:"ok"`
}
type heartbeatReq struct {
	WorkerID string `json:"box_id"`
}

// NewServer builds the mTLS server. Every route requires a verified client cert (enforced
// by the tls.Config); the token check is a second, independent authorization gate.
func NewServer(addr string, tlsCfg tlsutil.ServerConfig, reg Registry) (*Server, error) {
	tc, err := tlsCfg.Build()
	if err != nil {
		return nil, err
	}
	s := &Server{
		registry:   reg,
		lastBeat:   make(map[string]time.Time),
		registered: make(map[string]bool),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/worker/register", s.handleRegister)
	mux.HandleFunc("/v1/worker/heartbeat", s.handleHeartbeat)
	s.http = &http.Server{
		Addr:              addr,
		Handler:           mux,
		TLSConfig:         tc,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s, nil
}

// ListenAndServeTLS serves over mTLS. Cert/key already live in TLSConfig.
func (s *Server) ListenAndServeTLS() error {
	return s.http.ListenAndServeTLS("", "")
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		// Defense in depth; TLS layer already requires a client cert.
		http.Error(w, `{"code":"unauthenticated"}`, http.StatusUnauthorized)
		return
	}
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"code":"bad_request"}`, http.StatusBadRequest)
		return
	}
	if !s.registry.authorize(req.WorkerID, req.Token) {
		// Two-vocabulary rule: no internal reason to the client.
		log.Printf("worker register REJECTED id=%q (token/cert mismatch)", req.WorkerID)
		http.Error(w, `{"code":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	s.registered[req.WorkerID] = true
	s.lastBeat[req.WorkerID] = time.Now().UTC()
	s.mu.Unlock()
	log.Printf("worker registered id=%q (token verified, client cert CN=%s)", req.WorkerID, r.TLS.PeerCertificates[0].Subject.CommonName)
	writeJSON(w, http.StatusOK, registerResp{OK: true})
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req heartbeatReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"code":"bad_request"}`, http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	reg := s.registered[req.WorkerID]
	if reg {
		s.lastBeat[req.WorkerID] = time.Now().UTC()
	}
	s.mu.Unlock()
	if !reg {
		http.Error(w, `{"code":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// LastHeartbeat exposes the in-memory last-beat for tests/health.
func (s *Server) LastHeartbeat(workerID string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.lastBeat[workerID]
	return t, ok
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
