package adminapi

import "context"

// ctxKey is a private context key type (avoids collisions across packages).
type ctxKey int

const requestIDKey ctxKey = iota

// withRequestID returns a child context carrying the request_id.
func withRequestID(ctx context.Context, rid string) context.Context {
	return context.WithValue(ctx, requestIDKey, rid)
}

// requestIDFrom recovers the request_id, or mints a fallback if absent (a handler reached
// without the auth middleware — should not happen for /v1/admin/*).
func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok && v != "" {
		return v
	}
	return newRequestID()
}
