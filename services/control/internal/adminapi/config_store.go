package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// configStore reads/writes send_managed.config, translating the Phase 1 JSONB envelope {"v": …} to a
// clean scalar for the API (§6.1) and back. The DB stores opaque JSONB; value_type + semantic
// validation are application-layer (§6.3/§6.4) — the store does not validate, callers do.

// configRow is the external config object (§6.1).
type configRow struct {
	Key          string          `json:"key"`
	Value        json.RawMessage `json:"value"`         // unwrapped scalar (the inner "v")
	DefaultValue json.RawMessage `json:"default_value"` // unwrapped scalar
	ValueType    string          `json:"value_type"`
	Description  string          `json:"description"`
	UpdatedAt    string          `json:"updated_at"`
}

var errConfigKeyNotFound = errors.New("config key not found")

// getConfig reads one key. Returns errConfigKeyNotFound if absent. Takes pgxDB (not *pgxpool.Pool)
// so the PUT path can read INSIDE its own write transaction — see handleConfigPut.
func getConfig(ctx context.Context, db pgxDB, key string) (*configRow, error) {
	var (
		valJSON, defJSON []byte
		vtype, desc      string
		updatedAt        string
	)
	err := db.QueryRow(ctx,
		`SELECT value, default_value, value_type, description,
		        to_char(updated_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"')
		   FROM send_managed.config WHERE key=$1`, key).
		Scan(&valJSON, &defJSON, &vtype, &desc, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errConfigKeyNotFound
	}
	if err != nil {
		return nil, err
	}
	val, uerr := unwrapV(valJSON)
	if uerr != nil {
		return nil, fmt.Errorf("unwrap value for %q: %w", key, uerr)
	}
	def, uerr := unwrapV(defJSON)
	if uerr != nil {
		return nil, fmt.Errorf("unwrap default for %q: %w", key, uerr)
	}
	return &configRow{
		Key: key, Value: val, DefaultValue: def,
		ValueType: vtype, Description: desc, UpdatedAt: updatedAt,
	}, nil
}

// listConfig reads all keys ordered by key (§6.2 — the set is small, no pagination).
func listConfig(ctx context.Context, db pgxDB) ([]configRow, error) {
	rows, err := db.Query(ctx,
		`SELECT key, value, default_value, value_type, description,
		        to_char(updated_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS"Z"')
		   FROM send_managed.config ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []configRow
	for rows.Next() {
		var (
			key, vtype, desc, updatedAt string
			valJSON, defJSON            []byte
		)
		if err := rows.Scan(&key, &valJSON, &defJSON, &vtype, &desc, &updatedAt); err != nil {
			return nil, err
		}
		val, uerr := unwrapV(valJSON)
		if uerr != nil {
			return nil, fmt.Errorf("unwrap value for %q: %w", key, uerr)
		}
		def, uerr := unwrapV(defJSON)
		if uerr != nil {
			return nil, fmt.Errorf("unwrap default for %q: %w", key, uerr)
		}
		out = append(out, configRow{
			Key: key, Value: val, DefaultValue: def,
			ValueType: vtype, Description: desc, UpdatedAt: updatedAt,
		})
	}
	return out, rows.Err()
}

// updateConfigValue re-wraps value into {"v": value} and writes it (§6.3). Only `value` +
// `updated_at` change; key/default_value/value_type/description are immutable via the API.
// Returns errConfigKeyNotFound if the key does not exist (keys are not operator-creatable).
func updateConfigValue(ctx context.Context, db pgxDB, key string, value json.RawMessage) error {
	wrapped, err := wrapV(value)
	if err != nil {
		return err
	}
	tag, err := db.Exec(ctx,
		`UPDATE send_managed.config SET value=$2, updated_at=NOW() WHERE key=$1`, key, wrapped)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errConfigKeyNotFound
	}
	return nil
}

// unwrapV extracts the inner scalar from a stored {"v": …} JSONB envelope. A row that is not an
// object with a "v" key is a seed-contract violation (should never happen for seeded keys).
func unwrapV(raw []byte) (json.RawMessage, error) {
	var env struct {
		V json.RawMessage `json:"v"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.V == nil {
		// {"v": null} unmarshals V to the literal null; distinguish a truly missing "v".
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err == nil {
			if v, ok := m["v"]; ok {
				return v, nil
			}
		}
		return nil, fmt.Errorf("stored config value has no \"v\" envelope key")
	}
	return env.V, nil
}

// wrapV re-wraps a scalar into the {"v": …} envelope for storage.
func wrapV(value json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(value) {
		return nil, fmt.Errorf("value is not valid JSON")
	}
	out, err := json.Marshal(map[string]json.RawMessage{"v": value})
	if err != nil {
		return nil, err
	}
	return out, nil
}
