package dsnscrub

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// =============================================================================
// N1 / D12 — a DSN credential must never reach a log line
// =============================================================================
//
// See the package comment for the measurement and the three leak paths. This file is the control.
//
// EVERY DSN HERE USES A TRIGGER MEASURED TO FIRE AT v5.7.4. `sslmode=bogusmode` and a bad port both
// produce a pgx ParseConfigError that quotes the whole connection string. The obvious port from M1 —
// `pool_max_conns=notanumber` — does NOT: at this pin its message is "cannot parse pool_max_conns:
// strconv.ParseInt: parsing ...", with no DSN in it. Using it here would have made every test below
// pass without ever putting a credential in front of the assertion. TestDSNScrub_PgxStillLeaksAtThisPin
// is what keeps that from being a claim: it asserts the RAW driver error still leaks, so if a pgx bump
// fixes redaction upstream, or changes which trigger carries the DSN, the premise fails loudly instead
// of the suite quietly going vacuous.
//
// NO NETWORK. Every DSN either fails at the PARSE stage (before any socket) or points at
// 127.0.0.1. The suite is deterministic and needs no Postgres.

const canary = "CANARYSECRET1234"

// leakyDSNs are the shapes MEASURED to leak at this pin, plus the two pgx already redacts (kept so the
// fix is proven to hold for them too — if a future pgx regresses one, this catches it with no edit).
func leakyDSNs() map[string]string {
	return map[string]string{
		"url query password":    "postgres://u@127.0.0.1:5432/db?password=" + canary + "&sslmode=bogusmode",
		"url query sslpassword": "postgres://u@127.0.0.1:5432/db?sslpassword=" + canary + "&sslmode=bogusmode",
		"kv spaced password":    "host=127.0.0.1 password = " + canary + " dbname=db sslmode=bogusmode",
		"url query bad port":    "postgres://u@127.0.0.1:99999999/db?password=" + canary,
		"kv unspaced password":  "host=127.0.0.1 password=" + canary + " dbname=db sslmode=bogusmode",
		"url userinfo":          "postgres://u:" + canary + "@127.0.0.1:5432/db?sslmode=bogusmode",
	}
}

// pgxRedactsThese are the two shapes pgx v5.7.4 handles itself. Named so the anti-vacuity test can
// skip them: asserting "pgx still leaks this" for a shape pgx redacts would fail for the right reason
// and confuse the signal.
var pgxRedactsThese = map[string]bool{"kv unspaced password": true, "url userinfo": true}

// TestDSNScrub_NoCredentialSurvives is the property itself, asserted through the two driver calls the
// Control actually makes: pgxpool.ParseConfig (internal/db) and pgx.Connect (internal/singleton).
func TestDSNScrub_NoCredentialSurvives(t *testing.T) {
	for name, dsn := range leakyDSNs() {
		t.Run("ParseConfig/"+name, func(t *testing.T) {
			_, err := pgxpool.ParseConfig(dsn)
			if err == nil {
				t.Fatal("expected a parse error — without one this case proves nothing")
			}
			assertScrubbed(t, Err(err, dsn), dsn)
		})
		t.Run("Connect/"+name, func(t *testing.T) {
			_, err := pgx.Connect(context.Background(), dsn)
			if err == nil {
				t.Fatal("expected a parse error — without one this case proves nothing")
			}
			assertScrubbed(t, Err(err, dsn), dsn)
		})
	}
}

func assertScrubbed(t *testing.T, err error, dsn string) {
	t.Helper()
	if err == nil {
		t.Fatal("Err returned nil for a non-nil input")
	}
	if strings.Contains(err.Error(), canary) {
		t.Fatalf("N1: the DSN credential LEAKED into the error text (which main.go logs at boot and\n"+
			"the singleton logs on every re-acquire failure):\n%s", err.Error())
	}
	if strings.Contains(err.Error(), dsn) {
		t.Fatalf("the whole DSN survived in the error text:\n%s", err.Error())
	}
}

// TestDSNScrub_PgxStillLeaksAtThisPin is the ANTI-VACUITY control, and the important test in this
// file. It asserts the RAW driver error still leaks — i.e. that the test above defends something,
// rather than measuring a pgx version that never leaked or a trigger that never carries the DSN. If a
// pgx bump fixes redaction upstream, THIS test fails, and that failure is the signal to re-read
// pgconn/errors.go and decide whether the local scrub is still load-bearing — delete the premise, not
// just the assertion.
func TestDSNScrub_PgxStillLeaksAtThisPin(t *testing.T) {
	for name, dsn := range leakyDSNs() {
		if pgxRedactsThese[name] {
			continue
		}
		t.Run(name, func(t *testing.T) {
			_, err := pgxpool.ParseConfig(dsn)
			if err == nil {
				t.Fatalf("PREMISE CHANGED for %q: the trigger no longer produces a parse error at this pin.\n"+
					"Re-measure which trigger carries the DSN before touching anything — a trigger that does not\n"+
					"fire turns every test in this file vacuous, which is exactly what porting M1's\n"+
					"pool_max_conns trigger would have done.", name)
			}
			if !strings.Contains(err.Error(), canary) {
				t.Fatalf("PREMISE CHANGED for %q: the pinned pgx no longer leaks the credential in its own error.\n"+
					"raw error: %s\n"+
					"This is good news, not a bug — but re-read pgconn/errors.go redactPW/redactURL and decide\n"+
					"whether dsnscrub.Err is still needed before deleting anything. Do NOT just delete this assertion.",
					name, err.Error())
			}
		})
	}
}

