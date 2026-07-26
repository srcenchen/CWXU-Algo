package platform

import (
	"context"
	"cwxu-algo/app/core_data/internal/spider"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Compile-time: LeetCode 实现 SubmitContestFetcher（比赛记录写入路径 type-assert）
var _ spider.SubmitContestFetcher = NewLeetCode{}

func TestLeetCodeContestSlug(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"第 365 场周赛", "weekly-contest-365"},
		{"第 160 场双周赛", "biweekly-contest-160"},
		{"第  83 场周赛", "weekly-contest-83"},
		{"Weekly Contest 400", "weekly-contest-400"},
		{"Biweekly Contest 12", "biweekly-contest-12"},
		{"", ""},
		{"Special Contest", ""},
	}
	for _, c := range cases {
		if got := leetCodeContestSlug(c.title); got != c.want {
			t.Fatalf("slug(%q)=%q want %q", c.title, got, c.want)
		}
	}
}

func TestMapLeetCodeContestHistory(t *testing.T) {
	// 代表 GraphQL 形状的 fixture（含未参赛、缺 contest、中英文标题）
	raw := `[
		{"attended":false,"ranking":0,"score":0,"totalProblems":4,"contest":{"title":"第 1 场周赛","titleCn":"","startTime":1000}},
		{"attended":true,"ranking":2306,"score":0,"totalProblems":4,"contest":{"title":"第 365 场周赛","titleCn":"","startTime":1696127400}},
		{"attended":true,"ranking":583,"score":3,"totalProblems":4,"contest":{"title":"第 160 场双周赛","titleCn":"第 160 场双周赛","startTime":1751725800}},
		{"attended":true,"ranking":1,"score":18,"totalProblems":4,"contest":{"title":"Weekly Contest 999","titleCn":"","startTime":1800000000}},
		{"attended":true,"ranking":9,"score":0,"totalProblems":4,"contest":null},
		{"attended":true,"ranking":2,"score":0,"totalProblems":3,"contest":{"title":"Mystery Cup","titleCn":"神秘杯","startTime":1700000000}}
	]`
	var items []lcContestHistoryItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		t.Fatal(err)
	}

	full := mapLeetCodeContestHistory(42, items, true)
	// attended: 365, 160, weekly-999, mystery（null contest / unattended 丢弃）
	if len(full) != 4 {
		t.Fatalf("full len=%d want 4; got %+v", len(full), full)
	}
	// 降序：999 start > mystery > 160 > 365
	if full[0].ContestId != "weekly-contest-999" || full[0].Platform != spider.LeetCode {
		t.Fatalf("first=%+v", full[0])
	}
	if full[0].UserID != 42 || full[0].Rank != 1 {
		t.Fatalf("first user/rank=%+v", full[0])
	}
	if full[0].ContestUrl != "https://leetcode.cn/contest/weekly-contest-999/" {
		t.Fatalf("url=%q", full[0].ContestUrl)
	}
	if full[0].Time.Unix() != 1800000000 {
		t.Fatalf("time=%v", full[0].Time)
	}
	// AcCount 存力扣竞赛 score，供站内榜展示
	if full[0].TotalCount != 4 || full[0].AcCount != 18 {
		t.Fatalf("counts total=%d ac=%d want total=4 ac=18", full[0].TotalCount, full[0].AcCount)
	}

	var foundBi bool
	for _, c := range full {
		if c.ContestId == "biweekly-contest-160" {
			foundBi = true
			if c.ContestName != "第 160 场双周赛" || c.Rank != 583 {
				t.Fatalf("biweekly=%+v", c)
			}
			if c.ContestUrl != "https://leetcode.cn/contest/biweekly-contest-160/" {
				t.Fatalf("bi url=%q", c.ContestUrl)
			}
		}
	}
	if !foundBi {
		t.Fatal("missing biweekly-contest-160")
	}

	var mystery bool
	for _, c := range full {
		if c.ContestId == "lc-1700000000" && c.ContestName == "神秘杯" && c.ContestUrl == "" {
			mystery = true
		}
	}
	if !mystery {
		t.Fatalf("mystery fallback missing: %+v", full)
	}

	// needAll=false：fixture 仅 4 条 attended，应全部返回
	incr := mapLeetCodeContestHistory(42, items, false)
	if len(incr) != 4 {
		t.Fatalf("incr len=%d", len(incr))
	}

	// >10 条时增量只保留最近 10
	many := make([]lcContestHistoryItem, 0, 15)
	for i := 0; i < 15; i++ {
		title := fmt.Sprintf("Weekly Contest %d", 100+i)
		st := int64(1000 + i)
		many = append(many, lcContestHistoryItem{
			Attended:      true,
			Ranking:       i + 1,
			TotalProblems: 4,
			Contest: &struct {
				Title     string `json:"title"`
				TitleCn   string `json:"titleCn"`
				StartTime int64  `json:"startTime"`
			}{Title: title, StartTime: st},
		})
	}
	recent := mapLeetCodeContestHistory(1, many, false)
	if len(recent) != 10 {
		t.Fatalf("recent len=%d want 10", len(recent))
	}
	// 最新 startTime=1014 → weekly-contest-114
	if recent[0].ContestId != "weekly-contest-114" {
		t.Fatalf("most recent=%+v", recent[0])
	}
	allMany := mapLeetCodeContestHistory(1, many, true)
	if len(allMany) != 15 {
		t.Fatalf("allMany len=%d want 15", len(allMany))
	}

	// 空输入
	if got := mapLeetCodeContestHistory(1, nil, true); len(got) != 0 {
		t.Fatalf("nil items → empty, got %d", len(got))
	}
}

