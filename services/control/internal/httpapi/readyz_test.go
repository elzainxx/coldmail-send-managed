package httpapi

// Behavioral tests for /readyz's uniform envelope (fix-plan item 10 / contract §1.9).
//
// WHAT ITEM 10 CHANGED HERE. This surface already checked the right things — DB reachable AND the
// singleton lock genuinely held — and already answered 200/503 honestly. What it did not do was
// answer in a shape any other module shared: it returned {status, db, singleton_lock, at} while
// Module 1 returned {ready, config, pepper, supabaseKey}, so one dashboard needed one parser per
// module. The `checks{}` map is the fix: the envelope is uniform, the dependency NAMES live inside
// it, and a consumer walks the map instead of knowing which module it is talking to.
//
// So the properties worth testing are NOT "does readyz report the DB" (it always did, and
// leadership_test.go already covers the lock semantics against real Postgres). They are:
//
//  1. the two copies of each boolean — nested in checks{} and top-level — can never disagree;
//  2. every pre-existing top-level field is still there, un-nested (Rule 5, and operator-runbook.md#monitoring-and-alerting
//     tells a live uptime monitor to alert on those exact top-level names);
//  3. checks{} tracks each dependency SEPARATELY, so a mixed state renders correctly rather than
//     collapsing to one boolean;
//  4. the top-level fields remain distinguishable from the nested copy in the RAW body, because
//     the harness greps the body as text and adding checks{} gave it a second match.
//
// REAL INFRA, NOT FAKES (Rule 3). h.db is a concrete *db.DB and singleton.Guard's held flag is
// unexported, so there is no seam to fake from this package even if faking were allowed — the
// ready side of every assertion below runs against real Postgres. It needs no migrated template:
// Ping needs a reachable pool and pg_try_advisory_lock needs no tables, so these tests create a
// plain throwaway database. That is also why the harness can keep running this package before any
// template is built.
//
// The throwaway database is REQUIRED rather than reusing `platform` directly: singleton.LockKey is
// session-scoped per database, and the live stack's own Control holds it on `platform` while the
// harness runs. Acquiring there would block on a lock a passing test must not be able to steal.
//
// FAIL-VERIFIED — four breaks in health.go, each applied, run, then reverted and hash-proven
// byte-identical (health.go md5 77ede4b49bb05c5f85d3696be1a0002e). OBSERVED results, not predicted:
//   - checks{} removed entirely -> 5 tests: CarriesTheContractEnvelope,
//     NilDependenciesReportFalseNotAbsent, ChecksMapAgrees... (all 3 subtests),
//     ChecksMapTracksEachDependencySeparately, TopLevelFieldsAreDistinguishable...
//   - checks{} built from `dbOK && lockOK` for BOTH keys — the collapse this item exists to
//     prevent -> 2 tests, and only the two mixed-state subtests of ChecksMapAgrees fire while
//     both_up still passes. That asymmetry is the point: an all-up probe cannot detect a
//     collapsed map, which is why the mixed states are constructed at all.
//   - top-level "db"/"singleton_lock" removed, leaving only the nested copy — the plausible
//     "clean up the duplication" edit -> 3 tests: ChecksMapAgrees...,
//     PreExistingTopLevelFieldsAreStillThere, TopLevelFieldsAreDistinguishable...
//   - checks{} given a nested sub-object -> 5 tests, including
//     TopLevelFieldsAreDistinguishable... reporting that the harness's sed can no longer strip it.
//
// A FIFTH break is at the harness level, since the sed in tests/harness/run.sh is part of this
// change: against a body with the top-level fields deleted but checks{} kept, the OLD bare
// `grep '"singleton_lock":true'` still PASSES (it matches the nested copy) while the new
// strip-then-grep FAILS. That is the whole reason the sed was added — item 10 gave that grep a
// second place to match, which would have made a pre-existing anti-vacuity check vacuous.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/coldmail-send-managed/control/internal/db"
	"github.com/coldmail-send-managed/control/internal/singleton"
)

// readyzProbe drives the REAL mux the constructor builds (not handleReadyz directly), so route
// registration is part of what is asserted.
func readyzProbe(t *testing.T, database *db.DB, guard *singleton.Guard, path string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewHealthServer(":0", database, guard, nil, nil)
	rec := httptest.NewRecorder()
	h.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// readyzBody decodes the body and fails loudly if it is not an object — a caller that cannot parse
// this is exactly the failure mode item 10 exists to remove.
func readyzBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("readyz body is not a JSON object (%v): %s", err, rec.Body.String())
	}
	return m
}