// TestDSNScrub_CleanErrorsKeepTheirDetail is the other half of the anti-vacuity pairing: a scrub that
// fired on everything would "pass" every leak test above while destroying every diagnosis. The likelier
// real-world failure at these call sites is an unreachable database, whose message carries no
// credential — measured at this pin, pgx reports "failed to connect to <user=u database=db>" plus the
// resolver error, and that detail is what an operator needs.
//
// THE TARGET IS 127.0.0.1:1, NOT AN UNRESOLVABLE HOSTNAME. The first version aimed at
// nosuchhost.invalid and carried a t.Skip for the environment whose wildcard DNS resolver resolves it
// anyway — but tests/harness/run.sh counts a SKIP as a test that did not execute and turns the package
// red, correctly: a suite that can silently shrink to nothing is the defect this whole pass has been
// removing. Port 1 on loopback is refused deterministically, with no DNS involved, so there is no
// environment in which this case degrades into "not run".
func TestDSNScrub_CleanErrorsKeepTheirDetail(t *testing.T) {
	dsn := "postgres://u:" + canary + "@127.0.0.1:1/db?connect_timeout=1"
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("this DSN is supposed to PARSE — the point is a connect-stage error: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewWithConfig is lazy at this pin and should not fail: %v", err)
	}
	defer pool.Close()
	perr := pool.Ping(context.Background())
	if perr == nil {
		t.Fatal("expected a connect failure against 127.0.0.1:1 — something is listening on port 1, " +
			"so this case is not staged and must not be reported as passing")
	}
	scrubbed := Err(perr, dsn)
	if scrubbed.Error() != perr.Error() {
		t.Fatalf("a clean error was scrubbed, destroying the diagnosis an operator needs:\n"+
			"  raw:      %s\n  scrubbed: %s", perr.Error(), scrubbed.Error())
	}
	if strings.Contains(perr.Error(), canary) {
		t.Fatal("PREMISE CHANGED: the connect-stage error now carries the credential too, so this " +
			"case is no longer a clean error. Re-measure and move it to leakyDSNs().")
	}
}

// TestDSNScrub_CandidateCheckStandsAlone tests the DERIVED candidate check directly, with synthetic
// messages. It exists because that branch is NOT reachable through the driver at this pin — pgx echoes
// the whole DSN, so the whole-DSN containment check alone satisfies every driver-level test. Mutation
// testing on M1's equivalent showed deleting either check left every driver-level test green, which
// means a driver-level test can never be the proof for either branch.
func TestDSNScrub_CandidateCheckStandsAlone(t *testing.T) {
	sentinel := errors.New("sentinel")
	cases := map[string]struct{ dsn, msg string }{
		"url query": {
			dsn: "postgres://u@h:5432/db?password=" + canary,
			// A message that names the credential WITHOUT quoting the whole DSN — a future pgx, a
			// wrapped error, or a pool-option error that names one field.
			msg: "some future driver error mentioning " + canary + " on its own",
		},
		"kv spaced": {
			dsn: "host=h password = " + canary + " dbname=db",
			msg: "another shape: " + canary,
		},
		"url userinfo": {
			dsn: "postgres://u:" + canary + "@h:5432/db",
			msg: "userinfo leaked as " + canary,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := Err(&wrapped{sentinel, c.msg}, c.dsn)
			if strings.Contains(got.Error(), canary) {
				t.Fatalf("the candidate check did not fire: %s", got.Error())
			}
			if !errors.Is(got, sentinel) {
				t.Fatal("Unwrap did not survive scrubbing — errors.Is/As must still reach the original")
			}
		})
	}
}

// TestDSNScrub_WholeDSNCheckStandsAlone pins the OTHER branch on its own, for the same reason: a
// message that quotes the whole DSN but whose credential is too short to be a candidate (<4 chars)
// must still be scrubbed.
func TestDSNScrub_WholeDSNCheckStandsAlone(t *testing.T) {
	dsn := "postgres://u:ab@h:5432/db"
	got := Err(errors.New("cannot parse `"+dsn+"`: whatever"), dsn)
	if strings.Contains(got.Error(), dsn) {
		t.Fatalf("the whole-DSN check did not fire: %s", got.Error())
	}
}

// TestDSNScrub_ShortValuesDoNotSuppressEverything is the counterweight to the over-collection in
// credentialCandidates. Ports and short database names appear in ordinary driver messages; if they
// counted as candidates, every error in the process would be replaced by the suppression notice.
func TestDSNScrub_ShortValuesDoNotSuppressEverything(t *testing.T) {
	dsn := "host=h port=5432 dbname=db user=u"
	msg := "dial tcp 10.0.0.5:5432: connect: connection refused"
	got := Err(errors.New(msg), dsn)
	if got.Error() != msg {
		t.Fatalf("a message containing only short DSN values was suppressed, which would cost every\n"+
			"connection error its diagnosis:\n  raw:      %s\n  scrubbed: %s", msg, got.Error())
	}
}

