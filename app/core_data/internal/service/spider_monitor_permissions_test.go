package service

import (
	"testing"

	"cwxu-algo/app/common/rbac"
	"cwxu-algo/app/common/utils/auth"
)

func TestHasSpiderMonitorReadPermission(t *testing.T) {
	tests := []struct {
		name  string
		perms []string
		want  bool
	}{
		{name: "site config read", perms: []string{rbac.PermSiteConfigRead}, want: true},
		{name: "spider operations", perms: []string{rbac.PermSiteSpiderOps}, want: true},
		{name: "problem operations", perms: []string{rbac.PermSiteProblemOps}, want: true},
		{name: "unrelated permission", perms: []string{rbac.PermSiteStatsRead}, want: false},
		{name: "no permissions", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := &auth.JwtPayload{Pm: rbac.Encode(tt.perms)}
			if got := hasSpiderMonitorReadPermission(claims); got != tt.want {
				t.Fatalf("hasSpiderMonitorReadPermission() = %v, want %v", got, tt.want)
			}
		})
	}

	if hasSpiderMonitorReadPermission(nil) {
		t.Fatal("nil claims must be denied")
	}
}
