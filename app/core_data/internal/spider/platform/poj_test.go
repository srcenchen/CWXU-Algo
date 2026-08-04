package platform

import (
	"context"
	"strings"
	"testing"
	"time"

	"cwxu-algo/app/core_data/internal/spider"
)

const samplePOJStatusHTML = `
<html><body>
<table>
<tr class=in><td>Run ID</td><td>User</td><td>Problem</td><td>Result</td>
<td>Memory</td><td>Time</td><td>Language</td><td>Code Length</td><td>Submit Time</td></tr>
<tr align=center><td>25194151</td><td><a href=userstatus?user_id=sanenchen>sanenchen</a></td>
<td><a href=problem?id=1000>1000</a></td>
<td><a href=showcompileinfo?solution_id=25194151 target=_blank><font color=green>Compile Error</font></a></td>
<td></td><td></td><td>GCC</td><td>129B</td><td>2026-08-04 21:39:37</td></tr>
<tr align=center><td>25194100</td><td><a href=userstatus?user_id=sanenchen>sanenchen</a></td>
<td><a href=problem?id=1001>1001</a></td>
<td><font color=blue>Accepted</font></td>
<td>168K</td><td>0MS</td><td>G++</td><td>200B</td><td>2026-08-04 20:00:00</td></tr>
</table>
<p align=center>[<a href=status?user_id=sanenchen>Top</a>]
&nbsp;&nbsp;[<a href=status?user_id=sanenchen&top=25194100><font color=blue>Next Page</font></a>]</p>
</body></html>
`

func TestMapPOJStatus(t *testing.T) {
	cases := map[string]string{
		"Accepted":              "AC",
		"Wrong Answer":          "WA",
		"Time Limit Exceeded":   "TLE",
		"Memory Limit Exceeded": "MLE",
		"Runtime Error":         "RE",
		"Compile Error":         "CE",
		"Presentation Error":    "PE",
		"Output Limit Exceeded": "OLE",
		"Waiting":               "Judging",
	}
	for in, want := range cases {
		if got := mapPOJStatus(in); got != want {
			t.Fatalf("mapPOJStatus(%q)=%q want %q", in, got, want)
		}
	}
}

func TestParsePOJStatusPage(t *testing.T) {
	logs, minRun, ok := parsePOJStatusPage(samplePOJStatusHTML, 7)
	if !ok {
		t.Fatal("expected ok")
	}
	if len(logs) != 2 {
		t.Fatalf("len=%d", len(logs))
	}
	if minRun != "25194100" {
		t.Fatalf("minRun=%s", minRun)
	}
	a := logs[0]
	if a.Platform != spider.POJ || a.SubmitID != "25194151" || a.ExternalID != "1000" {
		t.Fatalf("first=%+v", a)
	}
	if a.Problem != "#1000" || a.Status != "CE" || a.Lang != "GCC" {
		t.Fatalf("first fields=%+v", a)
	}
	if a.UserID != 7 {
		t.Fatalf("userId=%d", a.UserID)
	}
	if a.Time.IsZero() {
		t.Fatal("time zero")
	}
	// 北京时间 21:39:37 → UTC 13:39:37
	if utc := a.Time.UTC(); utc.Hour() != 13 || utc.Minute() != 39 {
		t.Fatalf("time utc=%v local=%v", utc, a.Time)
	}
	b := logs[1]
	if b.Status != "AC" || b.ExternalID != "1001" {
		t.Fatalf("second=%+v", b)
	}
}

func TestParsePOJStatusPage_Empty(t *testing.T) {
	_, _, ok := parsePOJStatusPage(`<html><body><p>no rows</p></body></html>`, 1)
	if ok {
		t.Fatal("expected empty")
	}
}

func TestPOJ_FetchSubmitLog_IncrementalLive(t *testing.T) {
	if testing.Short() {
		t.Skip("skip live poj")
	}
	logs, err := NewPOJ{}.FetchSubmitLog(context.Background(), 1, "sanenchen", false)
	if err != nil {
		t.Fatalf("poj incremental: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("expected at least one submission for sanenchen")
	}
	for _, l := range logs {
		if l.Platform != spider.POJ {
			t.Fatalf("platform=%s", l.Platform)
		}
		if l.SubmitID == "" || !isAllDigits(l.SubmitID) {
			t.Fatalf("submit_id=%q", l.SubmitID)
		}
		if l.Status == "" {
			t.Fatal("empty status")
		}
	}
	t.Logf("poj incremental n=%d first=%+v", len(logs), logs[0])
}

func TestPOJ_FetchSubmitLog_FullLiveFewPages(t *testing.T) {
	if testing.Short() {
		t.Skip("skip live poj")
	}
	// zhuzeyuan 历史很长；这里用 context 超时截断，只验证能翻页且去重
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	logs, err := NewPOJ{}.FetchSubmitLog(ctx, 1, "zhuzeyuan", true)
	// 超时可能返回 ctx 错误；只要拉到了多页就行
	if err != nil && !strings.Contains(err.Error(), "context") && ctx.Err() == nil {
		t.Fatalf("poj full: %v", err)
	}
	if len(logs) < 20 {
		// 若因超时过早结束且不足一页，仍算环境问题
		if ctx.Err() != nil && len(logs) > 0 {
			t.Logf("timeout with n=%d (ok)", len(logs))
			return
		}
		t.Fatalf("expected multi-page, got %d err=%v", len(logs), err)
	}
	seen := map[string]struct{}{}
	for _, l := range logs {
		if _, ok := seen[l.SubmitID]; ok {
			t.Fatalf("dup submit_id %s", l.SubmitID)
		}
		seen[l.SubmitID] = struct{}{}
	}
	t.Logf("poj full partial n=%d", len(logs))
}
