// Package db is the ONLY place in the Control that talks to Postgres. The worker has no
// equivalent — structurally, the worker service ships no DB driver (CLAUDE.md Rule 6).
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("new pool: %w", err)
	}

	// SAFETY BOUND (Phase 4 closeout triage, Docs/history/open-decisions.md): a one-time startup
	// reachability check, not a runtime behavior dial.
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &DB{Pool: pool}, nil
}

// Close releases the pool.
func (d *DB) Close() {
	if d != nil && d.Pool != nil {
		d.Pool.Close()
	}
}
