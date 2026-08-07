// Package db is the ONLY place in the Control that talks to Postgres. The worker has no
// equivalent — structurally, the worker service ships no DB driver (CLAUDE.md Rule 6).
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/coldmail-send-managed/control/internal/dsnscrub"
)

// DB wraps the connection pool.
type DB struct {
	Pool *pgxpool.Pool
}

// Connect opens a pgx pool as the app role encoded in the DSN (send_managed_app — never a
// superuser at runtime). It pings to prove reachability before returning.
func Connect(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// N1/D12: MEASURED at the pinned pgx v5.7.4 — a rejected connection string is echoed WHOLE
		// into this error, and pgx's own redaction misses ?password=, ?sslpassword= and the honored
		// spaced `password = SECRET`. main.go logs this via log.Fatalf("FATAL db connect: %v", err),
		// so an unscrubbed value lands in the boot log. See internal/dsnscrub for the measurement.
		return nil, fmt.Errorf("parse dsn: %w", dsnscrub.Err(err, dsn))
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		// Lazy at this pin (it does not dial), but that is an implementation detail, not a contract:
		// scrubbed so a pgx change cannot open this path without anyone noticing.
		return nil, fmt.Errorf("new pool: %w", dsnscrub.Err(err, dsn))
	}

	// SAFETY BOUND (Phase 4 closeout triage, Docs/history/open-decisions.md): a one-time startup
	// reachability check, not a runtime behavior dial.
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		// Clean at this pin — a connect-stage failure names user and database, not the DSN — and the
		// dsnscrub tests pin that this case keeps its full detail. Wrapped anyway for the same reason
		// as above: the scrub is a no-op on a clean message, so the only thing it costs is nothing.
		return nil, fmt.Errorf("ping: %w", dsnscrub.Err(err, dsn))
	}
	return &DB{Pool: pool}, nil
}

// Close releases the pool.
func (d *DB) Close() {
	if d != nil && d.Pool != nil {
		d.Pool.Close()
	}
}