// checksOf pulls checks{} and fails loudly when it is absent or the wrong shape, rather than
// returning a zero map — an absent checks{} must fail every test that reads it, not silently
// report "no checks are false".
func checksOf(t *testing.T, body map[string]any) map[string]bool {
	t.Helper()
	raw, present := body["checks"]
	if !present {
		t.Fatalf("readyz has no `checks` object — the contract's envelope requires it: %v", body)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("readyz `checks` is %T, must be an object of name->bool: %v", raw, raw)
	}
	out := make(map[string]bool, len(obj))
	for k, v := range obj {
		b, ok := v.(bool)
		if !ok {
			t.Fatalf("readyz checks[%q] is %T (%v), must be a bool — a consumer renders these directly", k, v, v)
		}
		out[k] = b
	}
	return out
}

// newThrowawayDB creates a plain database (no template, no migrations — readyz needs neither) and
// returns a pool on it plus its DSN for the guard. Skips when the admin DSN is absent, exactly as
// every other real-infra suite in this repo does.
func newThrowawayDB(t *testing.T, suffix string) (*db.DB, string) {
	t.Helper()
	adminDSN := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("TEST_ADMIN_DATABASE_URL not set; skipping real-infra readyz test")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	defer admin.Close(ctx)

	name := "test_readyz_" + suffix
	if _, err := admin.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %q", name)); err != nil {
		t.Fatalf("drop stale throwaway db: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf("CREATE DATABASE %q", name)); err != nil {
		t.Fatalf("create throwaway db: %v", err)
	}
	dsn := strings.Replace(adminDSN, "/platform", "/"+name, 1)
	if dsn == adminDSN {
		t.Fatalf("could not point the DSN at the throwaway db (admin DSN has no /platform path): %s",
			regexp.MustCompile(`://[^@]*@`).ReplaceAllString(adminDSN, "://***@"))
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), adminDSN)
		if err != nil {
			return
		}
		defer c.Close(context.Background())
		_, _ = c.Exec(context.Background(),
			"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()", name)
		_, _ = c.Exec(context.Background(), fmt.Sprintf("DROP DATABASE IF EXISTS %q", name))
	})

	connCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	pool, err := db.Connect(connCtx, dsn)
	if err != nil {
		t.Fatalf("connect throwaway db: %v", err)
	}
	return pool, dsn
}

func acquireGuard(t *testing.T, dsn string) *singleton.Guard {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	g, err := singleton.Acquire(ctx, dsn, 5*time.Second)
	if err != nil {
		t.Fatalf("acquire singleton lock on the throwaway db: %v", err)
	}
	if !g.Healthy(ctx) {
		t.Fatalf("fixture broken: Acquire returned a guard that is not Healthy — every 'lock held' case below would silently become 'lock lost', and since lock-lost is ALSO not-ready they would pass for the wrong reason")
	}
	return g
}

// TestReadyz_CarriesTheContractEnvelope: status + checks + at, all three, on the same body.
// Needs no infrastructure — the envelope must be present in the NOT-ready state too, which is the
// state a consumer is most likely to be parsing.
func TestReadyz_CarriesTheContractEnvelope(t *testing.T) {
	body := readyzBody(t, readyzProbe(t, nil, nil, "/readyz"))

	if got, ok := body["status"].(string); !ok || (got != "ready" && got != "not_ready") {
		t.Errorf("status must be the word \"ready\" or \"not_ready\" (contract §1.9), got %#v", body["status"])
	}
	checksOf(t, body) // fails loudly if absent or wrong-shaped
	at, ok := body["at"].(string)
	if !ok {
		t.Fatalf("at must be an RFC3339 string, got %T (%v)", body["at"], body["at"])
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Errorf("at %q does not parse as RFC3339 (%v) — a dashboard cannot tell a live answer from a cached one", at, err)
	}
}