// TestDSNScrub_StructuralValuesNeverSuppress is the generalisation of the bug the test above found.
// Every case is a message an operator NEEDS, containing a structural DSN value and no credential. The
// first version of credentialCandidates collected every bare token, so `port=5432` made "5432" a
// candidate and the connection-refused message — the single most common database error there is — was
// replaced by the suppression notice. Both spellings are covered, because the whole point of tracking
// the pending key is that `port = 5432` must behave like `port=5432`.
func TestDSNScrub_StructuralValuesNeverSuppress(t *testing.T) {
	cases := map[string]struct{ dsn, msg string }{
		"port, kv unspaced": {
			dsn: "host=db.internal port=5432 dbname=sendmanaged user=send_managed_app",
			msg: "dial tcp 10.0.0.5:5432: connect: connection refused",
		},
		"port, kv SPACED": {
			dsn: "host = db.internal port = 5432 dbname = sendmanaged",
			msg: "dial tcp 10.0.0.5:5432: connect: connection refused",
		},
		"hostname": {
			dsn: "host=db.internal port=5432 dbname=sendmanaged",
			msg: "hostname resolving error: lookup db.internal: no such host",
		},
		"dbname and user": {
			dsn: "host=h port=5432 dbname=sendmanaged user=send_managed_app",
			msg: `failed to connect to \"user=send_managed_app database=sendmanaged\": server closed the connection`,
		},
		"url form, structural only": {
			dsn: "postgres://send_managed_app@db.internal:5432/sendmanaged?sslmode=require&connect_timeout=10",
			msg: "dial tcp 10.0.0.5:5432: i/o timeout",
		},
		"the literal word password in a SERVER error": {
			// `password` as a bare key must not itself become a candidate, or every authentication
			// failure — the error most likely to be a real misconfiguration — is suppressed.
			dsn: "host=h port=5432 dbname=db user=u password = " + canary,
			msg: `failed to connect: password authentication failed for user "u"`,
		},
		"sslmode value": {
			dsn: "host=h port=5432 dbname=db sslmode=require",
			msg: "failed to configure TLS (sslmode is invalid: require)",
		},
		"pool option value": {
			dsn: "postgres://u@h:5432/db?pool_max_conns=10",
			msg: "cannot parse pool_max_conns: too many connections (10)",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got := Err(errors.New(c.msg), c.dsn)
			if got.Error() != c.msg {
				t.Fatalf("a message with NO credential in it was suppressed. An operator loses this\n"+
					"diagnosis entirely, which for connection-refused means every DB outage looks like a\n"+
					"DSN syntax error:\n  dsn:      %s\n  raw:      %s\n  scrubbed: %s", c.dsn, c.msg, got.Error())
			}
		})
	}
}

// TestDSNScrub_SpacedCredentialStillCaught is the counterweight to the test above: making structural
// values safe must NOT have made the spaced credential spelling safe too. This is the exact shape pgx
// itself fails to redact, so it is the one that most needs to survive the fix.
func TestDSNScrub_SpacedCredentialStillCaught(t *testing.T) {
	for name, dsn := range map[string]string{
		"password spaced":            "host=h port=5432 dbname=db password = " + canary,
		"sslpassword spaced":         "host=h port=5432 dbname=db sslpassword = " + canary,
		"password spaced, quoted":    "host=h port=5432 dbname=db password = '" + canary + "'",
		"password unspaced":          "host=h port=5432 dbname=db password=" + canary,
		"unknown keyword, spaced":    "host=h port=5432 dbname=db future_secret = " + canary,
		"unknown keyword, unspaced":  "host=h port=5432 dbname=db future_secret=" + canary,
		"url query unknown keyword":  "postgres://u@h:5432/db?future_secret=" + canary,
		"url query sslpassword only": "postgres://u@h:5432/db?sslpassword=" + canary,
	} {
		t.Run(name, func(t *testing.T) {
			// A message that names the credential WITHOUT quoting the whole DSN, so only the derived
			// candidate check can catch it.
			got := Err(errors.New("driver said: "+canary), dsn)
			if strings.Contains(got.Error(), canary) {
				t.Fatalf("the credential was NOT caught for %q — making structural values safe must not\n"+
					"make credentials safe. dsn: %s\n  got: %s", name, dsn, got.Error())
			}
		})
	}
}

// TestDSNScrub_NilIsNil — Err composes at a return site without a guard, so nil must pass through.
func TestDSNScrub_NilIsNil(t *testing.T) {
	if Err(nil, "postgres://u:"+canary+"@h/db") != nil {
		t.Fatal("Err(nil) must be nil")
	}
}

type wrapped struct {
	err error
	msg string
}

func (w *wrapped) Error() string { return w.msg }
func (w *wrapped) Unwrap() error { return w.err }
