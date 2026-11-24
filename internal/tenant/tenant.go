// Package tenant carries the authenticated API-key identity through request
// contexts. It is intentionally small: the scheduler uses it to namespace
// session IDs so two API keys cannot share provider affinity by reusing the
// same X-Session-ID.
package tenant

import "context"

type contextKey struct{}

// WithID returns a context carrying the tenant/API-key identifier.
func WithID(ctx context.Context, id string) context.Context {
	if ctx == nil || id == "" {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, id)
}

// ID returns the tenant identifier, or an empty string when unauthenticated.
func ID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}
