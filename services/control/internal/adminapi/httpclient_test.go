package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// resp is a decoded admin API response for assertions.
type resp struct {
	status int
	raw    []byte
	body   map[string]any
}

// do sends an authenticated request (bearer = env.adminKey unless overridden) and decodes JSON.
// bodyObj is marshaled as the request body (nil => no body). Use doRaw for a custom bearer.
func (e *testEnv) do(t *testing.T, method, path string, bodyObj any) resp {
	t.Helper()
	return e.doRaw(t, method, path, bodyObj, e.adminKey)
}

func (e *testEnv) doRaw(t *testing.T, method, path string, bodyObj any, bearer string) resp {
	t.Helper()
	var reqBody io.Reader
	if bodyObj != nil {
		b, err := json.Marshal(bodyObj)
		if err != nil {
			t.Fatalf("marshal req: %v", err)
		}
		reqBody = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.baseURL+path, reqBody)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if bodyObj != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer httpResp.Body.Close()
	raw, _ := io.ReadAll(httpResp.Body)
	r := resp{status: httpResp.StatusCode, raw: raw}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &r.body)
	}
	return r
}

// doRawBody sends a RAW request body string (for malformed-JSON / secret-in-null tests) with the
// default bearer.
func (e *testEnv) doRawBody(t *testing.T, method, path, rawBody string) resp {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, e.baseURL+path, bytes.NewReader([]byte(rawBody)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.adminKey)
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer httpResp.Body.Close()
	raw, _ := io.ReadAll(httpResp.Body)
	r := resp{status: httpResp.StatusCode, raw: raw}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &r.body)
	}
	return r
}

// errorCode extracts error.code from a decoded envelope ("" if absent).
func (r resp) errorCode() string {
	if r.body == nil {
		return ""
	}
	e, ok := r.body["error"].(map[string]any)
	if !ok {
		return ""
	}
	c, _ := e["code"].(string)
	return c
}

// errorDetailIssue returns the issue slug for the first detail matching field ("" if none).
func (r resp) errorDetailIssue(field string) string {
	if r.body == nil {
		return ""
	}
	e, ok := r.body["error"].(map[string]any)
	if !ok {
		return ""
	}
	ds, ok := e["details"].([]any)
	if !ok {
		return ""
	}
	for _, d := range ds {
		dm, ok := d.(map[string]any)
		if !ok {
			continue
		}
		if dm["field"] == field {
			s, _ := dm["issue"].(string)
			return s
		}
	}
	return ""
}

// str returns a top-level string field from the decoded body.
func (r resp) str(key string) string {
	if r.body == nil {
		return ""
	}
	s, _ := r.body[key].(string)
	return s
}

// errorMessage returns the error.message string ("" if absent).
func (r resp) errorMessage() string {
	if r.body == nil {
		return ""
	}
	e, ok := r.body["error"].(map[string]any)
	if !ok {
		return ""
	}
	m, _ := e["message"].(string)
	return m
}

// errorRequestID returns the error.request_id string ("" if absent).
func (r resp) errorRequestID() string {
	if r.body == nil {
		return ""
	}
	e, ok := r.body["error"].(map[string]any)
	if !ok {
		return ""
	}
	id, _ := e["request_id"].(string)
	return id
}

// requireStatus fails the test if the status is unexpected, printing the raw body (which must
// NEVER contain a secret — so this is safe to print).
func (r resp) requireStatus(t *testing.T, want int) resp {
	t.Helper()
	if r.status != want {
		t.Fatalf("status = %d, want %d; body = %s", r.status, want, string(r.raw))
	}
	return r
}
