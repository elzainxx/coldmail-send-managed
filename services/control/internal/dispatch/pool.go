// pool.go — Phase 4 Chunk 4: pool resolution + fallback (§4). RESOLVE_POOL_CHAIN produces the
// ordered list of pools a job's account-consume tries: the tenant's own dedicated pool(s) first,
// then — ONLY when a shared fallback resolves — one shared pool. Kind policy (P4-3): fallback
// lands ONLY on shared pools, never dedicated→dedicated (a dedicated pool's overflow must never
// route onto another user's dedicated accounts). Multi-shared resolution (P4-4): an explicit
// per-dedicated-pool fallback_pool_id wins if it resolves to an enabled SHARED pool, else the
// config default_shared_pool_id if set+valid, else no fallback at all.
package dispatch

import (
	"context"
	"time"
)

// maxFallbackHops is a hardcoded defense-in-depth cap on the chain length (§4.2, §10 P4-19) — NOT
// a config-not-code violation: it bounds a walk that Phase 2's cycle check (§5.5) already makes
// acyclic, so this is a safety backstop against a hypothetical uncaught cycle, never an
// operator-facing behavior dial. If it is ever needed as a real dial, promote it to a seeded key
// then (documented here for the config-not-code audit trail, mirroring the admin API's
// DailyLimitCeiling precedent, OD-24).
const maxFallbackHops = 8

// poolInfo is the subset of send_managed.pools RESOLVE_POOL_CHAIN needs.
type poolInfo struct {
	id             string
	kind           string // 'shared' | 'dedicated'
	fallbackPoolID *string
	enabled        bool
}

// resolvePoolChain implements RESOLVE_POOL_CHAIN (§4.2) for a job's tenant. It NEVER errors on a
// "chain resolves to nothing" outcome — an empty chain (dedicated exhausted with no shared
// fallback, or a base user with no configured/valid default_shared_pool_id, §4.3) is represented
// as a nil/empty slice, exactly like an exhausted pool: the caller requeues, no crash, no churn.
func (d *Dispatcher) resolvePoolChain(ctx context.Context, userID string, cfg Config) ([]string, error) {
	dedicated, err := d.dedicatedPools(ctx, userID)
	if err != nil {
		return nil, err
	}

	chain := make([]string, 0, len(dedicated)+1)
	for _, p := range dedicated {
		chain = append(chain, p.id)
	}

	sharedTarget, err := d.resolveSharedFallback(ctx, dedicated, cfg)
	if err != nil {
		return nil, err
	}
	if sharedTarget != "" {
		chain = append(chain, sharedTarget)
	}

	resolved := dedupeCapped(chain, maxFallbackHops)
	// An EMPTY chain means literally nothing was reachable — no dedicated pool AND no resolvable
	// shared fallback. That is exactly the §4.3 shared_pool_not_configured condition (whether the
	// cause is a base user with no dedicated pool, or a dedicated user whose fallback would have
	// been the missing shared pool). Emit the throttled internal diagnostic (never per-job/per-tick
	// — logSharedPoolNotConfigured rate-limits across the whole process).
	if len(resolved) == 0 {
		d.logSharedPoolNotConfigured()
	}
	return resolved, nil
}

// dedicatedPools returns the tenant's own enabled dedicated pools, oldest-created first
// (deterministic — a user MAY own several, §0/§4.2).
func (d *Dispatcher) dedicatedPools(ctx context.Context, userID string) ([]poolInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := d.db.Query(ctx,
		`SELECT id, kind::text, fallback_pool_id, enabled
		 FROM send_managed.pools
		 WHERE kind = 'dedicated' AND owner_user_id = $1 AND enabled = TRUE
		 ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []poolInfo
	for rows.Next() {
		var p poolInfo
		if err := rows.Scan(&p.id, &p.kind, &p.fallbackPoolID, &p.enabled); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// resolveSharedFallback implements P4-4's multi-shared resolution: (a) the first dedicated pool's
// explicit fallback_pool_id wins IF it resolves to an enabled SHARED pool (P4-3 kind policy — a
// fallback_pool_id pointing at a dedicated pool is ignored for routing, ruled out here by the
// kind='shared' filter itself); else (b) the config default_shared_pool_id, if set and it resolves
// to an enabled shared pool (§4.3); else (c) "" — no fallback. Returns "" (never an error) for
// every "not configured / invalid" case, matching §4.3's clean-state requirement.
func (d *Dispatcher) resolveSharedFallback(ctx context.Context, dedicated []poolInfo, cfg Config) (string, error) {
	// (a) explicit per-pool fallback, dedicated pools only carry one meaningfully (the FIRST
	// dedicated pool's fallback_pool_id — §4.2's pseudocode walks "for d in dedicated" and takes
	// the first that resolves to shared+enabled).
	for _, d0 := range dedicated {
		if d0.fallbackPoolID == nil {
			continue
		}
		ok, err := d.isEnabledSharedPool(ctx, *d0.fallbackPoolID)
		if err != nil {
			return "", err
		}
		if ok {
			return *d0.fallbackPoolID, nil
		}
	}
	// (b) the config default, if set and valid.
	if cfg.DefaultSharedPoolID != nil {
		ok, err := d.isEnabledSharedPool(ctx, *cfg.DefaultSharedPoolID)
		if err != nil {
			return "", err
		}
		if ok {
			return *cfg.DefaultSharedPoolID, nil
		}
	}
	// (c) no fallback — including default_shared_pool_id unset/non-existent/disabled/dedicated
	// (§4.3's "shared_pool_not_configured" clean state; the caller's empty chain IS that state).
	return "", nil
}

// isEnabledSharedPool checks a candidate fallback target is a real, enabled, SHARED pool (P4-3
// kind policy + §4.3 validity check). A non-existent id is "not eligible," not an error.
func (d *Dispatcher) isEnabledSharedPool(ctx context.Context, poolID string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var ok bool
	err := d.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM send_managed.pools WHERE id = $1 AND kind = 'shared' AND enabled = TRUE)`,
		poolID).Scan(&ok)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// dedupeCapped removes duplicate pool ids (defense-in-depth; the chain construction above cannot
// currently produce one, since dedicated and shared are disjoint sets, but this guards any future
// change) and caps the result to maxFallbackHops (§4.2's bounded, de-duplicated, acyclic walk).
func dedupeCapped(ids []string, max int) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) >= max {
			break
		}
	}
	return out
}
