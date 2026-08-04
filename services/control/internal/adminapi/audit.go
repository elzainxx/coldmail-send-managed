package adminapi

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// Audit sink (§7.4): every audited operation — a lifecycle transition, account create/update/
// delete — writes EXACTLY ONE send_managed.account_events row in the SAME transaction as its data
// write, performed EXPLICITLY here (NOT via a trigger on send_managed.accounts, so the audit stays
// scoped to admin actions and never fires on the dispatcher's future writes). actor is always
// 'admin' in Phase 2. The audit row commits iff the operation commits.

// flagsSnapshot is the JSONB shape stored in from_flags/to_flags: {paused, pause_reason,
// quarantined, disabled}.
type flagsSnapshot struct {
	Paused      bool    `json:"paused"`
	PauseReason *string `json:"pause_reason"`
	Quarantined bool    `json:"quarantined"`
	Disabled    bool    `json:"disabled"`
}

func snapshotOf(f accountFlags) flagsSnapshot {
	return flagsSnapshot{Paused: f.Paused, PauseReason: f.PauseReason, Quarantined: f.Quarantined, Disabled: f.Disabled}
}

// decodeFlags unmarshals a stored from_flags/to_flags JSONB blob back into a *flagsSnapshot
// (nil when the column is SQL NULL, e.g. create has no from, delete has no to).
func decodeFlags(raw []byte) *flagsSnapshot {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var f flagsSnapshot
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil
	}
	return &f
}

// writeAuditEvent inserts one account_events row inside the given tx. from/to may be nil (e.g.
// create has no "from"; delete has no "to"). reason is the operator's internal note (never
// surfaced in an error envelope); it and request_id bridge to the audit read (§7.4).
func writeAuditEvent(ctx context.Context, tx pgx.Tx, accountID, userID, action string, from, to *flagsSnapshot, reason *string, requestID string) error {
	var fromJSON, toJSON []byte
	if from != nil {
		b, err := json.Marshal(from)
		if err != nil {
			return err
		}
		fromJSON = b
	}
	if to != nil {
		b, err := json.Marshal(to)
		if err != nil {
			return err
		}
		toJSON = b
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO send_managed.account_events
		   (account_id, user_id, actor, action, from_flags, to_flags, reason, request_id)
		 VALUES ($1, $2, 'admin', $3, $4, $5, $6, $7)`,
		accountID, userID, action, fromJSON, toJSON, reason, requestID)
	return err
}
