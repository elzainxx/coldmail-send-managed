// Command send-managed-migrate is the thin, self-tracking migration runner (ADR-0003, OD-4).
//
// It keeps the LOCKED convention: every send_managed_NNNN_*.up.sql self-records into
// send_managed.schema_migrations via its own INSERT, and the .down.sql DELETEs it. This runner
// does NOT write the tracking row — it only ORDERS and APPLIES pending files. It does not
// adopt golang-migrate/dbmate (which would require editing locked migration bodies).
//
// Config-not-code exception (CLAUDE.md Rule 1): the runner's own lock_timeout /
// statement_timeout are CLI-flag defaults because send_managed.config does not exist until the
// bootstrap migration creates the schema. This exception is the runner only.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const (
	// Advisory lock held only for the duration of an up/down/status run. Distinct from the
	// Control singleton key (9_200_000_001). Registered in docs/architecture.md#advisory-lock-key-registry.
	migrateAdvisoryLockID   int64 = 9_100_000_001
	defaultLockTimeout            = "10s"
	defaultStatementTimeout       = "300s"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dsn := firstNonEmpty(os.Getenv("MIGRATE_DATABASE_URL"), os.Getenv("DATABASE_URL"))
	if dsn == "" {
		fatal("MIGRATE_DATABASE_URL (or DATABASE_URL) is required")
	}
	dir := firstNonEmpty(os.Getenv("MIGRATIONS_DIR"), "db/migrations")
	preflightPath := firstNonEmpty(os.Getenv("PREFLIGHT_SQL"), "db/preflight/check_platform_tables.sql")

	r := &Runner{
		DSN:              dsn,
		Dir:              dir,
		PreflightPath:    preflightPath,
		LockTimeout:      firstNonEmpty(os.Getenv("MIGRATE_LOCK_TIMEOUT"), defaultLockTimeout),
		StatementTimeout: firstNonEmpty(os.Getenv("MIGRATE_STATEMENT_TIMEOUT"), defaultStatementTimeout),
		AllowDown:        os.Getenv("SEND_MANAGED_MIGRATE_ALLOW_DOWN") == "true",
		SkipPreflight:    os.Getenv("MIGRATE_SKIP_PREFLIGHT") == "true",
	}

	var err error
	switch cmd {
	case "up":
		err = r.Up(ctx)
	case "down":
		if len(os.Args) < 3 {
			fatal("down requires a version, e.g. send-managed-migrate down 0001")
		}
		err = r.Down(ctx, os.Args[2])
	case "status":
		err = r.Status(ctx)
	case "set-app-password":
		// Sets send_managed_app's password out-of-band from SEND_MANAGED_MODULE_PASSWORD. Never logs
		// the value. This keeps role passwords OUT of migration SQL (CLAUDE.md Rule 6).
		err = r.SetAppPassword(ctx, os.Getenv("SEND_MANAGED_MODULE_PASSWORD"))
	case "up-and-set-password":
		// Convenience for the compose one-shot: apply the chain, then set the app password.
		if err = r.Up(ctx); err == nil {
			err = r.SetAppPassword(ctx, os.Getenv("SEND_MANAGED_MODULE_PASSWORD"))
		}
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		if errors.Is(err, errDownForbidden) {
			fatal("down is forbidden unless SEND_MANAGED_MIGRATE_ALLOW_DOWN=true (forward-only in prod)")
		}
		fatal(err.Error())
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `send-managed-migrate — thin self-tracking migration runner

usage:
  send-managed-migrate up                 apply all pending send_managed_NNNN_*.up.sql in order
  send-managed-migrate down <NNNN>        roll back exactly one version (dev only; needs
                                    SEND_MANAGED_MIGRATE_ALLOW_DOWN=true)
  send-managed-migrate status             show applied vs pending

env:
  MIGRATE_DATABASE_URL / DATABASE_URL   (required) admin/owner-capable DSN (OD-1)
  MIGRATIONS_DIR                        default db/migrations
  PREFLIGHT_SQL                         default db/preflight/check_platform_tables.sql
  MIGRATE_SKIP_PREFLIGHT=true           skip the external-table existence check
  SEND_MANAGED_MIGRATE_ALLOW_DOWN=true        permit down (forward-only in prod otherwise)
  MIGRATE_LOCK_TIMEOUT / _STATEMENT_TIMEOUT   runner bootstrap timeouts (CLI defaults)
`)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func fatal(msg string) {
	fmt.Fprintf(os.Stderr, "send-managed-migrate: FATAL: %s\n", msg)
	os.Exit(1)
}

// timeoutDur parses a Postgres-style duration string like "10s"/"300s" for local use.
func timeoutDur(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return def
}
