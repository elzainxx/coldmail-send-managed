package adminapi

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// accountStatus is the nested status object (§2.1).
type accountStatus struct {
	Paused      bool    `json:"paused"`
	PauseReason *string `json:"pause_reason"`
	Quarantined bool    `json:"quarantined"`
	Disabled    bool    `json:"disabled"`
}

// accountResource is the external account object (§2.1). It NEVER includes the secret,
// ciphertext, nonce, or key id — has_credential is the ONLY credential-related field.
type accountResource struct {
	ID                 string        `json:"id"`
	UserID             string        `json:"user_id"`
	PoolID             string        `json:"pool_id"`
	SMTPHost           string        `json:"smtp_host"`
	SMTPPort           int           `json:"smtp_port"`
	SMTPUsername       string        `json:"smtp_username"`
	FromAddress        string        `json:"from_address"`
	FromName           *string       `json:"from_name"`
	HasCredential      bool          `json:"has_credential"`
	Status             accountStatus `json:"status"`
	DailyLimit         int           `json:"daily_limit"`
	DailySentCount     int           `json:"daily_sent_count"`
	DailyResetDay      string        `json:"daily_reset_day"`
	AccountStatusScore int           `json:"account_status_score"`
	LastSuccessAt      *string       `json:"last_success_at"`
	LastFailureAt      *string       `json:"last_failure_at"`
	CreatedAt          string        `json:"created_at"`
	UpdatedAt          string        `json:"updated_at"`

	// cursorTS is created_at at FULL (microsecond) precision, for the keyset cursor ONLY — never serialized.
	// created_at above is rendered to the second (a documented, unchanged contract), and a cursor built from
	// it truncates the sort key: see handleAccountList.
	cursorTS string `json:"-"`
}

// accountSelectCols projects ONLY safe columns. The credential_* columns are deliberately absent
// from every projection (never read into a response) — has_credential is computed as "ciphertext
// is present and non-empty". octet_length keeps the actual bytes out of the query result.
const accountSelectCols = `a.id, a.user_id, a.pool_id, a.smtp_host, a.smtp_port, a.smtp_username,
	a.from_address, a.from_name,
	(octet_length(a.credential_ciphertext) > 0) AS has_credential,
	a.paused, a.pause_reason, a.quarantined, a.disabled,
	a.daily_limit, a.daily_sent_count,
	to_char(a.daily_reset_day,'YYYY-MM-DD') AS daily_reset_day,
	a.account_status_score,
	to_char(a.last_success_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(a.last_failure_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(a.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(a.updated_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"'),
	to_char(a.created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`

var errAccountNotFound = errors.New("account not found")

func scanAccount(row pgx.Row) (*accountResource, error) {
	var a accountResource
	if err := row.Scan(&a.ID, &a.UserID, &a.PoolID, &a.SMTPHost, &a.SMTPPort, &a.SMTPUsername,
		&a.FromAddress, &a.FromName, &a.HasCredential,
		&a.Status.Paused, &a.Status.PauseReason, &a.Status.Quarantined, &a.Status.Disabled,
		&a.DailyLimit, &a.DailySentCount, &a.DailyResetDay, &a.AccountStatusScore,
		&a.LastSuccessAt, &a.LastFailureAt, &a.CreatedAt, &a.UpdatedAt, &a.cursorTS); err != nil {
		return nil, err
	}
	return &a, nil
}

// getAccount reads one account by id. errAccountNotFound if absent.
func getAccount(ctx context.Context, db pgxDB, id string) (*accountResource, error) {
	row := db.QueryRow(ctx, "SELECT "+accountSelectCols+" FROM send_managed.accounts a WHERE a.id=$1", id)
	a, err := scanAccount(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errAccountNotFound
	}
	return a, err
}

// accountFlags holds just the four status flags + pause_reason (for audit from/to snapshots and
// transition preconditions). daily_sent_count is fetched alongside for the daily_limit pre-check.
type accountFlags struct {
	Paused      bool
	PauseReason *string
	Quarantined bool
	Disabled    bool
}

// readFlags loads the current status flags of an account (for lifecycle preconditions + audit
// snapshots). Uses the given pgxDB (pool or tx). errAccountNotFound if absent.
func readFlags(ctx context.Context, db pgxDB, id string) (accountFlags, error) {
	var f accountFlags
	err := db.QueryRow(ctx,
		"SELECT paused, pause_reason, quarantined, disabled FROM send_managed.accounts WHERE id=$1", id).
		Scan(&f.Paused, &f.PauseReason, &f.Quarantined, &f.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, errAccountNotFound
	}
	return f, err
}

// poolTenancy returns a pool's (kind, owner_user_id) for deriving an account's user_id (§2.2.1).
func poolTenancy(ctx context.Context, db pgxDB, poolID string) (kind string, ownerUserID *string, err error) {
	err = db.QueryRow(ctx, "SELECT kind, owner_user_id FROM send_managed.pools WHERE id=$1", poolID).
		Scan(&kind, &ownerUserID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, errPoolNotFound
	}
	return kind, ownerUserID, err
}

// deriveUserID resolves an account's user_id from its target pool (§2.2.1): dedicated → the
// pool's owner_user_id (real user); shared → SystemUserID. Returns errPoolNotFound if absent, or
// a system-user-unprovisioned signal via the returned needSystemUser flag for the caller to
// pre-flight (shared pools).
func deriveUserID(ctx context.Context, db pgxDB, poolID string) (userID string, shared bool, err error) {
	kind, owner, err := poolTenancy(ctx, db, poolID)
	if err != nil {
		return "", false, err
	}
	if kind == "shared" {
		return SystemUserID, true, nil
	}
	if owner == nil {
		// Should not happen (Phase 1 CHECK guarantees dedicated has an owner), but fail safe.
		return "", false, errors.New("dedicated pool has null owner_user_id")
	}
	return *owner, false, nil
}
