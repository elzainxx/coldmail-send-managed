// Command control is the Phase-0 SKELETON of the coldmail-send-managed orchestrator ("the Control").
//
// Scope (Phase 0 only — NO feature logic): start; acquire the single-active-Control
// advisory lock (a second Control exits cleanly); connect to Postgres as send_managed_app;
// load the dev KMS key (Control is the SOLE decryptor); serve a health stub; serve an
// mTLS worker-link that lets a worker register (id+token) and heartbeat. There is deliberately
// NO dispatch loop, NO routing, NO SMTP — those are Phase 1+.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-send-managed/control/internal/adminapi"
	"github.com/coldmail-send-managed/control/internal/workerlink"
	"github.com/coldmail-send-managed/control/internal/config"
	"github.com/coldmail-send-managed/control/internal/credential"
	"github.com/coldmail-send-managed/control/internal/db"
	"github.com/coldmail-send-managed/control/internal/dispatch"
	"github.com/coldmail-send-managed/control/internal/httpapi"
	"github.com/coldmail-send-managed/control/internal/kms"
	"github.com/coldmail-send-managed/control/internal/platformapi"
	"github.com/coldmail-send-managed/control/internal/singleton"
)

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("control ")

	cfg, err := config.FromEnv()
	if err != nil {
		log.Fatalf("FATAL config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// DB connection as the app role (send_managed_app) — never a superuser at runtime.
	database, err := db.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("FATAL db connect: %v", err)
	}
	defer database.Close()
	log.Printf("db connected")

	// Singleton guard: exactly one active Control. Bounded-wait-then-fail. A second Control
	// that cannot acquire the session-level advisory lock exits non-zero and does nothing.
	// Uses a DEDICATED standalone connection (its own DSN), independent of the pool.
	//
	// brain_singleton_acquire_timeout_sec (Phase 9.5 B5, §5.2, seeded send_managed_0021) is config-not-code
	// (CLAUDE.md Rule 1) — the env var CONTROL_SINGLETON_ACQUIRE_TIMEOUT_SEC is ONLY the Phase-0
	// bootstrap default (OD-18: before send_managed.config existed) and is used ONLY as the fallback when
	// the DB read fails (e.g. a genuinely pre-migration database — the runner hasn't applied
	// send_managed_0021 yet). The already-connected pool can read the live value BEFORE the singleton
	// lock is attempted; a bad/missing row degrades to the env default rather than blocking startup
	// on a config problem the singleton guard itself has nothing to do with.
	acquireTimeout := readSingletonAcquireTimeout(ctx, database.Pool, cfg.SingletonAcquireTimeout)
	guard, err := singleton.Acquire(ctx, cfg.DatabaseURL, acquireTimeout)
	if err != nil {
		if errors.Is(err, singleton.ErrNotAcquired) {
			// This is the correct, clean exit for a second Control — not a crash.
			log.Printf("singleton lock not acquired within %s: another Control is active; exiting cleanly", acquireTimeout)
			database.Close()
			os.Exit(3)
		}
		log.Fatalf("FATAL singleton acquire: %v", err)
	}
	defer guard.Release(context.Background())
	log.Printf("singleton lock acquired (key=%d)", singleton.LockKey)

	// G11-OD-1 / ADR-0011 (option B, ruled 2026-07-29): install the bounded re-acquire cadence for a
	// MID-LIFE lock loss. A dropped lock connection (DB restart, failover, pg_terminate_backend,
	// connection reaper) no longer leaves the keeper dead — it pauses dispatch (see SetLeadership
	// below) and re-attempts on this cadence. Config-not-code (Rule 1); 0/unreadable = never
	// re-acquire, which leaves the Control paused-and-honest rather than resuming on a guessed cadence.
	reacquireEvery := readSingletonReacquireInterval(ctx, database.Pool)
	guard.SetReacquireInterval(reacquireEvery)
	if reacquireEvery > 0 {
		log.Printf("singleton: mid-life lock loss will PAUSE dispatch/reclaim/retention and re-attempt acquisition every %s (brain_singleton_reacquire_interval_sec)", reacquireEvery)
	} else {
		log.Printf("singleton: mid-life re-acquisition DISABLED (brain_singleton_reacquire_interval_sec=0 or unreadable) — on lock loss this Control pauses dispatch and stays paused until restarted")
	}

	// Control is the SOLE decryptor: load the dev KMS key path. The worker holds NO KMS key
	// and cannot reach this material (enforced by container mounts + a harness test).
	decryptor, err := kms.LoadDev(cfg.KMSDevKeyFile)
	if err != nil {
		log.Fatalf("FATAL kms load: %v", err)
	}
	log.Printf("kms dev key loaded (sole decryptor; %d bytes of key material held in-process, never logged)", decryptor.KeyLen())

	// Admin API (Domain B, operator — Phase 2). Mounted under /v1/admin/* on the health server.
	// It authenticates callers with a hashed operator bearer key and is the ONLY writer of
	// account credentials — Encrypt-ONLY (OD-7: Decrypt stays Control-only, the send path). The
	// surface only mounts when an admin key hash is configured (fail-closed: no key => no admin
	// surface). The credential Encrypt key material is loaded via a DELIBERATELY separate KMS
	// entry point (never the decryptor) so the encrypt-only property is structural.
	var adminHandler http.Handler
	if cfg.AdminEnabled {
		encKey, err := kms.LoadEncryptKey(cfg.KMSDevKeyFile)
		if err != nil {
			log.Fatalf("FATAL admin credential key load: %v", err)
		}
		enc, err := adminapi.NewEncryptor(encKey, cfg.CredentialKeyID)
		if err != nil {
			log.Fatalf("FATAL admin encryptor init: %v", err)
		}
		auth := adminapi.NewAuthenticator(cfg.AdminKeyHashes)
		adminSrv := adminapi.NewServer(database.Pool, enc, auth)
		adminHandler = adminSrv.Handler()
		log.Printf("admin API mounted on %s under /v1/admin/* (operator-key auth; Encrypt-only; %d key(s) configured)", cfg.HealthAddr, len(cfg.AdminKeyHashes))
	} else {
		log.Printf("admin API NOT mounted (ADMIN_API_KEY_HASHES unset — operator surface disabled, fail-closed)")
	}

	// --- Phase 3: the minimal send path (dispatch + credential handoff + outcome handler) -------
	// The credential DECRYPTOR is the sole decryptor (Rule 6). It is loaded via a DELIBERATELY
	// separate KMS entry point and wired ONLY into the dispatcher — NEVER into the admin surface
	// (OD-7, structural: the admin Encryptor has no Decrypt; this Decryptor is not injected there).
	decKey, err := kms.LoadDecryptKey(cfg.KMSDevKeyFile)
	if err != nil {
		log.Fatalf("FATAL send-path decrypt key load: %v", err)
	}
	dec, err := credential.NewDecryptor(decKey, cfg.CredentialKeyID)
	if err != nil {
		log.Fatalf("FATAL credential decryptor init: %v", err)
	}
	// Zero the raw key bytes now that the Decryptor holds its own copy.
	for i := range decKey {
		decKey[i] = 0
	}

	sendCfg, err := dispatch.LoadConfig(ctx, database.Pool)
	if err != nil {
		log.Fatalf("FATAL send-path config load: %v", err)
	}

	// The persistent worker↔Control send stream (§2.1). The outcome handler is its ReportHandler.
	outcome := dispatch.NewOutcomeHandler(database.Pool, sendCfg)
	stream, err := workerlink.NewStreamServer(cfg.WorkerlinkStreamAddr, cfg.TLS, database.Pool, outcome.Handle)
	if err != nil {
		log.Fatalf("FATAL workerlink stream init: %v", err)
	}

	// The dispatcher pushes over the stream; a Runner drives it off a KICK signal (event-driven,
	// NOT a polling loop — Phase 4). A worker coming online kicks it too (a queued job may be waiting).
	dispatcher := dispatch.NewDispatcher(database.Pool, dec, stream)

	// Phase 7 (§2, P7-3): the Supabase-down gate. The dispatcher owns the ONE shared DBHealth so the
	// dispatch loop, the reclaim loop and the retention sweep all pause on a single detection; the
	// outcome handler shares it as an OBSERVER only — it always attempts a report (a commit is what
	// ends an outage) and merely reports what the DB did, so a failure there also pauses the loops
	// and a success resumes them promptly. The Control writes NO job state while the DB is unreachable;
	// it stays alive on bounded backoff and never crash-loops.
	outcome.WithHealth(dispatcher.Health())

	// G11-OD-1 / ADR-0011 (option B): the LEADERSHIP gate. Until this line existed, guard was passed
	// ONLY to the health server, so held=false was visible on readyz and acted on NOWHERE — a Control
	// that lost the lock kept claiming and pushing (observed: 56 minutes, while a second Control held
	// the lock and came fully up). Now the same three loops that pause on a DB outage also pause on
	// lost leadership.
	//
	// DELIBERATELY NOT GATED: the outcome path. A worker reporting an in-flight send must always be
	// persisted, even by a Control that no longer holds the lock — that job was pushed while this Control
	// WAS the leader, the worker really did run the SMTP session, and refusing the report would either
	// invent a non-outcome (Rule 5) or force a needless duplicate send later. Persisting it is
	// idempotent (provider_message_id UNIQUE + the pre-persist check), so a successor Control reaching
	// the same row cannot double-count it. Same reasoning for the mTLS listeners and the admin/
	// platform read surfaces: none of them CLAIM work.
	dispatcher.SetLeadership(guard.Healthy)

	// Phase 7 (§1): the one-time STARTUP RESCAN — reconnect-reconciliation (P7-2). It runs ONCE,
	// BEFORE the dispatch/reclaim loops start, so there is no window in which both it and Detector B
	// act on the same job (§1.4). It extends orphaned in-flight leases by startup_reconnect_window_sec
	// (EVENT-FREE — no state change, no job_events, no account lock, cannot deadlock; the Phase 6
	// lesson) and refreshes worker heartbeats, opening a bounded window for reconnecting workers to replay
	// buffered outcomes with no duplicate. It issues NO reclaim itself — actual reclaim stays the
	// continuous loop's job via the shared §2.4 transition. A transient failure here does not wedge
	// boot (the continuous loop is the backstop, §1.4).
	dispatcher.RunStartupRescan(ctx)

	runner := dispatch.NewRunner(dispatcher)
	stream.SetOnRegister(func(string) { runner.Kick() })
	go runner.Run(ctx)

	// Phase 6 (§2): the continuous reclaim loop — a THIRD single-Control polling loop that recovers
	// jobs stranded on dead workers (heartbeat scan) or hung on live ones (per-job lease backstop),
	// applying the locked reclaim transition. Ticks every reclaim_tick_interval_ms; no Kick (a
	// periodic sweep, not event-driven).
	reclaimer := dispatch.NewReclaimRunner(dispatcher)
	go reclaimer.Run(ctx)

	// Phase 7 (§3.1): the retention sweep — a FOURTH single-Control housekeeping loop that DELETEs raw
	// send_managed.send_logs content past debug_log_retention_days, in batches of retention_delete_batch_size,
	// every retention_sweep_interval_ms. It drops ONLY raw content: job OUTCOMES (job_events + final job
	// states) are permanent and are never rolled up or deleted, and the per-day summaries are DERIVED ON
	// DEMAND from job_events (P7-1, no summary table). It shares the §2 outage gate, so a DB outage
	// pauses it too — retention is a ceiling on raw-content lifetime, not a deadline, so a delayed sweep
	// is correct. Deliberately in-Control rather than pg_cron (P7-7: one owner of send_managed.* DML).
	retention := dispatch.NewRetentionRunner(dispatcher)
	go retention.Run(ctx)
	go func() {
		if err := stream.Serve(); err != nil {
			log.Printf("workerlink stream server stopped: %v", err)
		}
	}()
	log.Printf("workerlink send-stream mTLS server on %s (client cert required; credential channel)", cfg.WorkerlinkStreamAddr)

	// Platform API (Domain A, Phase 3). Service-token auth (distinct credential from the operator
	// admin key). run kicks the dispatcher asynchronously; it never blocks on the send.
	var platformHandler http.Handler
	if cfg.PlatformEnabled {
		pauth := platformapi.NewAuthenticator(cfg.PlatformTokenHashes)
		psrv := platformapi.NewServer(database.Pool, pauth, runner.Kick)
		platformHandler = psrv.Handler()
		log.Printf("platform API mounted under /v1/{quote,run,report,describe} (service-token auth; %d token(s) configured)", len(cfg.PlatformTokenHashes))
	} else {
		log.Printf("platform API NOT mounted (PLATFORM_TOKEN_HASHES unset — Domain A surface disabled, fail-closed)")
	}

	// Health stub (plain HTTP; TLS termination is a deployment concern — §4.5). Reports true
	// state. The admin surface (/v1/admin/*) and platform surface (/v1/{quote,run,report,describe})
	// mount on the same server when their credentials are configured.
	health := httpapi.NewHealthServer(cfg.HealthAddr, database, guard, adminHandler, platformHandler)
	go func() {
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("FATAL health server: %v", err)
		}
	}()
	log.Printf("health server on %s", cfg.HealthAddr)

	// Worker-link: the Phase-0 mTLS HTTP register/heartbeat routes (still served, additive-only). The
	// Phase-3 persistent send stream is the separate listener above.
	link, err := workerlink.NewServer(cfg.WorkerlinkAddr, cfg.TLS, cfg.WorkerRegistry)
	if err != nil {
		log.Fatalf("FATAL workerlink init: %v", err)
	}
	go func() {
		if err := link.ListenAndServeTLS(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("FATAL workerlink server: %v", err)
		}
	}()
	log.Printf("workerlink mTLS server on %s (client cert required)", cfg.WorkerlinkAddr)

	log.Printf("control ready")
	<-ctx.Done()
	log.Printf("shutdown signal received")

	// --- Phase 7 §4 (P7-5): GRACEFUL SHUTDOWN — stop-claim → drain → leave-in-place -------------
	//
	// STEP 1 (stop-claim). The dispatcher enters the drain: it claims no new job and pushes nothing,
	// so no fresh work enters flight behind us. The signal ctx is already cancelled at this point, so
	// the dispatch/reclaim/retention loop goroutines are exiting between ticks (never mid-transaction:
	// each tick's DB work runs to commit or rolls back on its own bounded context).
	//
	// STEP 2 (drain). The worker-link stream and the outcome handler stay UP for the whole drain window,
	// so workers finishing in-flight sends can still REPORT and have their outcomes persisted and acked
	// (Chunk 2's persist-ack). Each job that reports resolves normally — sent/bounced/requeued — which
	// is the clean terminal for it. The wait is bounded by the seeded shutdown_drain_timeout_sec
	// (config-not-code; NOT the old hardcoded 10s), and ends early the moment in-flight hits zero.
	//
	// STEP 3 (leave-in-place). Whatever is still in-flight at the timeout is LEFT EXACTLY AS STORED —
	// `sending`/`assigned` with its lease. We deliberately do NOT flip sending→queued: that job may
	// already have hit 250 at the provider, so an eager requeue would manufacture a duplicate (§4.2).
	// The next boot's startup rescan extends those leases so a reconnecting worker can replay its buffered
	// outcome with no duplicate, and the continuous loop reclaims the genuinely-stuck (§4.3). A SIGKILL
	// that preempts all of this reduces to the same path — nothing was going to be written anyway.
	drainTimeout := time.Duration(sendCfg.ShutdownDrainTimeoutSec) * time.Second
	// Re-read the key so an operator's change since boot is honored; fall back to the boot value if the
	// DB is unreachable (a shutdown must never block on a sick DB).
	if freshCfg, err := dispatch.LoadConfig(context.Background(), database.Pool); err == nil {
		drainTimeout = time.Duration(freshCfg.ShutdownDrainTimeoutSec) * time.Second
	}
	// The drain gets its own context, NOT the cancelled signal ctx (which would abort it instantly).
	// Bounded a little above the drain window so the drain, not this context, decides when to stop.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), drainTimeout+5*time.Second)
	left := dispatcher.Drain(drainCtx, drainTimeout)
	cancelDrain()
	if left > 0 {
		log.Printf("shutdown: %d job(s) left in flight and RECOVERABLE (not requeued, not marked) — the next boot's rescan resolves them (§4.3)", left)
	}

	// Now the listeners come down. SAFETY BOUND (Phase 4 closeout triage,
	// Docs/history/open-decisions.md): the HTTP/listener close grace is a deployment concern, not a runtime
	// dial — same class as MAX_FALLBACK_HOPS. The DRAIN window above is the operator-tunable part.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = health.Shutdown(shutdownCtx)
	_ = link.Shutdown(shutdownCtx)
	_ = stream.Close()
	log.Printf("shutdown complete (no job was requeued or marked on the way out — §4.2 leave-in-place)")
}

