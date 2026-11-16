package httpapi

// Behavioral tests for the ROOT mux's coded 404 (fix-plan item 7b / audit D17).
//
// The two sub-surfaces (adminapi, platformapi) each own method dispatch for their own routes. This
// mux is the layer ABOVE them, and it had the same defect for a different reason: any path it did
// not route — an unknown path, or a KNOWN path whose surface is unmounted because its credential is
// unconfigured — got ServeMux's `404 page not found` as text/plain with no code and no request_id.
// So whether a client's `error.code` branch worked depended on which path it typo'd.
//
// These are the first tests in this package. They drive the real handler the constructor builds
// (h.http.Handler) over real HTTP. db and guard are nil, which the no-route path never touches; the
// health routes are covered by the harness's live-stack checks and the drain_grace gate, and the two
// tests below that DO hit /healthz assert only routing, not readiness.

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// newRootTestServer starts the real root mux with the given sub-surfaces (nil = unmounted).
func newRootTestServer(t *testing.T, admin, platform http.Handler) *httptest.Server {
	t.Helper()
	h := NewHealthServer(":0", nil, nil, admin, platform)
	ts := httptest.NewServer(h.http.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func getRoot(t *testing.T, ts *httptest.Server, method, path string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(method, ts.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(raw)
}

func rootErrField(t *testing.T, body, field string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("body is not JSON (%v): %s", err, body)
	}
	obj, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("body has no error object, so a client's body.error.%s nil-derefs: %s", field, body)
	}
	s, _ := obj[field].(string)
	return s
}

// TestRootMux_UnknownPathIsACodedEnvelopeNotPlainText is the flagship control.
func TestRootMux_UnknownPathIsACodedEnvelopeNotPlainText(t *testing.T) {
	ts := newRootTestServer(t, nil, nil)
	ridRe := regexp.MustCompile(`^req_[0-9a-f]{32}$`)

	for _, path := range []string{"/", "/nope", "/v1", "/v1/", "/v2/quote", "/HEALTHZ", "/healthz/x"} {
		status, header, body := getRoot(t, ts, "GET", path)
		if status != http.StatusNotFound {
			t.Fatalf("GET %s: status=%d want 404; body=%s", path, status, body)
		}
		if ct := header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Fatalf("GET %s: Content-Type=%q want application/json", path, ct)
		}
		if strings.Contains(body, "page not found") {
			t.Fatalf("GET %s still carries ServeMux's plain text: %s", path, body)
		}
		if code := rootErrField(t, body, "code"); code != "not_found" {
			t.Fatalf("GET %s: error.code=%q want %q", path, code, "not_found")
		}
		if msg := rootErrField(t, body, "message"); msg == "" {
			t.Fatalf("GET %s: error.message is empty: %s", path, body)
		}
		if rid := rootErrField(t, body, "request_id"); !ridRe.MatchString(rid) {
			t.Fatalf("GET %s: request_id=%q does not match %s — the contract says request_id is "+
				"mandatory on EVERY error, and it must be the same shape the API packages mint so "+
				"one log-grep serves all three surfaces", path, rid, ridRe)
		}
	}
}

// TestRootMux_UnmountedSurfacePathAnswersTheEnvelopeToo covers the case that makes this fallback
// worth having separately from the sub-surfaces': when a credential is unconfigured, its surface is
// deliberately not mounted (fail-closed). Those paths then reach THIS mux, so before item 7b a
// production deployment missing one credential answered plain text on its real, documented routes.
func TestRootMux_UnmountedSurfacePathAnswersTheEnvelopeToo(t *testing.T) {
	ts := newRootTestServer(t, nil, nil) // both surfaces unmounted
	for _, path := range []string{
		"/v1/quote", "/v1/run", "/v1/report", "/v1/describe", "/v1/cancel", // platform, unmounted
		"/v1/admin/accounts", "/v1/admin/config", // admin, unmounted
	} {
		status, _, body := getRoot(t, ts, "GET", path)
		if status != http.StatusNotFound {
			t.Fatalf("GET %s with its surface unmounted: status=%d want 404; body=%s", path, status, body)
		}
		if code := rootErrField(t, body, "code"); code != "not_found" {
			t.Fatalf("GET %s: code=%q want not_found; body=%s", path, code, body)
		}
		if rootErrField(t, body, "request_id") == "" {
			t.Fatalf("GET %s: no request_id; body=%s", path, body)
		}
	}
}

// TestRootMux_EveryVerbOnAnUnknownPathIsTheSameEnvelope — this mux has no route table, so it makes no
// 405-vs-404 distinction by design (the sub-surfaces own that, being the only layer that knows which
// methods a path serves). Asserting the SAMENESS pins that as a decision rather than an accident.
func TestRootMux_EveryVerbOnAnUnknownPathIsTheSameEnvelope(t *testing.T) {
	ts := newRootTestServer(t, nil, nil)
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		status, header, body := getRoot(t, ts, method, "/nope")
		if status != http.StatusNotFound {
			t.Fatalf("%s /nope: status=%d want 404; body=%s", method, status, body)
		}
		if code := rootErrField(t, body, "code"); code != "not_found" {
			t.Fatalf("%s /nope: code=%q want not_found", method, code)
		}
		if allow := header.Get("Allow"); allow != "" {
			t.Fatalf("%s /nope: carries Allow=%q — this mux has no route table and must not imply "+
				"the path exists with other verbs", method, allow)
		}
	}
}

