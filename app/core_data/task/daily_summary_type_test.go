package task

import (
	"testing"
)

// 日报分流：AIDailyEmailEnabled=false → PersonalDailyRule；true → PersonalLastDay
func TestDailySummaryType(t *testing.T) {
	cases := []struct {
		aiDaily bool
		want    string
	}{
		{false, "PersonalDailyRule"},
		{true, "PersonalLastDay"},
	}
	for _, c := range cases {
		if got := dailySummaryType(c.aiDaily); got != c.want {
			t.Fatalf("dailySummaryType(%v) = %s, want %s", c.aiDaily, got, c.want)
		}
	}
}
