package main

// N1/D12 — the migration runner's copy of the DSN scrub.
//
// WHY A COPY AND NOT AN IMPORT. This tool is its own Go module (tools/send-managed-migrate/go.mod), and
// its Dockerfile copies ONLY tools/send-managed-migrate/ into the build context. Importing
// services/control/internal/dsnscrub would need either a replace directive across module boundaries or
// a wider Docker context — both of which change how this tool is built and shipped in order to share
// 40 lines. The copy is the smaller change; the DRIFT is the risk it creates, so it is not left to
// discipline: TestDSNScrub_CopyHasNotDriftedFromControl compares this file's logic against the
// canonical one byte-for-byte and fails when they diverge. A comment asking the next person to keep
// two files in sync is not a control.
//
// See services/control/internal/dsnscrub/dsnscrub.go for the full measurement at pgx v5.7.4: which
// four DSN shapes leak the whole connection string, which trigger actually fires at this pin (NOT
// pool_max_conns — that message carries no DSN), and why the check is derived rather than a pattern
// list.
//
// THIS TOOL'S LEAK PATH: runner.go connect() wraps the pgx.Connect error, Up/Down/Status return it,
// and main.go:91 hands it to fatal(err.Error()), which prints to stderr. A migration run is exactly
// when an operator is most likely to be pasting output into a ticket.

import (
	"net/url"
	"strings"
)

// scrubbedDSNError replaces a driver error whose text embeds the DSN. Unwrap is kept so errors.Is/As
// still reach the original; only the STRING — the thing that reaches stderr — is replaced.
type scrubbedDSNError struct {
	err error
	msg string
}

func (e *scrubbedDSNError) Error() string { return e.msg }
func (e *scrubbedDSNError) Unwrap() error { return e.err }

// scrubDSNError returns err unchanged when its text cannot expose the DSN, and a scrubbed replacement
// when it can. A nil err returns nil.
func scrubDSNError(err error, dsn string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	leaks := strings.Contains(msg, dsn)
	if !leaks {
		for _, cand := range dsnCredentialCandidates(dsn) {
			if strings.Contains(msg, cand) {
				leaks = true
				break
			}
		}
	}
	if !leaks {
		return err
	}
	return &scrubbedDSNError{
		err: err,
		msg: "connection string rejected by the driver (message suppressed: it embedded the DSN — check MIGRATE_DATABASE_URL or DATABASE_URL)",
	}
}

// structuralDSNKeys — keywords whose VALUES cannot be a credential. A key MISSING here costs one
// message its detail; a key WRONGLY PRESENT costs a credential, so only keywords whose libpq
// semantics make a secret impossible are listed. `password`/`sslpassword` are deliberately absent, as
// is every keyword nobody enumerated: unknown means possibly-secret.
var structuralDSNKeys = map[string]bool{
	"host": true, "hostaddr": true, "port": true, "dbname": true, "database": true,
	"user": true, "application_name": true, "fallback_application_name": true,
	"connect_timeout": true, "client_encoding": true, "options": true, "service": true,
	"sslmode": true, "sslrootcert": true, "sslcert": true, "sslcrl": true, "sslcrldir": true,
	"sslsni": true, "sslnegotiation": true, "sslkey": true, "passfile": true,
	"target_session_attrs": true, "load_balance_hosts": true, "channel_binding": true,
	"require_auth": true, "gssencmode": true, "gsslib": true, "krbsrvname": true,
	"replication": true, "tcp_user_timeout": true,
	"keepalives": true, "keepalives_idle": true, "keepalives_interval": true, "keepalives_count": true,
	"pool_max_conns": true, "pool_min_conns": true, "pool_min_idle_conns": true,
	"pool_max_conn_lifetime": true, "pool_max_conn_lifetime_jitter": true,
	"pool_max_conn_idle_time": true, "pool_health_check_period": true,
	"search_path": true,
}

func knownDSNKey(s string) bool {
	return structuralDSNKeys[s] || s == "password" || s == "sslpassword"
}

// dsnCredentialCandidates extracts every substring of the DSN that could be a secret, in every
// spelling pgx/libpq honors. The pending-key state exists because libpq honors `password = SECRET`,
// which splits into three whitespace fields — and because collecting every bare token instead made
// `port=5432` yield the candidate "5432", which suppressed ordinary connection-refused messages.
func dsnCredentialCandidates(dsn string) []string {
	var out []string
	add := func(s string) {
		if len(s) >= 4 {
			out = append(out, s)
		}
	}
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		if pw, ok := u.User.Password(); ok {
			add(pw)
		}
		for k, vs := range u.Query() {
			if structuralDSNKeys[k] {
				continue
			}
			for _, v := range vs {
				add(v)
			}
		}
	}
	var pendingKey string
	for _, field := range strings.FieldsFunc(dsn, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '&' || r == '?'
	}) {
		if eq := strings.IndexByte(field, '='); eq >= 0 {
			key := strings.TrimSpace(field[:eq])
			val := strings.Trim(strings.TrimSpace(field[eq+1:]), `'"`)
			if key == "" {
				key = pendingKey
			}
			if val == "" {
				pendingKey = key
				continue
			}
			pendingKey = ""
			if !structuralDSNKeys[key] {
				add(val)
			}
			continue
		}
		token := strings.Trim(field, `'"`)
		if pendingKey != "" {
			if !structuralDSNKeys[pendingKey] {
				add(token)
			}
			pendingKey = ""
			continue
		}
		if knownDSNKey(token) {
			pendingKey = token
			continue
		}
		add(token)
	}
	return out
}