// TestRootMux_RequestIDIsUniquePerRequest — a constant id would correlate every miss to one log line.
func TestRootMux_RequestIDIsUniquePerRequest(t *testing.T) {
	ts := newRootTestServer(t, nil, nil)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		_, _, body := getRoot(t, ts, "GET", "/nope")
		rid := rootErrField(t, body, "request_id")
		if seen[rid] {
			t.Fatalf("request_id %q reissued on request %d", rid, i)
		}
		seen[rid] = true
	}
	if len(seen) != 5 {
		t.Fatalf("%d distinct ids from 5 requests", len(seen))
	}
}

// TestRootMux_CatchAllDoesNotShadowAMountedRoute is the containment control: `/` matches everything
// in a Go mux, so a fallback registered carelessly could swallow real traffic. Every mounted route
// must still reach its own surface.
func TestRootMux_CatchAllDoesNotShadowAMountedRoute(t *testing.T) {
	// Sentinel handlers standing in for the two API servers.
	adminHit, platformHit := false, false
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		adminHit = true
		w.WriteHeader(http.StatusTeapot)
	})
	platform := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		platformHit = true
		w.WriteHeader(http.StatusTeapot)
	})
	ts := newRootTestServer(t, admin, platform)

	for _, path := range []string{"/v1/admin/accounts", "/v1/admin/anything/at/all"} {
		adminHit = false
		if status, _, body := getRoot(t, ts, "GET", path); status != http.StatusTeapot || !adminHit {
			t.Fatalf("GET %s: status=%d adminHit=%v — the catch-all swallowed a mounted admin route; body=%s",
				path, status, adminHit, body)
		}
	}
	for _, path := range []string{"/v1/quote", "/v1/run", "/v1/report", "/v1/describe", "/v1/cancel"} {
		platformHit = false
		if status, _, body := getRoot(t, ts, "GET", path); status != http.StatusTeapot || !platformHit {
			t.Fatalf("GET %s: status=%d platformHit=%v — the catch-all swallowed a mounted platform route; body=%s",
				path, status, platformHit, body)
		}
	}
	// And the health routes, which are registered method-less and must never fall through.
	for _, method := range []string{"GET", "HEAD", "POST"} {
		if status, _, body := getRoot(t, ts, method, "/healthz"); status != http.StatusOK {
			t.Fatalf("%s /healthz: status=%d want 200 — the catch-all swallowed a health route "+
				"(they are method-less, so every verb must reach the handler); body=%s", method, status, body)
		}
	}
}

// TestRootMux_ForgedPathCannotSplitALogLine is the end-to-end control on the log-forgery fix. This
// fallback is the WIDEST exposure of the three: it logs an ARBITRARY unmatched path, so a caller
// chooses the text that reaches the log line.
func TestRootMux_ForgedPathCannotSplitALogLine(t *testing.T) {
	ts := newRootTestServer(t, nil, nil)

	var buf strings.Builder
	oldOut, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0) // no timestamp, so a line count is exactly an entry count
	t.Cleanup(func() { log.SetOutput(oldOut); log.SetFlags(oldFlags) })

	status, _, _ := getRoot(t, ts,
		"GET", "/x%0Acontrol%20request_id=FORGED%20outcome=error%20code=internal_error%20level=error")
	if status != http.StatusNotFound {
		t.Fatalf("forged path: status=%d want 404", status)
	}

	out := strings.TrimRight(buf.String(), "\n")
	if out == "" {
		t.Fatal("the fallback logged nothing — an unrouted path must be observable, and this test " +
			"would otherwise pass vacuously")
	}
	if lines := strings.Split(out, "\n"); len(lines) != 1 {
		t.Fatalf("one request produced %d log lines:\n%s\nA decoded %%0A split the line and the "+
			"remainder is indistinguishable from a genuine entry with an attacker-chosen request_id.",
			len(lines), out)
	}
}

// TestRootMux_ForgedPathIsNotReflectedIntoTheBody — the body must echo nothing the caller sent, so a
// forged path cannot become client-visible content (or a stored-XSS vector in whatever renders it).
func TestRootMux_ForgedPathIsNotReflectedIntoTheBody(t *testing.T) {
	ts := newRootTestServer(t, nil, nil)
	_, _, body := getRoot(t, ts, "GET", "/nope-%3Cscript%3Ealert(1)%3C/script%3E")
	for _, marker := range []string{"script", "alert", "nope"} {
		if strings.Contains(body, marker) {
			t.Fatalf("the 404 body reflects the caller's path (%q): %s", marker, body)
		}
	}
}

// TestSanitizeLogPath_NeutralizesControlCharacters is the unit half of the forgery fix.
func TestSanitizeLogPath_NeutralizesControlCharacters(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/healthz", "/healthz"},
		{"/x?y=1", "/x"},
		{"/x\ncontrol request_id=FORGED", "/x?control request_id=FORGED"},
		{"/x\r\ny", "/x??y"},
		{"/x\ty", "/x?y"},
		{"/x\x00y", "/x?y"},
		{"/x\x7fy", "/x?y"},
	}
	for _, c := range cases {
		got := sanitizeLogPath(c.in)
		if got != c.want {
			t.Fatalf("sanitizeLogPath(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.ContainsAny(got, "\r\n") {
			t.Fatalf("sanitizeLogPath(%q) = %q still contains a line break", c.in, got)
		}
	}
}

// TestNewRequestID_ShapeMatchesTheOtherSurfaces — one log-grep pattern must serve all three surfaces,
// which is only true if this mints the same shape.
func TestNewRequestID_ShapeMatchesTheOtherSurfaces(t *testing.T) {
	ridRe := regexp.MustCompile(`^req_[0-9a-f]{32}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newRequestID()
		if !ridRe.MatchString(id) {
			t.Fatalf("newRequestID() = %q, want %s", id, ridRe)
		}
		if seen[id] {
			t.Fatalf("newRequestID() repeated %q within 100 calls", id)
		}
		seen[id] = true
	}
}
