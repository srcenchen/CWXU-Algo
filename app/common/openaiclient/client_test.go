package openaiclient

import (
	"testing"
	"time"
)

func TestLLMCallTimeoutIsTenMinutes(t *testing.T) {
	if LLMCallTimeout != 10*time.Minute {
		t.Fatalf("LLMCallTimeout = %s, want 10m", LLMCallTimeout)
	}
}