func TestDedupeLeetCodeRecentAC(t *testing.T) {
	t1 := time.Unix(100, 0)
	t2 := time.Unix(200, 0)
	in := []lcRecentAC{
		{SubmissionID: 1, SubmitTime: t2, TitleSlug: "two-sum", Title: "A"},
		{SubmissionID: 1, SubmitTime: t2, TitleSlug: "two-sum", Title: "dup id"},
		{SubmissionID: 2, SubmitTime: t1, TitleSlug: "two-sum", Title: "older same slug"},
		{SubmissionID: 3, SubmitTime: t1, TitleSlug: "add-two", Title: "B"},
		{SubmissionID: 0, TitleSlug: "x"},
		{SubmissionID: 4, TitleSlug: ""},
	}
	out := dedupeLeetCodeRecentAC(in)
	if len(out) != 2 {
		t.Fatalf("len=%d want 2 (two-sum latest + add-two)", len(out))
	}
	if out[0].SubmissionID != 1 || out[0].TitleSlug != "two-sum" {
		t.Fatalf("first=%+v", out[0])
	}
	if out[1].SubmissionID != 3 {
		t.Fatalf("second=%+v", out[1])
	}
}

func TestLeetCodeFetchContestLogLive(t *testing.T) {
	p := NewLeetCode{}
	// 已知有参赛记录的 userSlug
	logs, err := p.FetchContestLog(999002, "sanenchen-o", true)
	if err != nil {
		t.Fatalf("FetchContestLog: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected non-empty contest history for sanenchen-o")
	}
	for i, c := range logs {
		if c.Platform != spider.LeetCode {
			t.Fatalf("[%d] platform=%q", i, c.Platform)
		}
		if c.ContestId == "" || c.ContestName == "" {
			t.Fatalf("[%d] missing id/name: %+v", i, c)
		}
		if c.UserID != 999002 {
			t.Fatalf("[%d] userId=%d", i, c.UserID)
		}
		if c.Time.IsZero() {
			t.Fatalf("[%d] zero time: %+v", i, c)
		}
		if c.Rank < 0 {
			t.Fatalf("[%d] negative rank: %d", i, c.Rank)
		}
	}
	// 增量应是全量的前缀（按时间降序的最近 10 条）
	incr, err := p.FetchContestLog(999002, "sanenchen-o", false)
	if err != nil {
		t.Fatalf("incr: %v", err)
	}
	if len(incr) == 0 {
		t.Fatal("incr empty")
	}
	if len(incr) > 10 {
		t.Fatalf("incr len=%d want ≤10", len(incr))
	}
	if len(logs) > 10 && len(incr) != 10 {
		t.Fatalf("full=%d incr=%d want incr=10", len(logs), len(incr))
	}
	if incr[0].ContestId != logs[0].ContestId {
		t.Fatalf("incr[0]=%s full[0]=%s", incr[0].ContestId, logs[0].ContestId)
	}

	// 未参赛 / 不存在用户：空切片、无错误
	empty, err := p.FetchContestLog(1, "nonexistent-user-xyz-99999", true)
	if err != nil {
		t.Fatalf("empty user err: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty user want 0 got %d", len(empty))
	}

	// 写样例到 scratch（若设置 GROK_SCRATCH）或测试目录旁可跳过
	scratch := os.Getenv("GROK_SCRATCH")
	if scratch == "" {
		// 目标 harness 默认路径（存在则写）
		candidates := []string{
			"/var/folders/09/hj34hxn56v3f7l72gw24zhth0000gn/T/grok-goal-001b66d10cea/implementer",
		}
		for _, c := range candidates {
			if st, err := os.Stat(c); err == nil && st.IsDir() {
				scratch = c
				break
			}
		}
	}
	if scratch != "" {
		type row struct {
			Platform    string `json:"platform"`
			ContestId   string `json:"contestId"`
			ContestName string `json:"contestName"`
			ContestUrl  string `json:"contestUrl"`
			Rank        int    `json:"rank"`
			TotalCount  int    `json:"totalCount"`
			TimeUnix    int64  `json:"timeUnix"`
		}
		n := len(logs)
		if n > 8 {
			n = 8
		}
		sample := make([]row, 0, n)
		for i := 0; i < n; i++ {
			sample = append(sample, row{
				Platform:    logs[i].Platform,
				ContestId:   logs[i].ContestId,
				ContestName: logs[i].ContestName,
				ContestUrl:  logs[i].ContestUrl,
				Rank:        logs[i].Rank,
				TotalCount:  logs[i].TotalCount,
				TimeUnix:    logs[i].Time.Unix(),
			})
		}
		b, _ := json.MarshalIndent(map[string]interface{}{
			"userSlug": "sanenchen-o",
			"total":    len(logs),
			"sample":   sample,
		}, "", "  ")
		out := filepath.Join(scratch, "lc-contest-sample.json")
		if err := os.WriteFile(out, b, 0o644); err != nil {
			t.Logf("write sample: %v", err)
		} else {
			t.Logf("wrote %s (%d rows total)", out, len(logs))
		}
	}
	t.Logf("contest full=%d first=%s rank=%d", len(logs), logs[0].ContestName, logs[0].Rank)
}

