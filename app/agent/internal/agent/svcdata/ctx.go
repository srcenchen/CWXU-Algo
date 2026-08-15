package svcdata

import (
	"context"
	"strings"

	"google.golang.org/grpc/metadata"
)

// toolRPCContext returns ctx if non-nil, else Background.
// Elevated agent tools should be constructed with ContextWithElevatedAgent so
// Authorization metadata is present on every gRPC call.
func toolRPCContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// BearerFromContext extracts the Bearer token from outgoing gRPC metadata (if any).
// Used by tests to prove elevated identity is wired into tool calls.
func BearerFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	md, ok := metadata.FromOutgoingContext(ctx)
	if !ok {
		return ""
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		vals = md.Get("Authorization")
	}
	if len(vals) == 0 {
		return ""
	}
	parts := strings.Fields(vals[0])
	if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
		return parts[1]
	}
	return vals[0]
}

// HasElevatedAuth reports whether ctx carries a non-empty Bearer token.
func HasElevatedAuth(ctx context.Context) bool {
	return BearerFromContext(ctx) != ""
}