// TestReadyz_NilDependenciesReportFalseNotAbsent: an unwired dependency is reported FALSE, not
// omitted. An absent check would let a monitor read "no news is good news" off a body that is
// describing an unserviceable process.
func TestReadyz_NilDependenciesReportFalseNotAbsent(t *testing.T) {
	rec := readyzProbe(t, nil, nil, "/readyz")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("nil db + nil guard must be 503, got %d", rec.Code)
	}
	body := readyzBody(t, rec)
	checks := checksOf(t, body)
	for _, name := range []string{"db", "singleton_lock"} {
		v, present := checks[name]
		if !present {
			t.Errorf("checks[%q] is ABSENT with the dependency unwired; it must be present and false", name)
			continue
		}
		if v {
			t.Errorf("checks[%q] is true with the dependency unwired", name)
		}
	}
	if body["status"] != "not_ready" {
		t.Errorf("status must be not_ready with both dependencies down, got %v", body["status"])
	}
}

// TestReadyz_ChecksMapAgreesWithTheTopLevelFields is the anti-drift test. Both copies are derived
// from the same two variables today, so this passes trivially — which is the point: it fails the
// moment someone adds a dependency to only one of the two places, or nests one and forgets the
// other. It runs the matrix against REAL Postgres so the true values are genuinely different
// across subtests rather than all-false.
func TestReadyz_ChecksMapAgreesWithTheTopLevelFields(t *testing.T) {
	database, dsn := newThrowawayDB(t, "agree")
	guard := acquireGuard(t, dsn)

	assertAgrees := func(t *testing.T, wantDB, wantLock bool) {
		t.Helper()
		rec := readyzProbe(t, database, guard, "/readyz")
		body := readyzBody(t, rec)
		checks := checksOf(t, body)

		for name, wantVal := range map[string]bool{"db": wantDB, "singleton_lock": wantLock} {
			if checks[name] != wantVal {
				t.Errorf("checks[%q] = %v, want %v", name, checks[name], wantVal)
			}
			top, ok := body[name].(bool)
			if !ok {
				t.Fatalf("top-level %q is %T, must still be a bool (Rule 5: additive only)", name, body[name])
			}
			if top != checks[name] {
				t.Errorf("top-level %q = %v but checks[%q] = %v — the two copies DISAGREE, so a dashboard and an uptime monitor reading the same body would report different states",
					name, top, name, checks[name])
			}
		}

		wantReady := wantDB && wantLock
		wantStatus := "not_ready"
		wantCode := http.StatusServiceUnavailable
		if wantReady {
			wantStatus, wantCode = "ready", http.StatusOK
		}
		if body["status"] != wantStatus {
			t.Errorf("status = %v, want %q (db=%v lock=%v)", body["status"], wantStatus, wantDB, wantLock)
		}
		if rec.Code != wantCode {
			t.Errorf("HTTP %d, want %d (db=%v lock=%v)", rec.Code, wantCode, wantDB, wantLock)
		}
		// The status word must be the AND of the checks map, not an independent value: this is what
		// makes checks{} load-bearing rather than decorative.
		allTrue := true
		for _, v := range checks {
			allTrue = allTrue && v
		}
		if allTrue != wantReady {
			t.Errorf("the AND of checks{} is %v but the body claims ready=%v", allTrue, wantReady)
		}
	}

	// Sequenced deliberately: each step CHANGES a real dependency's state rather than constructing a
	// different server, so the values under test are produced the way production produces them.
	t.Run("both_up", func(t *testing.T) { assertAgrees(t, true, true) })

	t.Run("db_up_lock_released", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		guard.Release(ctx)
		assertAgrees(t, true, false)
	})

	t.Run("db_closed_lock_reacquired", func(t *testing.T) {
		guard = acquireGuard(t, dsn) // the lock is free again after Release
		database.Close()
		assertAgrees(t, false, true)
	})
}

// TestReadyz_ChecksMapTracksEachDependencySeparately: the mixed state must render as a mixed state.
// Collapsing checks{} to one overall boolean per key would still pass every all-up/all-down test,
// and would destroy the only reason to have a map at all — telling an operator WHICH dependency is
// the problem. operator-runbook.md#monitoring-and-alerting's two distinct alert paragraphs depend on this.
func TestReadyz_ChecksMapTracksEachDependencySeparately(t *testing.T) {
	database, dsn := newThrowawayDB(t, "separate")
	guard := acquireGuard(t, dsn)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	guard.Release(ctx) // DB stays reachable; only the lock goes away

	checks := checksOf(t, readyzBody(t, readyzProbe(t, database, guard, "/readyz")))
	if !checks["db"] {
		t.Errorf("checks[db] is false while Postgres is reachable — the map is not reporting this dependency, it is echoing the overall verdict")
	}
	if checks["singleton_lock"] {
		t.Errorf("checks[singleton_lock] is true after Release — the map is not reporting this dependency")
	}
}