func TestLeetCodeFetchLive(t *testing.T) {
	p := NewLeetCode{}
	full, err := p.FetchSubmitLog(context.Background(), 999001, "sanenchen-o", true)
	if err != nil {
		t.Fatalf("full fetch: %v", err)
	}
	if len(full) == 0 {
		t.Fatal("full fetch empty")
	}

	cal, pad, ac, prob := 0, 0, 0, 0
	probSlugs := map[string]struct{}{}
	for _, l := range full {
		switch {
		case strings.HasPrefix(l.SubmitID, "lc-cal-"):
			cal++
			if l.Status != "SUBMIT" {
				t.Fatalf("cal status want SUBMIT got %s", l.Status)
			}
		case strings.HasPrefix(l.SubmitID, "lc-pad-"):
			pad++
			if l.Status != "SUBMIT" {
				t.Fatalf("pad status want SUBMIT got %s", l.Status)
			}
		case strings.HasPrefix(l.SubmitID, "lc-ac-"):
			ac++
			if l.Status != "AC" {
				t.Fatalf("ac status want AC got %s", l.Status)
			}
			// 全量初始化 AC 不应落在今天，避免污染今日做题数
			now := time.Now()
			if l.Time.Year() == now.Year() && l.Time.YearDay() == now.YearDay() {
				t.Fatalf("full AC should not be today: %v", l.Time)
			}
		case strings.HasPrefix(l.SubmitID, "lc-prob-"):
			prob++
			if l.Status != "AC" {
				t.Fatalf("prob status want AC got %s", l.Status)
			}
			if l.ExternalID == "" {
				t.Fatal("prob missing external_id (titleSlug)")
			}
			if !strings.HasPrefix(l.Problem, l.ExternalID) {
				t.Fatalf("prob problem should start with slug: %q", l.Problem)
			}
			probSlugs[l.ExternalID] = struct{}{}
		}
	}
	if cal == 0 {
		t.Fatal("expected calendar submits")
	}
	if ac == 0 && len(probSlugs) == 0 {
		t.Fatal("expected AC total or recent problems")
	}
	if prob == 0 {
		t.Fatal("expected recent AC (lc-prob-*) from public profile")
	}
	// 生涯提交：cal + pad 应接近 totalSubmissions（允许接口略有偏差）
	prog, err := fetchLeetCodeProgress(context.Background(), "sanenchen-o")
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if cal+pad < prog.TotalSubmissions-5 || cal+pad > prog.TotalSubmissions+5 {
		t.Fatalf("cal+pad=%d want ~totalSubmissions=%d", cal+pad, prog.TotalSubmissions)
	}
	// 合成 AC 条数 = acTotal（全量稳定 ID）
	if ac != prog.AcTotal {
		t.Fatalf("syn ac=%d want acTotal=%d", ac, prog.AcTotal)
	}
	t.Logf("full total=%d cal=%d pad=%d ac=%d prob=%d uniqueSlug=%d acTotal=%d totalSub=%d",
		len(full), cal, pad, ac, prob, len(probSlugs), prog.AcTotal, prog.TotalSubmissions)

	incr, err := p.FetchSubmitLog(context.Background(), 999001, "sanenchen-o", false)
	if err != nil {
		t.Fatalf("incr fetch: %v", err)
	}
	cal2, pad2, ac2, prob2 := 0, 0, 0, 0
	for _, l := range incr {
		switch {
		case strings.HasPrefix(l.SubmitID, "lc-cal-"):
			cal2++
		case strings.HasPrefix(l.SubmitID, "lc-pad-"):
			pad2++
		case strings.HasPrefix(l.SubmitID, "lc-ac-"):
			ac2++
		case strings.HasPrefix(l.SubmitID, "lc-prob-"):
			prob2++
		}
	}
	if ac2 != ac {
		t.Fatalf("incr ac count should equal full ac for stable ids: full=%d incr=%d", ac, ac2)
	}
	if pad2 != pad {
		t.Fatalf("incr pad should equal full pad: full=%d incr=%d", pad, pad2)
	}
	if cal2 > cal {
		t.Fatalf("incr cal should be subset: full=%d incr=%d", cal, cal2)
	}
	if prob2 == 0 {
		t.Fatal("incr expected recent AC")
	}
	t.Logf("incr total=%d cal=%d pad=%d ac=%d prob=%d", len(incr), cal2, pad2, ac2, prob2)
}
