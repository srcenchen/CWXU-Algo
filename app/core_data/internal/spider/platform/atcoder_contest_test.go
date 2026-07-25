package platform

import (
	"cwxu-algo/app/core_data/internal/spider"
	"strings"
	"testing"
	"time"
)

// Compile-time: registered AtCoder provider satisfies SubmitContestFetcher.
var _ spider.SubmitContestFetcher = NewAtCoder{}

func TestNormalizeAtCoderContestID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"agc004.contest.atcoder.jp", "agc004"},
		{"abc382.contest.atcoder.jp", "abc382"},
		{"abc382", "abc382"},
		{"  arc061.contest.atcoder.jp ", "arc061"},
		{"", ""},
	}
	for _, c := range cases {
		if got := normalizeAtCoderContestID(c.in); got != c.want {
			t.Errorf("normalizeAtCoderContestID(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestContestLogsFromAtCoderHistory_Fixture(t *testing.T) {
	raw := []byte(`[
		{
			"IsRated": true,
			"Place": 2,
			"OldRating": 0,
			"NewRating": 2720,
			"ContestScreenName": "agc004.contest.atcoder.jp",
			"ContestName": "AtCoder Grand Contest 004",
			"ContestNameEn": "",
			"EndTime": "2016-09-04T22:50:00+09:00"
		},
		{
			"IsRated": true,
			"Place": 1,
			"OldRating": 2720,
			"NewRating": 3000,
			"ContestScreenName": "abc100.contest.atcoder.jp",
			"ContestName": "AtCoder Beginner Contest 100",
			"ContestNameEn": "",
			"EndTime": "2018-06-16T22:40:00+09:00"
		}
	]`)
	hist, err := parseAtCoderHistoryJSON(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	all := contestLogsFromAtCoderHistory(42, hist, true, map[string]int{"agc004": 5, "abc100": 3})
	if len(all) != 2 {
		t.Fatalf("needAll=true got %d logs", len(all))
	}
	first := all[0]
	if first.Platform != spider.AtCoder {
		t.Errorf("platform=%q", first.Platform)
	}
	if first.UserID != 42 {
		t.Errorf("userId=%d", first.UserID)
	}
	if first.ContestId != "agc004" {
		t.Errorf("contestId=%q", first.ContestId)
	}
	if first.ContestName != "AtCoder Grand Contest 004" {
		t.Errorf("name=%q", first.ContestName)
	}
	if first.ContestUrl != "https://atcoder.jp/contests/agc004" {
		t.Errorf("url=%q", first.ContestUrl)
	}
	if first.Rank != 2 {
		t.Errorf("rank=%d", first.Rank)
	}
	if first.AcCount != 5 {
		t.Errorf("acCount=%d want 5", first.AcCount)
	}
	if first.Time.IsZero() {
		t.Error("time empty")
	}
	// EndTime +09:00 → check parsed wall clock
	wantYear, wantMonth, wantDay := 2016, time.September, 4
	y, m, d := first.Time.Date()
	if y != wantYear || m != wantMonth || d != wantDay {
		t.Errorf("time date=%v-%v-%v", y, m, d)
	}

	latestOnly := contestLogsFromAtCoderHistory(42, hist, false, nil)
	if len(latestOnly) != 1 {
		t.Fatalf("needAll=false got %d", len(latestOnly))
	}
	if latestOnly[0].ContestId != "abc100" || latestOnly[0].Rank != 1 {
		t.Errorf("latest=%+v", latestOnly[0])
	}
	if latestOnly[0].AcCount != 0 {
		t.Errorf("nil ac map should keep 0, got %d", latestOnly[0].AcCount)
	}
}

func TestFetchAtCoderContestAC_FiltersPracticeAndDedups(t *testing.T) {
	// contest ends at 1000; practice after end excluded; WA ignored; same problem dedup
	endBy := map[string]int64{"abc467": 1000}
	subs := []atcJson{
		{ContestID: "abc467", ProblemID: "abc467_a", Result: "AC", EpochSecond: 900},
		{ContestID: "abc467", ProblemID: "abc467_a", Result: "AC", EpochSecond: 950}, // dup
		{ContestID: "abc467", ProblemID: "abc467_b", Result: "WA", EpochSecond: 960},
		{ContestID: "abc467", ProblemID: "abc467_b", Result: "AC", EpochSecond: 980},
		{ContestID: "abc467", ProblemID: "abc467_c", Result: "AC", EpochSecond: 1001}, // practice
		{ContestID: "arc100", ProblemID: "arc100_a", Result: "AC", EpochSecond: 2000}, // no end bound
	}
	ac := fetchAtCoderContestAC(subs, endBy)
	if ac["abc467"] != 2 {
		t.Fatalf("abc467 ac=%d want 2 (a,b; practice c excluded)", ac["abc467"])
	}
	if ac["arc100"] != 1 {
		t.Fatalf("arc100 ac=%d want 1", ac["arc100"])
	}
}

// 回归：仅补题用户（history 无该场）不得把赛后 AC 写成赛时格子。
// 线上 arc225「憨憨的竹林」绿钩即此路径污染 contest_user_problems。
func TestAggregateAtCoderContestDetailCells_SkipsUpsolveOnly(t *testing.T) {
	endBy := map[string]int64{"arc225": 1_000_000} // 正式参赛者才有
	subs := []atcJson{
		// 正式赛时
		{ContestID: "arc225", ProblemID: "arc225_a", Result: "WA", EpochSecond: 900_000},
		{ContestID: "arc225", ProblemID: "arc225_a", Result: "AC", EpochSecond: 950_000},
		// 赛后练习（有 history）
		{ContestID: "arc225", ProblemID: "arc225_c", Result: "AC", EpochSecond: 1_000_100},
		// 从未参赛的场次：只有补题提交
		{ContestID: "arc999", ProblemID: "arc999_e", Result: "WA", EpochSecond: 2_000_000},
		{ContestID: "arc999", ProblemID: "arc999_e", Result: "AC", EpochSecond: 2_000_100},
	}
	cells := aggregateAtCoderContestDetailCells(subs, endBy)
	by := map[string]string{}
	for _, c := range cells {
		by[c.ExternalID] = c.Status
	}
	if by["arc225_a"] != "AC" {
		t.Fatalf("contest AC missing: %+v", cells)
	}
	if _, ok := by["arc225_c"]; ok {
		t.Fatalf("post-end practice must not become cell: %+v", cells)
	}
	if _, ok := by["arc999_e"]; ok {
		t.Fatalf("upsolve-only contest must not write AC cells: %+v", cells)
	}
}

func TestFetchContestLog_EmptyUsername(t *testing.T) {
	_, err := NewAtCoder{}.FetchContestLog(1, "", true)
	if err == nil {
		t.Fatal("expected error for empty username")
	}
	if !strings.Contains(err.Error(), "username") {
		t.Errorf("error should mention username: %v", err)
	}
}

func TestFetchContestLog_LiveTourist(t *testing.T) {
	logs, err := NewAtCoder{}.FetchContestLog(1, "tourist", true)
	if err != nil {
		t.Skipf("network/API: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("tourist should have non-empty contest history")
	}
	var ok, withAC int
	for _, l := range logs {
		if l.Platform != spider.AtCoder {
			t.Fatalf("platform=%q", l.Platform)
		}
		if l.ContestId == "" || l.ContestName == "" {
			t.Fatalf("empty id/name: %+v", l)
		}
		if !strings.HasPrefix(l.ContestUrl, "https://atcoder.jp/contests/") {
			t.Fatalf("bad url: %q", l.ContestUrl)
		}
		if strings.Contains(l.ContestId, ".contest.atcoder.jp") {
			t.Fatalf("contest id not normalized: %q", l.ContestId)
		}
		if l.Rank < 1 {
			t.Fatalf("rank should be >=1 for tourist: %+v", l)
		}
		if l.Time.IsZero() {
			t.Fatalf("time empty: %+v", l)
		}
		if l.AcCount > 0 {
			withAC++
		}
		ok++
	}
	t.Logf("AtCoder tourist contests=%d withAC=%d first=%s rank=%d ac=%d last=%s rank=%d ac=%d",
		len(logs), withAC, logs[0].ContestId, logs[0].Rank, logs[0].AcCount,
		logs[len(logs)-1].ContestId, logs[len(logs)-1].Rank, logs[len(logs)-1].AcCount)
	if ok < 1 {
		t.Fatal("no valid contests")
	}
	// tourist 全量应能从提交统计出至少若干场 AC（代理/网络失败时 AcCount 全 0 可接受）
	if withAC == 0 {
		t.Log("warning: no contest has AcCount>0 (submission API may be down)")
	}

	// needAll=false → single latest
	one, err := NewAtCoder{}.FetchContestLog(1, "tourist", false)
	if err != nil {
		t.Skipf("network/API: %v", err)
	}
	if len(one) != 1 {
		t.Fatalf("needAll=false want 1 got %d", len(one))
	}
	if one[0].ContestId != logs[len(logs)-1].ContestId {
		t.Errorf("latest id=%q want %q", one[0].ContestId, logs[len(logs)-1].ContestId)
	}
}

func TestFetchContestLog_LiveCubberLatest(t *testing.T) {
	logs, err := NewAtCoder{}.FetchContestLog(1, "Cubber", false)
	if err != nil {
		t.Skipf("network/API: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("want 1 got %d", len(logs))
	}
	l := logs[0]
	t.Logf("Cubber latest %s rank=%d ac=%d", l.ContestId, l.Rank, l.AcCount)
	if l.ContestId == "abc467" && l.AcCount < 1 {
		t.Fatalf("abc467 ac=%d want >0", l.AcCount)
	}
}