// readSingletonAcquireTimeout reads the live brain_singleton_acquire_timeout_sec (Phase 9.5 B5,
// §5.2) from the already-connected pool, falling back to envDefault (the Phase-0 bootstrap value,
// OD-18) on any read/parse failure — a missing row (pre-send_managed_0021 database) or a transient DB
// blip must not block startup on a config problem the singleton guard itself has nothing to do
// with. This is a minimal, scoped-to-one-key reader rather than a full dispatch.LoadConfig call:
// at this point in startup the send-path config (job_max_processing_time_sec etc.) has not been
// validated yet, and a config-ordering violation elsewhere must not prevent the singleton guard
// itself from running.
func readSingletonAcquireTimeout(ctx context.Context, pool *pgxpool.Pool, envDefault time.Duration) time.Duration {
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key = 'brain_singleton_acquire_timeout_sec'`).Scan(&raw); err != nil {
		return envDefault
	}
	var wrap struct {
		V *int `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil || *wrap.V < 0 {
		return envDefault
	}
	return time.Duration(*wrap.V) * time.Second
}

// readSingletonReacquireInterval reads the live brain_singleton_reacquire_interval_sec (G11-OD-1 /
// ADR-0011 option B, seeded send_managed_0023) — the cadence at which a Control that LOST the lock
// re-attempts it. Same minimal scoped-reader shape and same reasoning as
// readSingletonAcquireTimeout above.
//
// The failure fallback here is 0 (never re-acquire), NOT a hardcoded interval, and that direction is
// deliberate: 0 leaves the Control paused-and-honest (readyz 503, container unhealthy, dispatching
// nothing) until an operator or supervisor acts, whereas guessing a cadence would resume dispatch on
// a schedule nobody configured. Rule 1 says a literal that governs runtime behavior is a bug — a
// literal that DISABLES an optional behavior when its config is unreadable is the fail-closed
// default, which is the same posture PLATFORM_TOKEN_HASHES-empty and alert_webhook_url-empty take.
func readSingletonReacquireInterval(ctx context.Context, pool *pgxpool.Pool) time.Duration {
	var raw []byte
	if err := pool.QueryRow(ctx, `SELECT value FROM send_managed.config WHERE key = 'brain_singleton_reacquire_interval_sec'`).Scan(&raw); err != nil {
		return 0
	}
	var wrap struct {
		V *int `json:"v"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil || wrap.V == nil || *wrap.V < 0 {
		return 0
	}
	return time.Duration(*wrap.V) * time.Second
}
