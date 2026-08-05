package loadgate

import (
	"context"
	"testing"
	"time"
)

func TestNewDefaultThreshold(t *testing.T) {
	g := New()
	if g.Threshold() != 0.7 {
		t.Fatalf("default threshold = %v, want 0.7", g.Threshold())
	}
	if g.ncpu <= 0 {
		t.Fatalf("ncpu = %v, want > 0", g.ncpu)
	}
}

func TestEnvThreshold(t *testing.T) {
	t.Setenv("CWXU_CPU_GATE_THRESHOLD", "0.5")
	g := New()
	if g.Threshold() != 0.5 {
		t.Fatalf("env threshold = %v, want 0.5", g.Threshold())
	}
	t.Setenv("CWXU_CPU_GATE_THRESHOLD", "1.5") // 非法（>=1）
	g = New()
	if g.Threshold() != 0.7 {
		t.Fatalf("invalid env threshold should fall back to 0.7, got %v", g.Threshold())
	}
	t.Setenv("CWXU_CPU_GATE_THRESHOLD", "-1") // 非法（<=0）
	g = New()
	if g.Threshold() != 0.7 {
		t.Fatalf("invalid env threshold should fall back to 0.7, got %v", g.Threshold())
	}
}

// Wait 在系统不忙时应立即返回 true（开发机/CI 负载低）。
func TestWaitIdleFast(t *testing.T) {
	g := New()
	if !g.Wait(nil, 2*time.Second) {
		t.Fatalf("Wait should return quickly on an idle system")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if !g.Wait(ctx, 3*time.Second) {
		t.Fatalf("Wait(ctx) should return quickly on an idle system")
	}
}
