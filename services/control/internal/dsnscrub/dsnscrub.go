// Package dsnscrub suppresses driver errors whose text embeds the connection string.
//
// THE DEFECT, MEASURED against the PINNED pgx v5.7.4 rather than assumed. pgx returns a
// ParseConfigError whose Error() quotes the ENTIRE connection string, and its own redaction covers
// only two of the spellings it honors:
//
//	pgconn/errors.go redactPW  → takes the URL branch for any postgres:// DSN...
//	pgconn/errors.go redactURL → ...then re-emits RawQuery VERBATIM, so ?password= and
//	                             ?sslpassword= (both honored) survive in clear.
//	pgconn/errors.go redactPW  → the keyword/value branch's `password=[^ ]*` cannot match the
//	                             honored SPACED spelling `password = SECRET`.
//
// Measured at this pin through pgxpool.ParseConfig — the Control's own call, internal/db/db.go:
//
//	postgres://u@h:5432/db?password=SECRET&sslmode=bogusmode    → LEAKS (whole DSN echoed)
//	postgres://u@h:5432/db?sslpassword=SECRET&sslmode=bogusmode → LEAKS (whole DSN echoed)
//	host=h password = SECRET dbname=db sslmode=bogusmode        → LEAKS (whole DSN echoed)
//	postgres://u@h:99999999/db?password=SECRET                  → LEAKS (whole DSN echoed)
//	host=h password=SECRET dbname=db sslmode=bogusmode          → redacted by pgx (password=xxxxx)
//	postgres://u:SECRET@h:5432/db?sslmode=bogusmode             → redacted by pgx (:xxxxx@)
//
// THE TRIGGER IS PART OF THE FINDING. M1's equivalent test forces the parse stage to fail with a
// malformed `pool_max_conns=notanumber`. At M1's pgx pin that produces a message containing the DSN;
// at v5.7.4 it produces "cannot parse pool_max_conns: strconv.ParseInt..." with NO DSN in it at all.
// So porting M1's test verbatim into this module would have measured LEAK=false on every shape and
// declared the module safe — a vacuous test that would have shipped, and the reason the shapes above
// are pinned with `sslmode=bogusmode` and a bad port, both re-measured on every run by
// TestDSNScrub_PgxStillLeaksAtThisPin. A test inherited from a sibling module is a test whose premise
// was verified somewhere else.
//
// WHERE THE ERROR REACHED. Three paths per module, one of them at RUNTIME, repeatedly:
//
//	cmd/control/main.go   log.Fatalf("FATAL db connect: %v", err)        — boot
//	cmd/control/main.go   log.Fatalf("FATAL singleton acquire: %v", err) — boot
//	internal/singleton    logReacquireFailure(... err.Error() ...)       — every re-acquire failure,
//	                      1st then every 12th attempt, for as long as the lock stays lost
//
// WHY THE FIX IS AT THE SOURCE AND NOT AT THE LOG CALLS. A fix at a log call protects that one call;
// the next `log.Printf("%v", err)` anywhere reintroduces the leak. Scrubbing where the error is
// PRODUCED means no caller can hold an unsafe value in the first place. That is also why this is a
// shared package rather than a helper inside internal/db: internal/singleton opens its own dedicated
// connection with its own DSN and is a separate producer.
//
// WHY THE CHECK IS DERIVED, NOT A PATTERN LIST. Err asks "does this message contain a credential
// candidate from the DSN I was handed" — not "does it match a known-bad spelling". A blocklist of
// `password=` spellings is precisely how pgx's own redaction came to miss two of them, and a
// blocklist cannot cover a future pgx release that leaks a field nobody has thought of yet.
package dsnscrub

import (
	"net/url"
	"strings"
)

