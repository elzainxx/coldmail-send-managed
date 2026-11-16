package adminapi

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// poolResource is the external pool object (§5.1). account_count is a computed convenience.
type poolResource struct {
	ID              string  `json:"id"`
	UserID          string  `json:"user_id"`
	Kind            string  `json:"kind"`
	OwnerUserID     *string `json:"owner_user_id"`
	Name            string  `json:"name"`
	RoutingStrategy string  `json:"routing_strategy"`
	FallbackPoolID  *string `json:"fallback_pool_id"`
	Enabled         bool    `json:"enabled"`
	AccountCount    int     `json:"account_count"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`

	// cursorTS: created_at at full microsecond precision for the keyset cursor only (never serialized) —
	// see account_store.go's accountResource.cursorTS.
	cursorTS string `json:"-"`
}

const poolSelectCols = `p.id, p.user_id, p.kind, p.owner_user_id, p.name, p.routing_strategy,
	p.fallback_pool_id, p.enabled,
	(SELECT count(*) FROM send_managed.accounts a WHERE a.pool_id = p.id) AS account_count,
	to_char(p.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(p.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(p.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`

var errPoolNotFound = errors.New("pool not found")

func scanPool(row pgx.Row) (*poolResource, error) {
	var p poolResource
	if err := row.Scan(&p.ID, &p.UserID, &p.Kind, &p.OwnerUserID, &p.Name, &p.RoutingStrategy,
		&p.FallbackPoolID, &p.Enabled, &p.AccountCount, &p.CreatedAt, &p.UpdatedAt, &p.cursorTS); err != nil {
		return nil, err
	}
	return &p, nil
}

// getPool reads one pool by id. errPoolNotFound if absent.
func getPool(ctx context.Context, db pgxDB, id string) (*poolResource, error) {
	row := db.QueryRow(ctx, "SELECT "+poolSelectCols+" FROM send_managed.pools p WHERE p.id=$1", id)
	p, err := scanPool(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errPoolNotFound
	}
	return p, err
}

// systemUserProvisioned reports whether the reserved SEND_MANAGED_SYSTEM_USER_ID row exists in
// public.users (§0.1 pre-flight). The module has SELECT-only on public.users; it cannot seed it.
func systemUserProvisioned(ctx context.Context, db pgxDB) (bool, error) {
	var exists bool
	err := db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM public.users WHERE id=$1)", SystemUserID).Scan(&exists)
	return exists, err
}

// userExists reports whether a real public.users row exists (for dedicated-pool owner validation).
func userExists(ctx context.Context, db pgxDB, userID string) (bool, error) {
	var exists bool
	err := db.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM public.users WHERE id=$1)", userID).Scan(&exists)
	return exists, err
}

// fallbackCycleExists walks the fallback chain starting at target and reports whether it reaches
// editing (the pool being created/edited) — i.e. setting editing.fallback = target would form a
// cycle (§5.5). editing may be "" on create (a brand-new pool cannot yet be pointed at). A
// max-depth guard bounds a corrupt chain. Kind-agnostic (OD-13: Phase 4 owns kind policy).
func fallbackCycleExists(ctx context.Context, db pgxDB, editing, target string) (bool, error) {
	const maxDepth = 64
	cur := target
	for depth := 0; depth < maxDepth; depth++ {
		if editing != "" && cur == editing {
			return true, nil // the chain from target loops back to the pool being edited
		}
		var next *string
		err := db.QueryRow(ctx, "SELECT fallback_pool_id FROM send_managed.pools WHERE id=$1", cur).Scan(&next)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, errPoolNotFound // target (or a hop) does not exist
		}
		if err != nil {
			return false, err
		}
		if next == nil {
			return false, nil // chain ends without reaching editing
		}
		cur = *next
	}
	// Exceeded max depth => treat as a cycle (defensive; a well-formed chain is short).
	return true, nil
}

// insertPool inserts a pool with a caller-derived user_id + owner_user_id and returns the full
// resource. Any raw DB error is returned for the handler to translate.
func insertPool(ctx context.Context, db pgxDB, userID string, ownerUserID *string, kind, name, routing string, fallback *string, enabled bool) (*poolResource, error) {
	var id string
	err := db.QueryRow(ctx,
		`INSERT INTO send_managed.pools (user_id, kind, owner_user_id, name, routing_strategy, fallback_pool_id, enabled)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		userID, kind, ownerUserID, name, routing, fallback, enabled).Scan(&id)
	if err != nil {
		return nil, err
	}
	return getPool(ctx, db, id)
}

// deletePool deletes a pool by id. Returns the command tag's row count via the error-free path;
// a RESTRICT violation surfaces as a *pgconn.PgError (23503) for the handler to translate.
func deletePool(ctx context.Context, db pgxDB, id string) (int64, error) {
	tag, err := db.Exec(ctx, "DELETE FROM send_managed.pools WHERE id=$1", id)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// pgxDB is the subset of pgxpool.Pool / pgx.Tx used by the stores, so a store function works
// against both a pool (single-statement reads) and a transaction (the audited write paths).
type pgxDB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Compile-time proof both a pool and a tx satisfy pgxDB.
var (
	_ pgxDB = (*pgxpool.Pool)(nil)
	_ pgxDB = (pgx.Tx)(nil)
)
