package resmon

import (
	"testing"
	"time"
)

func TestParseEntry(t *testing.T) {
	s, ok := ParseEntry("1754200000,42.5,33.3")
	if !ok || s.At != 1754200000 || s.CPU != 42.5 || s.Mem != 33.3 {
		t.Fatalf("parse failed: %+v ok=%v", s, ok)
	}
	if _, ok := ParseEntry("bad"); ok {
		t.Fatalf("bad entry should fail")
	}
	if _, ok := ParseEntry("1,x,3"); ok {
		t.Fatalf("non-numeric cpu should fail")
	}
	if _, ok := ParseEntry("1,2"); ok {
		t.Fatalf("missing fields should fail")
	}
}

func TestDownsample(t *testing.T) {
	samples := make([]Sample, 100)
	for i := range samples {
		samples[i] = Sample{At: int64(1000 + i), CPU: float64(i), Mem: float64(2 * i)}
	}
	out := downsample(samples, 20)
	if len(out) != 20 {
		t.Fatalf("downsample len = %d, want 20", len(out))
	}
	// 单调上升的序列，降采样后首点应低于尾点
	if out[0].CPU >= out[len(out)-1].CPU {
		t.Fatalf("downsample ordering wrong: first=%.1f last=%.1f", out[0].CPU, out[len(out)-1].CPU)
	}
	// 时间取组中点：首组应大致为前 5 个点均值附近
	expAvg := (samples[0].CPU + samples[1].CPU + samples[2].CPU + samples[3].CPU + samples[4].CPU) / 5
	if d := out[0].CPU - expAvg; d > 0.1 || d < -0.1 {
		t.Fatalf("downsample avg wrong: got %.2f want %.2f", out[0].CPU, expAvg)
	}
	// 不超过 points 时原样返回
	if got := downsample(samples, 500); len(got) != len(samples) {
		t.Fatalf("should return as-is when under limit, got %d", len(got))
	}
}

func TestReverse(t *testing.T) {
	in := []string{"a", "b", "c"}
	out := reverse(in)
	if out[0] != "c" || out[2] != "a" {
		t.Fatalf("reverse failed: %v", out)
	}
}

func TestSampleIntervalType(t *testing.T) {
	if SampleInterval <= 0 || time.Duration(SampleInterval) > time.Minute {
		t.Fatalf("unexpected SampleInterval %v", SampleInterval)
	}
}