// scrubbedError replaces a driver error whose text embeds the DSN. It keeps Unwrap so errors.Is/As
// still reach the original (a caller may legitimately test for a pgx sentinel); only the STRING —
// the thing that reaches a log — is replaced.
type scrubbedError struct {
	err error
	msg string
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// Err returns err unchanged when its text cannot expose the DSN, and a scrubbed replacement when it
// can. A nil err returns nil, so it composes at a return site without a guard.
func Err(err error, dsn string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	// The DSN appearing whole is the common case at this pin and is enough on its own: user, host,
	// port and database are not credentials, but they are infrastructure detail with no business in
	// a log line.
	leaks := strings.Contains(msg, dsn)
	if !leaks {
		for _, cand := range credentialCandidates(dsn) {
			if strings.Contains(msg, cand) {
				leaks = true
				break
			}
		}
	}
	if !leaks {
		return err
	}
	// Deliberately NOT a partial redaction of the message. Selective rewriting is what failed
	// upstream: it leaves whatever the pattern missed. Replace the whole text and name the one fact
	// an operator needs — which env var to look at.
	return &scrubbedError{
		err: err,
		msg: "connection string rejected by the driver (message suppressed: it embedded the DSN — check DATABASE_URL)",
	}
}

// structuralKeys are the libpq/pgx connection keywords whose VALUES cannot be a credential: they name
// where the database is and how to reach it, not who you are.
//
// THE DIRECTION OF RISK IS DELIBERATE, AND IT IS THE OPPOSITE OF A BLOCKLIST'S. A key MISSING from
// this map is over-collection: its value becomes a candidate, and the cost is one error message
// losing its detail. A key WRONGLY PRESENT is under-collection, and that costs a credential — so this
// map contains only keywords whose libpq semantics make a secret value impossible. `password` and
// `sslpassword` are deliberately absent, as is every keyword nobody has enumerated: an unknown key
// is treated as possibly-secret, which is what keeps the check DERIVED rather than a blocklist of
// known-bad spellings.
//
// `sslkey` and `passfile` are PATHS here, not secrets — the file they point at holds the secret, and
// a path in a log line is infrastructure detail, not a credential.
var structuralKeys = map[string]bool{
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

// knownKeys is structuralKeys plus the credential-bearing keywords. Used only to recognise a BARE
// token as a key awaiting its spaced '=' rather than as a value.
func knownKey(s string) bool {
	return structuralKeys[s] || s == "password" || s == "sslpassword"
}

// credentialCandidates extracts every substring of the DSN that could be a secret, in every spelling
// pgx/libpq honors: URL userinfo password, URL query values, and keyword/value values (quoted,
// unquoted, spaced or not).
//
// THE SPACED SPELLING IS WHY THIS TRACKS STATE. libpq honors `password = SECRET`, which splits into
// three whitespace fields, so the value arrives detached from its key. The first version of this
// function handled that by collecting EVERY bare token, and TestDSNScrub_ShortValuesDoNotSuppress-
// Everything caught what that costs: `port=5432` yields the candidate "5432", which appears in
// virtually every connection error ("dial tcp 10.0.0.5:5432: connect: connection refused"), so the
// scrub fired on a message containing no credential at all and replaced an operator's entire
// diagnosis with a suppression notice. Blinding every operator to every database error is not a
// conservative failure — it is a different outage. Tracking the pending key is what lets a
// structural value be recognised as structural in BOTH spellings.
func credentialCandidates(dsn string) []string {
	var out []string
	add := func(s string) {
		// Under 4 chars a "secret" is likelier to appear incidentally in a driver message ("db",
		// "u") than to be a real credential, and matching it would suppress every error. This is a
		// backstop beneath structuralKeys, not the primary defence — 5432 is four characters.
		if len(s) >= 4 {
			out = append(out, s)
		}
	}
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" {
		if pw, ok := u.User.Password(); ok {
			add(pw)
		}
		for k, vs := range u.Query() {
			if structuralKeys[k] {
				continue
			}
			for _, v := range vs {
				add(v)
			}
		}
	}
	// Keyword/value form, and the query-ish tail of a URL that failed to parse.
	var pendingKey string
	for _, field := range strings.FieldsFunc(dsn, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '&' || r == '?'
	}) {
		if eq := strings.IndexByte(field, '='); eq >= 0 {
			key := strings.TrimSpace(field[:eq])
			val := strings.Trim(strings.TrimSpace(field[eq+1:]), `'"`)
			if key == "" {
				// A lone "=" or "=VALUE": the key was the previous field (`password = SECRET`).
				key = pendingKey
			}
			if val == "" {
				// `password =` — the value is the next field.
				pendingKey = key
				continue
			}
			pendingKey = ""
			if !structuralKeys[key] {
				add(val)
			}
			continue
		}
		token := strings.Trim(field, `'"`)
		if pendingKey != "" {
			// The detached value of a spaced pair.
			if !structuralKeys[pendingKey] {
				add(token)
			}
			pendingKey = ""
			continue
		}
		if knownKey(token) {
			// A key awaiting its spaced '='. Not a value, so not a candidate — and collecting it
			// would make the literal word "password" a candidate, which suppresses every
			// `password authentication failed for user "u"` the server ever returns.
			pendingKey = token
			continue
		}
		// An unrecognised bare token: possibly the value of a spaced pair for a keyword this code has
		// never heard of. Over-collect, per the risk direction documented on structuralKeys.
		add(token)
	}
	return out
}
