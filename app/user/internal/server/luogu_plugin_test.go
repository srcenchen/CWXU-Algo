package server

import (
	"context"
	"testing"

	pb "cwxu-algo/api/user/v1/plugin"
)

func TestLuoguPluginTokenIsTheOnlyPublicJWTMatcherOperation(t *testing.T) {
	matcher := NewWhiteListMatcher()
	tests := []struct {
		operation string
		wantJWT   bool
	}{
		{operation: pb.OperationLuoguPluginAuthorizeCode, wantJWT: true},
		{operation: pb.OperationLuoguPluginListAuthorizations, wantJWT: true},
		{operation: pb.OperationLuoguPluginRevoke, wantJWT: true},
		{operation: pb.OperationLuoguPluginToken, wantJWT: false},
		{operation: pb.LuoguPlugin_ValidateLuoguPluginToken_FullMethodName, wantJWT: true},
	}
	for _, tt := range tests {
		t.Run(tt.operation, func(t *testing.T) {
			if got := matcher(context.Background(), tt.operation); got != tt.wantJWT {
				t.Fatalf("matcher(%q) = %v, want JWT=%v", tt.operation, got, tt.wantJWT)
			}
		})
	}
}
