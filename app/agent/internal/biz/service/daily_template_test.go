package service

import (
	"strings"
	"testing"
)

func TestRenderDailyRuleHTML_HasLayout(t *testing.T) {
	data := &DailyReportData{
		Name:             "小明",
		Yesterday:        "2026-07-22",
		YesterdayCount:   3,
		ConsecutiveZeros: 0,
		Last7Days:        []DayCount{{Date: "2026-07-22", Count: 3}},
		Last7DaysAC:      []DayCount{{Date: "2026-07-22", Count: 2}},
		YesterdayLogs: []SubmitItem{
			{Platform: "CF", Title: "A+B", Status: "AC", Time: "21:36", Tags: []string{"DP", "贪心"}, Difficulty: "1200"},
		},
		YesterdayTagHits: map[string]int{"DP": 2},
	}
	html := RenderDailyRuleHTML(data, "GoAlgo")
	for _, want := range []string{"<!DOCTYPE html>", "小明", "昨日提交", "近 7 日", "A+B", "DP", "171717", "fafafa", "</html>"} {
		if !strings.Contains(html, want) {
			t.Fatalf("missing %q", want)
		}
	}
	for _, want := range []string{`data-series="提交"`, `data-series="AC"`, `data-axis="y"`, `overflow-x:auto`, `min-width:760px`, "提交时间", "题目标签", "难度", "21:36", "DP、贪心", "1200"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing daily chart/detail %q", want)
		}
	}
	for _, want := range []string{">日期</th>", ">提交</th>", ">AC</th>", "07-22"} {
		if !strings.Contains(html, want) {
			t.Errorf("daily trend fallback table missing %q", want)
		}
	}
}

func TestSanitizeDailyHTML(t *testing.T) {
	okHTML := `<table><tr><td><p>日报内容足够长的一段文字用来过长度检查</p></td></tr></table>`
	out, ok, reason := SanitizeDailyHTML(okHTML)
	if !ok {
		t.Fatalf("want ok: %s", reason)
	}
	if !strings.Contains(out, "<html") {
		t.Fatal("should wrap document")
	}

	_, ok, _ = SanitizeDailyHTML("这只是纯文本没有标签")
	if ok {
		t.Fatal("plain should fail")
	}

	_, ok, _ = SanitizeDailyHTML("```html\n<p>x</p>\n```")
	// after fence strip may still be short
	_ = ok
}

func TestBuildNotifyEmail_InjectsInsideBody(t *testing.T) {
	doc := `<!DOCTYPE html><html><body><p>report</p></body></html>`
	job := &TrainingReportJob{JobID: "j1", StartDate: "2026-07-01", EndDate: "2026-07-07", ExpiresAt: 1780000000, FileName: "r.html"}
	_, body, name := BuildNotifyEmail(job, "GoAlgo", doc)
	if name != "r.html" {
		t.Fatalf("name %s", name)
	}
	if !strings.Contains(body, "任务 j1") {
		t.Fatalf("missing footer: %s", body)
	}
	// footer must be before </body>
	idxFoot := strings.Index(body, "任务 j1")
	idxBody := strings.Index(strings.ToLower(body), "</body>")
	if idxFoot < 0 || idxBody < 0 || idxFoot > idxBody {
		t.Fatalf("footer not inside body: foot=%d body=%d", idxFoot, idxBody)
	}
	if strings.HasSuffix(strings.TrimSpace(body), "任务 j1 · 下载有效期") {
		t.Fatal("footer outside document")
	}
}
