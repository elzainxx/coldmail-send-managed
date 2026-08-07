package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var errDownForbidden = errors.New("down forbidden")

// migrationFile is one on-disk send_managed_NNNN_*.{up,down}.sql pair member.
type migrationFile struct {
	Version      string // "0001"
	Name         string // "bootstrap"
	UpPath       string
	Concurrently bool // contains CREATE/DROP INDEX CONCURRENTLY -> run outside a txn
}

var fileRe = regexp.MustCompile(`^send_managed_(\d{4})_(.+)\.up\.sql$`)
var concurrentlyRe = regexp.MustCompile(`(?i)\b(create|drop)\s+index\s+concurrently\b`)

// Runner applies/rolls back migrations against the DSN.
type Runner struct {
	DSN              string
	Dir              string
	PreflightPath    string
	LockTimeout      string
	StatementTimeout string
	AllowDown        bool
	SkipPreflight    bool
}

func (r *Runner) connect(ctx context.Context) (*pgx.Conn, error) {
	// Bounded connect so an unreachable Postgres fails fast rather than hanging on the OS
	// default TCP timeout (mirrors the Control's pool-ping timeout).
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := pgx.Connect(cctx, r.DSN)
	if err != nil {
		// N1/D12: MEASURED at the pinned pgx v5.7.4 — a rejected connection string is echoed WHOLE
		// into this error, and pgx's own redaction misses ?password=, ?sslpassword= and the honored
		// spaced `password = SECRET`. Every caller of connect() returns the error up to main.go, which
		// hands it to fatal(err.Error()) -> stderr. A migration run is exactly when that output gets
		// pasted into a ticket. See dsnscrub.go for the measurement.
		return nil, fmt.Errorf("connect: %w", scrubDSNError(err, r.DSN))
	}
	return c, nil
}