// TestReadyz_PreExistingTopLevelFieldsAreStillThere pins Rule 5 on an operator surface. These are
// not hypothetical consumers: Docs/operator-runbook.md#monitoring-and-alerting §2 tells an uptime monitor to alert on `db:false`
// or `singleton_lock:false` at the TOP level, and operator-runbook.md hangs a recovery procedure
// off the field name. Nesting them under checks{} would break a live monitor silently — it would
// look like a successful refactor until an incident went unnoticed.
func TestReadyz_PreExistingTopLevelFieldsAreStillThere(t *testing.T) {
	body := readyzBody(t, readyzProbe(t, nil, nil, "/readyz"))
	for _, name := range []string{"status", "db", "singleton_lock", "at"} {
		if _, present := body[name]; !present {
			t.Errorf("top-level %q is GONE; operator-runbook.md#monitoring-and-alerting and operator-runbook.md both document it — this change must be additive", name)
		}
	}
}

// TestReadyz_TopLevelFieldsAreDistinguishableFromTheNestedCopy guards a consequence of item 10 that
// is invisible in the decoded body: the harness asserts `"singleton_lock":true` by GREPPING the raw
// response, and checks{} gave that grep a second place to match. So the grep would keep passing even
// if the top-level field were deleted — the anti-vacuity check would itself have become vacuous.
//
// The harness therefore strips the checks object first (`sed 's/"checks":{[^}]*}//'`) and greps what
// remains. That sed is only correct while checks{} is a FLAT object; this test pins both halves —
// the raw body really does contain a brace-free checks object, and stripping it really does leave a
// top-level match behind.
func TestReadyz_TopLevelFieldsAreDistinguishableFromTheNestedCopy(t *testing.T) {
	database, dsn := newThrowawayDB(t, "rawgrep")
	guard := acquireGuard(t, dsn)

	raw := readyzProbe(t, database, guard, "/readyz").Body.String()

	flatChecks := regexp.MustCompile(`"checks":\{[^{}]*\}`)
	if !flatChecks.MatchString(raw) {
		t.Fatalf("checks{} is not a flat brace-free object in the raw body, so the harness's `sed 's/\"checks\":{[^}]*}//'` cannot strip it and its singleton_lock grep is ambiguous: %s", raw)
	}
	stripped := flatChecks.ReplaceAllString(raw, "")
	if strings.Contains(stripped, `"checks"`) {
		t.Errorf("stripping the checks object left a `checks` fragment behind: %s", stripped)
	}
	if !strings.Contains(stripped, `"singleton_lock":true`) {
		t.Errorf("with checks{} stripped, the raw body no longer contains a top-level \"singleton_lock\":true — the harness's anti-vacuity grep would be asserting the NESTED copy only: %s", stripped)
	}
	if !strings.Contains(stripped, `"db":true`) {
		t.Errorf("with checks{} stripped, the raw body no longer contains a top-level \"db\":true: %s", stripped)
	}
}

// TestHealthz_IsLivenessNotReadiness: liveness must stay 200 with every dependency down. If health
// ever started reporting dependencies, operator-runbook.md#monitoring-and-alerting's central instruction — probe /readyz, not
// /healthz, because /healthz misses a lost singleton lock entirely — would become wrong.
func TestHealthz_IsLivenessNotReadiness(t *testing.T) {
	rec := readyzProbe(t, nil, nil, "/healthz")
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz must be 200 with every dependency down (it is liveness), got %d", rec.Code)
	}
	body := readyzBody(t, rec)
	if body["status"] != "ok" {
		t.Errorf("/healthz status = %v, want \"ok\"", body["status"])
	}
	if _, present := body["checks"]; present {
		t.Errorf("/healthz must NOT carry checks{} — it does not check anything, and a checks map there would invite pointing an uptime monitor at the wrong endpoint")
	}
}
