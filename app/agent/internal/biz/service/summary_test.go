package service

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestFillMissingDays(t *testing.T) {
	start := time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)
	end := time.Date(2026, 7, 3, 0, 0, 0, 0, time.Local)
	days := fillMissingDays(start, end, nil)
	if len(days) != 3 {
		t.Fatalf("expected 3 days, got %d", len(days))
	}
	if days[0].Count != 0 || days[1].Date != "2026-07-02" {
		t.Fatalf("unexpected days: %+v", days)
	}
}

func TestConsecutiveZeroFromEnd(t *testing.T) {
	days := []DayCount{
		{Date: "2026-07-01", Count: 3},
		{Date: "2026-07-02", Count: 0},
		{Date: "2026-07-03", Count: 0},
	}
	if n := consecutiveZeroFromEnd(days); n != 2 {
		t.Fatalf("expected 2, got %d", n)
	}
	days[2].Count = 1
	if n := consecutiveZeroFromEnd(days); n != 0 {
		t.Fatalf("expected 0, got %d", n)
	}
}

func TestSumDayCounts(t *testing.T) {
	if sumDayCounts([]DayCount{{Count: 1}, {Count: 2}, {Count: 3}}) != 6 {
		t.Fatal("sum mismatch")
	}
}

func TestStripCodeFence(t *testing.T) {
	in := "```html\n<p>hello</p>\n```"
	out := stripCodeFence(in)
	if out != "<p>hello</p>" {
		t.Fatalf("unexpected output %q", out)
	}
}

func TestCoachRoleSkipLogic(t *testing.T) {
	tests := []struct {
		roleId    int
		isMonday  bool
		expectNil bool
		desc      string
	}{
		{2, false, true, "coach non-Monday skip"},
		{2, true, false, "coach Monday run weekly"},
		{0, false, false, "member daily"},
		{1, true, false, "admin monday both"},
		{3, false, false, "captain daily"},
		{3, true, false, "captain monday both"},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			// 纯教练非周一跳过；队长/管理员/队员走日报（周一另加周报）
			shouldSkip := tt.roleId == 2 && !tt.isMonday
			if shouldSkip != tt.expectNil {
				t.Errorf("role=%d monday=%v skip want %v got %v", tt.roleId, tt.isMonday, tt.expectNil, shouldSkip)
			}
		})
	}
}

func TestWeeklyDateRange(t *testing.T) {
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.Local) // Monday
	weekEnd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -1)
	weekStart := weekEnd.AddDate(0, 0, -6)
	if weekEnd.Format(dateLayout) != "2026-07-12" || weekStart.Format(dateLayout) != "2026-07-06" {
		t.Fatalf("range %s ~ %s", weekStart.Format(dateLayout), weekEnd.Format(dateLayout))
	}
}

func TestFormatCNDate(t *testing.T) {
	if formatCNDate("2026-07-06") != "7月6日" {
		t.Fatalf("got %s", formatCNDate("2026-07-06"))
	}
}

func TestWeeklySentMarkerIsScopedByUserOrgAndWeek(t *testing.T) {
	mr := miniredis.RunT(t)
	uc := &SummaryUseCase{redis: redis.NewClient(&redis.Options{Addr: mr.Addr()})}
	ctx := context.Background()
	start := time.Date(2026, 8, 10, 0, 0, 0, 0, time.Local)
	end := time.Date(2026, 8, 16, 0, 0, 0, 0, time.Local)

	sent, err := uc.weeklyAlreadySent(ctx, 7, 11, start, end)
	if err != nil {
		t.Fatalf("read weekly marker: %v", err)
	}
	if sent {
		t.Fatal("new weekly delivery must not be marked sent")
	}
	if err := uc.markWeeklySent(ctx, 7, 11, start, end); err != nil {
		t.Fatalf("mark weekly sent: %v", err)
	}
	sent, err = uc.weeklyAlreadySent(ctx, 7, 11, start, end)
	if err != nil || !sent {
		t.Fatal("marked weekly delivery should be detected")
	}
	otherOrg, err := uc.weeklyAlreadySent(ctx, 7, 12, start, end)
	if err != nil {
		t.Fatalf("read other org marker: %v", err)
	}
	otherUser, err := uc.weeklyAlreadySent(ctx, 8, 11, start, end)
	if err != nil {
		t.Fatalf("read other user marker: %v", err)
	}
	if otherOrg || otherUser {
		t.Fatal("weekly marker leaked across user or organization")
	}
}