// acquireLock takes the runner's advisory lock with a bounded wait (retry loop around the
// non-blocking pg_try_advisory_lock), then fails loud. Never blocks forever.
func (r *Runner) acquireLock(ctx context.Context, c *pgx.Conn) error {
	deadline := time.Now().Add(timeoutDur(r.LockTimeout, 10*time.Second))
	for {
		var got bool
		if err := c.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", migrateAdvisoryLockID).Scan(&got); err != nil {
			return fmt.Errorf("advisory lock: %w", err)
		}
		if got {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("could not acquire migration advisory lock within %s (another migrator running?)", r.LockTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (r *Runner) releaseLock(ctx context.Context, c *pgx.Conn) {
	_, _ = c.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrateAdvisoryLockID)
}

// preflight runs the external-table existence check (OD-8) unless skipped.
func (r *Runner) preflight(ctx context.Context, c *pgx.Conn) error {
	if r.SkipPreflight {
		fmt.Println("preflight: SKIPPED (MIGRATE_SKIP_PREFLIGHT=true)")
		return nil
	}
	sqlBytes, err := os.ReadFile(r.PreflightPath)
	if err != nil {
		return fmt.Errorf("read preflight %q: %w", r.PreflightPath, err)
	}
	if _, err := c.Exec(ctx, string(sqlBytes)); err != nil {
		return fmt.Errorf("preflight failed: %w", err)
	}
	fmt.Println("preflight: OK (public.users + public.job_items exist)")
	return nil
}

// appliedVersions reads send_managed.schema_migrations. If the table doesn't exist yet (fresh DB,
// pre-bootstrap), returns an empty set — the only special case, to bootstrap 0000 itself.
func (r *Runner) appliedVersions(ctx context.Context, c *pgx.Conn) (map[string]bool, error) {
	var exists bool
	err := c.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.tables
		WHERE table_schema='send_managed' AND table_name='schema_migrations')`).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("check schema_migrations: %w", err)
	}
	applied := map[string]bool{}
	if !exists {
		return applied, nil
	}
	rows, err := c.Query(ctx, "SELECT version FROM send_managed.schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("select versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

// discover globs send_managed_NNNN_*.up.sql, sorted ascending by version.
func (r *Runner) discover() ([]migrationFile, error) {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir %q: %w", r.Dir, err)
	}
	var files []migrationFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := fileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		body, err := os.ReadFile(filepath.Join(r.Dir, e.Name()))
		if err != nil {
			return nil, err
		}
		files = append(files, migrationFile{
			Version:      m[1],
			Name:         m[2],
			UpPath:       filepath.Join(r.Dir, e.Name()),
			Concurrently: concurrentlyRe.Match(body),
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Version < files[j].Version })
	return files, nil
}

// Up applies all pending migrations in order.
func (r *Runner) Up(ctx context.Context) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())

	if err := r.acquireLock(ctx, c); err != nil {
		return err
	}
	defer r.releaseLock(context.Background(), c)

	if err := r.preflight(ctx, c); err != nil {
		return err
	}

	applied, err := r.appliedVersions(ctx, c)
	if err != nil {
		return err
	}
	files, err := r.discover()
	if err != nil {
		return err
	}

	// Contiguity guard: the chain must be a gap-free ascending sequence. A file NNNN whose
	// predecessor NNNN-1 is neither already applied nor present on disk is a hard error, not
	// a silent skip (Phase 0 spec §5.5). This catches a dropped/renamed migration (e.g. a bad
	// rebase) before it produces a half-applied schema. The chain start is the LOWEST version
	// present (0000 pre-feature bootstrap, or 0001 if there is no bootstrap) — that lowest
	// file needs no predecessor; every file above it does. So 0000 → 0001 is contiguous.
	onDisk := make(map[int]bool, len(files))
	minVer := -1
	for _, f := range files {
		n, _ := strconv.Atoi(f.Version)
		onDisk[n] = true
		if minVer == -1 || n < minVer {
			minVer = n
		}
	}
	for _, f := range files {
		n, _ := strconv.Atoi(f.Version)
		if n > minVer {
			prev := n - 1
			prevVer := fmt.Sprintf("%04d", prev)
			if !applied[prevVer] && !onDisk[prev] {
				return fmt.Errorf("migration chain gap: %s present but predecessor %s is neither applied nor on disk", f.Version, prevVer)
			}
		}
	}

	pending := 0
	for _, f := range files {
		if applied[f.Version] {
			continue
		}
		if err := r.applyOne(ctx, c, f); err != nil {
			return fmt.Errorf("migration %s_%s: %w", f.Version, f.Name, err)
		}
		fmt.Printf("applied  %s_%s\n", f.Version, f.Name)
		pending++
	}
	if pending == 0 {
		fmt.Println("up: no pending migrations (already current)")
	} else {
		fmt.Printf("up: applied %d migration(s)\n", pending)
	}
	return nil
}

// applyOne runs a single up file. Transactional by default; non-transactional if it
// contains CONCURRENTLY (the file must then be idempotent + carry its own bookkeeping last).
func (r *Runner) applyOne(ctx context.Context, c *pgx.Conn, f migrationFile) error {
	body, err := os.ReadFile(f.UpPath)
	if err != nil {
		return err
	}
	sqlText := string(body)

	if f.Concurrently {
		// Non-transactional: CREATE/DROP INDEX CONCURRENTLY cannot run inside a transaction block,
		// AND a multi-statement simple query is itself wrapped in an implicit transaction by
		// Postgres — so we must send each statement as its OWN Exec (auto-commit), never the whole
		// file at once. Statements run sequentially; the file's own bookkeeping INSERT/DELETE is
		// last, so a mid-file failure leaves the version UN-recorded (safe: a re-run retries, and
		// the DDL is written IF (NOT) EXISTS to be idempotent).
		//
		// SET (not SET LOCAL) persists on the session, so RESET afterward to avoid leaking this
		// timeout onto any subsequent file applied on the same connection in this run.
		if _, err := c.Exec(ctx, fmt.Sprintf("SET statement_timeout = '%s'", r.StatementTimeout)); err != nil {
			return err
		}
		var execErr error
		for _, stmt := range splitStatements(sqlText) {
			if _, execErr = c.Exec(ctx, stmt); execErr != nil {
				break
			}
		}
		_, _ = c.Exec(ctx, "RESET statement_timeout")
		return execErr
	}

	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%s'", r.LockTimeout)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = '%s'", r.StatementTimeout)); err != nil {
		return err
	}
	// The file includes its OWN INSERT INTO send_managed.schema_migrations — we do not add one.
	if _, err := tx.Exec(ctx, sqlText); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Down rolls back exactly one version (dev only). It enforces reverse-order safety: a version
// N is refused while any HIGHER version is still recorded applied. Without this, `down 0000`
// while the feature chain 0001..NN is applied would run the bootstrap's `DROP SCHEMA ... CASCADE`
// and silently destroy every higher-numbered object without their own down files ever running
// (a real footgun, since down takes an arbitrary single version). Roll back highest-first.
func (r *Runner) Down(ctx context.Context, version string) error {
	if !r.AllowDown {
		return errDownForbidden
	}
	version = normalizeVersion(version)
	downPath, err := r.downPathFor(version)
	if err != nil {
		return err
	}

	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	if err := r.acquireLock(ctx, c); err != nil {
		return err
	}
	defer r.releaseLock(context.Background(), c)

	// Reverse-order guard: refuse if a higher applied version exists (it would be orphaned, and
	// for the bootstrap 0000 its CASCADE would destroy the higher objects outright).
	applied, err := r.appliedVersions(ctx, c)
	if err != nil {
		return err
	}
	target, _ := strconv.Atoi(version)
	for v := range applied {
		if n, e := strconv.Atoi(v); e == nil && n > target {
			return fmt.Errorf("refusing to roll back %s while higher version %s is still applied; roll back highest-first", version, v)
		}
	}

	body, err := os.ReadFile(downPath)
	if err != nil {
		return err
	}

	// A down file that drops an index CONCURRENTLY (mirroring its up) cannot run inside a
	// transaction block — apply it statement-by-statement, auto-commit, exactly like the up path.
	if concurrentlyRe.Match(body) {
		if _, err := c.Exec(ctx, fmt.Sprintf("SET statement_timeout = '%s'", r.StatementTimeout)); err != nil {
			return err
		}
		var execErr error
		for _, stmt := range splitStatements(string(body)) {
			if _, execErr = c.Exec(ctx, stmt); execErr != nil {
				break
			}
		}
		_, _ = c.Exec(ctx, "RESET statement_timeout")
		if execErr != nil {
			return execErr
		}
		fmt.Printf("rolled back %s\n", version)
		return nil
	}

	tx, err := c.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	fmt.Printf("rolled back %s\n", version)
	return nil
}

// splitStatements splits a migration file into individual SQL statements on top-level
// semicolons. It is used ONLY for the non-transactional CONCURRENTLY path, where the whole file
// cannot be sent as one implicit-transaction simple-query. It is intentionally simple: it handles
// single-quoted string literals and line/block comments, but NOT dollar-quoted bodies — so a
// CONCURRENTLY migration MUST contain only plain statements (no plpgsql / $$ ... $$). Index
// migrations meet that contract. Empty/whitespace/comment-only fragments are dropped.
func splitStatements(sqlText string) []string {
	var out []string
	var buf strings.Builder
	inLine, inBlock, inString := false, false, false
	rs := []rune(sqlText)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		var next rune
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		switch {
		case inLine:
			buf.WriteRune(c)
			if c == '\n' {
				inLine = false
			}
		case inBlock:
			buf.WriteRune(c)
			if c == '*' && next == '/' {
				buf.WriteRune(next)
				i++
				inBlock = false
			}
		case inString:
			buf.WriteRune(c)
			if c == '\'' {
				inString = false
			}
		case c == '-' && next == '-':
			buf.WriteRune(c)
			buf.WriteRune(next)
			i++
			inLine = true
		case c == '/' && next == '*':
			buf.WriteRune(c)
			buf.WriteRune(next)
			i++
			inBlock = true
		case c == '\'':
			buf.WriteRune(c)
			inString = true
		case c == ';':
			stmt := strings.TrimSpace(buf.String())
			if !isBlankSQL(stmt) {
				out = append(out, stmt)
			}
			buf.Reset()
		default:
			buf.WriteRune(c)
		}
	}
	if stmt := strings.TrimSpace(buf.String()); !isBlankSQL(stmt) {
		out = append(out, stmt)
	}
	return out
}

// isBlankSQL reports whether a fragment is empty or contains only line comments/whitespace (so it
// would be a no-op statement, which Postgres rejects on an empty simple query).
func isBlankSQL(stmt string) bool {
	if stmt == "" {
		return true
	}
	for _, line := range strings.Split(stmt, "\n") {
		l := strings.TrimSpace(line)
		if l != "" && !strings.HasPrefix(l, "--") {
			return false
		}
	}
	return true
}

func (r *Runner) downPathFor(version string) (string, error) {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return "", err
	}
	prefix := "send_managed_" + version + "_"
	for _, e := range entries {
		n := e.Name()
		if strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".down.sql") {
			return filepath.Join(r.Dir, n), nil
		}
	}
	return "", fmt.Errorf("no down file for version %s", version)
}

// Status prints applied vs pending.
func (r *Runner) Status(ctx context.Context) error {
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())

	applied, err := r.appliedVersions(ctx, c)
	if err != nil {
		return err
	}
	files, err := r.discover()
	if err != nil {
		return err
	}
	fmt.Printf("migrations dir: %s\n", r.Dir)
	if len(files) == 0 {
		fmt.Println("  (no migration files on disk)")
	}
	pending := 0
	for _, f := range files {
		state := "PENDING"
		if applied[f.Version] {
			state = "applied"
		} else {
			pending++
		}
		fmt.Printf("  %s_%-24s %s\n", f.Version, f.Name, state)
	}
	fmt.Printf("summary: %d applied, %d pending\n", len(applied), pending)
	return nil
}

// SetAppPassword sets send_managed_app's password from the provided value (out-of-band; never
// in migration SQL, never logged). Uses a parameterized-safe ALTER ROLE via format with a
// quoted literal. The role must already exist (created by the bootstrap migration).
func (r *Runner) SetAppPassword(ctx context.Context, password string) error {
	if password == "" {
		return fmt.Errorf("SEND_MANAGED_MODULE_PASSWORD is empty; refusing to set an empty app-role password")
	}
	c, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close(context.Background())
	// quote_literal via a server-side format to avoid injection; the value is never logged.
	var stmt string
	if err := c.QueryRow(ctx, "SELECT format('ALTER ROLE send_managed_app LOGIN PASSWORD %L', $1::text)", password).Scan(&stmt); err != nil {
		return fmt.Errorf("format alter role: %w", err)
	}
	if _, err := c.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("alter role send_managed_app: %w", err)
	}
	fmt.Println("set-app-password: send_managed_app password set (value not logged)")
	return nil
}

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	for len(v) < 4 {
		v = "0" + v
	}
	return v
}
