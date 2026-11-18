package workerproto

// reconnect_dials_test.go — Phase 3 close-out B1 and B3 on the worker wire: the three reconnect dials on
// register_ack, the optional plain-text part on push_job, and the pin that the Go and Node halves of the dials'
// legal ranges and defaults are the SAME numbers (the worker has no database, so the Control delivers the dials
// and the worker re-validates them; if the two ranges drifted, a value one side accepts would be silently
// dropped by the other).

import (
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func intp(n int) *int { return &n }

func TestRegisterAck_ReconnectDialsAreOmittedWhenNotDelivered_AndZeroJitterIsARealValue(t *testing.T) {
	// not delivered (an unreadable or out-of-range row): the frame is the one an older Control sent
	raw, err := json.Marshal(RegisterAck{Type: TypeRegisterAck, RequestID: "r", Accepted: true, OutcomeBufferTTLSec: 900})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "reconnect_") {
		t.Fatalf("an undelivered dial leaked into the frame: %s", raw)
	}
	// delivered, including a jitter of exactly 0 (the legal minimum), which omitempty on a plain int would drop
	raw, err = json.Marshal(RegisterAck{Type: TypeRegisterAck, RequestID: "r", Accepted: true,
		ReconnectBaseMS: intp(50), ReconnectCapMS: intp(200), ReconnectJitterPct: intp(0)})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]float64{"reconnect_base_ms": 50, "reconnect_cap_ms": 200, "reconnect_jitter_pct": 0} {
		v, ok := got[k].(float64)
		if !ok || v != want {
			t.Fatalf("frame field %s = %v (present=%v), want %v: %s", k, got[k], ok, want, raw)
		}
	}
}

func TestSend_TextIsOmittedWhenEmpty_AndPresentWhenSet(t *testing.T) {
	without, err := json.Marshal(Send{Recipient: "r@example.com", FromAddress: "s@example.org", Subject: "s", Body: "<p>b</p>", Headers: map[string]string{"To": "r@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), `"text"`) {
		t.Fatalf("a Send without a text part carries a \"text\" key (it must be the frame an older Control sent): %s", without)
	}
	with, err := json.Marshal(Send{Recipient: "r@example.com", FromAddress: "s@example.org", Subject: "s", Body: "<p>b</p>", Text: "plain\r\ntext", Headers: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(with, &m); err != nil {
		t.Fatal(err)
	}
	if m["text"] != "plain\r\ntext" {
		t.Fatalf("send.text = %v, want the text verbatim: %s", m["text"], with)
	}
	// every other field is identical with and without the text (the text part adds a key, changes none)
	var w map[string]any
	_ = json.Unmarshal(without, &w)
	delete(m, "text")
	if len(m) != len(w) {
		t.Fatalf("the text part changed the other fields: with=%v without=%v", m, w)
	}
}

// jsNumbers reads `[lo, hi]` pairs and defaults out of services/worker/src/reconnect.js.
func jsLimit(t *testing.T, js, key string) (lo, hi int) {
	t.Helper()
	m := regexp.MustCompile(key + `:\s*Object\.freeze\(\[(\d+),\s*(\d+)\]\)`).FindStringSubmatch(js)
	if m == nil {
		t.Fatalf("could not read RECONNECT_LIMITS.%s from services/worker/src/reconnect.js", key)
	}
	lo, _ = strconv.Atoi(m[1])
	hi, _ = strconv.Atoi(m[2])
	return lo, hi
}

func jsDefault(t *testing.T, js, key string) int {
	t.Helper()
	m := regexp.MustCompile(`RECONNECT_DEFAULTS = Object\.freeze\(\{[^}]*\b` + key + `:\s*(\d+)`).FindStringSubmatch(js)
	if m == nil {
		t.Fatalf("could not read RECONNECT_DEFAULTS.%s from services/worker/src/reconnect.js", key)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func TestReconnectDials_GoAndNodeAgree(t *testing.T) {
	jsBytes, err := os.ReadFile("../../../worker/src/reconnect.js")
	if err != nil {
		t.Fatalf("read the worker's reconnect.js: %v", err)
	}
	js := string(jsBytes)
	for _, c := range []struct {
		key    string
		lo, hi int
	}{
		{"baseMs", ReconnectBaseMinMS, ReconnectBaseMaxMS},
		{"capMs", ReconnectCapMinMS, ReconnectCapMaxMS},
		{"jitterPct", ReconnectJitterMinPct, ReconnectJitterMaxPct},
	} {
		lo, hi := jsLimit(t, js, c.key)
		if lo != c.lo || hi != c.hi {
			t.Fatalf("%s: the worker accepts %d..%d but the Control validates %d..%d — a value legal on one side would be dropped by the other", c.key, lo, hi, c.lo, c.hi)
		}
	}

	// The built-in worker defaults are the SEEDED defaults (migration 0029), so a worker that has not yet
	// registered waits exactly as one that has.
	sqlBytes, err := os.ReadFile("../../../../db/migrations/send_managed_0029_worker_reconnect_config.up.sql")
	if err != nil {
		t.Fatalf("read migration 0029: %v", err)
	}
	for key, jsKey := range map[string]string{
		"worker_reconnect_base_ms":    "baseMs",
		"worker_reconnect_cap_ms":     "capMs",
		"worker_reconnect_jitter_pct": "jitterPct",
	} {
		m := regexp.MustCompile(`\('` + key + `',\s*'\{"v":(\d+)\}',\s*'\{"v":(\d+)\}'`).FindStringSubmatch(string(sqlBytes))
		if m == nil {
			t.Fatalf("could not read the seed of %s from migration 0029", key)
		}
		if m[1] != m[2] {
			t.Fatalf("%s: seeded value %s != default_value %s", key, m[1], m[2])
		}
		seeded, _ := strconv.Atoi(m[1])
		if d := jsDefault(t, js, jsKey); d != seeded {
			t.Fatalf("%s: the worker's built-in default is %d but the seed is %d", key, d, seeded)
		}
		// and the seeded default is inside its own legal range
		lo, hi := jsLimit(t, js, jsKey)
		if seeded < lo || seeded > hi {
			t.Fatalf("%s: the seeded default %d is outside its legal range %d..%d", key, seeded, lo, hi)
		}
	}
}
