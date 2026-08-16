package server

import (
	"context"
	"testing"
)

func TestWhiteListMatcherAllowsHealthOnly(t *testing.T) {
	match := NewWhiteListMatcher()
	for _, operation := range []string{"/healthz", "/readyz"} {
		if match(context.Background(), operation) {
			t.Fatalf("health operation %q requires authentication", operation)
		}
	}
	if !match(context.Background(), "/api.agent.v1.Summary/Get") {
		t.Fatal("business operation unexpectedly bypasses authentication")
	}
}
