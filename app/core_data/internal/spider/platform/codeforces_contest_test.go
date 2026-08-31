package platform

import (
	"cwxu-algo/app/core_data/internal/spider"
	"testing"
)

// 编译期：Codeforces 实现 SubmitContestFetcher / RatingFetcher
var (
	_ spider.SubmitContestFetcher = NewCodeforces{}
	_ spider.RatingFetcher        = NewCodeforces{}
)

func TestCFFetchContestLog_EmptyUsername(t *testing.T) {
	_, err := NewCodeforces{}.FetchContestLog(1, "", true)
	if err == nil {
		t.Fatal("empty handle should error")
	}
}

func TestCFContestIDSegment(t *testing.T) {
	if !isCFGymContestID(106501) || isCFGymContestID(2261) {
		t.Fatal("contest id segment classification is incorrect")
	}
	if got := canonicalCFContestID(106501); got != -106501 {
		t.Fatalf("gym id=%d want -106501", got)
	}
	if got := canonicalCFContestID(2261); got != 2261 {
		t.Fatalf("regular id=%d want 2261", got)
	}
	if got := cfContestURL(-106501); got != "https://codeforces.com/gym/106501" {
		t.Fatalf("gym url=%s", got)
	}
}

func TestCFFetchContestLog_LiveTourist(t *testing.T) {
	logs, err := NewCodeforces{}.FetchContestLog(1, "tourist", false)
	if err != nil {
		t.Fatalf("FetchContestLog: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("tourist should have contest history")
	}
	var hasRank, hasAC bool
	for _, l := range logs {
		if l.Platform != spider.CodeForces {
			t.Errorf("platform=%q want CodeForces", l.Platform)
		}
		if l.ContestId == "" || l.ContestName == "" {
			t.Errorf("missing id/name: %+v", l)
		}
		if l.Rank > 0 {
			hasRank = true
		}
		if l.AcCount > 0 {
			hasAC = true
		}
		t.Logf("contest=%s name=%s rank=%d ac=%d time=%v",
			l.ContestId, l.ContestName, l.Rank, l.AcCount, l.Time)
	}
	if !hasRank {
		t.Error("expected at least one rated rank for tourist")
	}
	if !hasAC {
		t.Error("expected at least one AC count from user.status for tourist")
	}

	// needAll=false 应截断
	if len(logs) > 15 {
		t.Errorf("needAll=false should cap at 15, got %d", len(logs))
	}
}

func TestCFFetchContestLog_LiveTouristFullHasMore(t *testing.T) {
	full, err := NewCodeforces{}.FetchContestLog(1, "tourist", true)
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	incr, err := NewCodeforces{}.FetchContestLog(1, "tourist", false)
	if err != nil {
		t.Fatalf("incr: %v", err)
	}
	if len(full) < len(incr) {
		t.Fatalf("full=%d should be >= incr=%d", len(full), len(incr))
	}
	// tourist 历史很长
	if len(full) < 50 {
		t.Fatalf("tourist full contests too few: %d", len(full))
	}
	t.Logf("tourist contests full=%d incr=%d first=%s rank=%d ac=%d",
		len(full), len(incr), full[0].ContestName, full[0].Rank, full[0].AcCount)
}
