package service

import (
	"testing"
	"time"
)

// AI 分析月配额：计数 399 < 400 → 通过；计数 400 → 拒绝（不 INCR）
func TestAiQuotaAllows(t *testing.T) {
	cases := []struct {
		name  string
		used  int64
		quota int64
		want  bool
	}{
		{"计数399<400 通过", 399, 400, true},
		{"计数400=400 拒绝", 400, 400, false},
		{"计数401>400 拒绝", 401, 400, false},
		{"配额0（无配额）拒绝", 0, 0, false},
		{"配额0但已用1 拒绝", 1, 0, false},
		{"首次计数0<1 通过", 0, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := aiQuotaAllows(c.used, c.quota)
			if got != c.want {
				t.Fatalf("aiQuotaAllows(%d,%d) = %v, want %v", c.used, c.quota, got, c.want)
			}
		})
	}
}

// 月计数 key：上海自然月格式
func TestAiAnalyzeMonthKey(t *testing.T) {
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, aiAnalyzeLoc)
	key := aiAnalyzeMonthKey(42, now)
	if key != "sub:ai_analyze:42:202608" {
		t.Fatalf("aiAnalyzeMonthKey = %s", key)
	}
}

// 月末 TTL：8 月剩余应约 (8/31 24:00 - now) + 1min，落在 0–32 天
func TestAiAnalyzeMonthKeyTTL(t *testing.T) {
	ttl := aiAnalyzeMonthKeyTTL()
	if ttl <= 0 || ttl > 32*24*time.Hour {
		t.Fatalf("aiAnalyzeMonthKeyTTL out of range: %v", ttl)
	}
}
