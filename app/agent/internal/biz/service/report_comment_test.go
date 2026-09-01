package service

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAIReportComment_Valid(t *testing.T) {
	raw := `{"headline":"状态不错 🚀","trendChanges":["提交节奏回升"],"highlights":["昨日提交 5 次","标签 DP 有进步"],"issues":[],"dimensionAnalysis":["活跃度稳定"],"suggestions":["今天做 1 题","复盘错题"]}`
	c, err := ParseAIReportComment(raw)
	if err != nil {
		t.Fatalf("want ok: %v", err)
	}
	if c.Headline != "状态不错 🚀" {
		t.Fatalf("headline=%q", c.Headline)
	}
	if len(c.TrendChanges) != 1 || len(c.Highlights) != 2 || len(c.Issues) != 0 || len(c.DimensionAnalysis) != 1 || len(c.Suggestions) != 2 {
		t.Fatalf("lists wrong: %+v", c)
	}
}

func TestParseAIReportComment_ToleratesFenceAndPreamble(t *testing.T) {
	raw := "现在我已获取数据：\n```json\n{\"headline\":\"点评\",\"highlights\":[\"亮点\"],\"issues\":[],\"suggestions\":[]}\n```"
	c, err := ParseAIReportComment(raw)
	if err != nil {
		t.Fatalf("want ok: %v", err)
	}
	if c.Headline != "点评" {
		t.Fatalf("headline=%q", c.Headline)
	}
}

func TestParseAIReportComment_RejectsInvalid(t *testing.T) {
	cases := []string{
		"",
		"没有 JSON",
		"```html\n<table>…</table>\n```",
		`{"headline":"","trendChanges":[],"highlights":[],"issues":[],"dimensionAnalysis":[],"suggestions":[]}`,
	}
	for _, raw := range cases {
		if _, err := ParseAIReportComment(raw); err == nil {
			t.Fatalf("want error for %q", raw)
		}
	}
}

func TestParseAIReportComment_Truncates(t *testing.T) {
	long := strings.Repeat("长", 200)
	raw := `{"headline":"` + long + `","highlights":["1","2","3","4","5","6"],"issues":[],"suggestions":[]}`
	c, err := ParseAIReportComment(raw)
	if err != nil {
		t.Fatalf("want ok: %v", err)
	}
	if len([]rune(c.Headline)) > aiHeadlineMaxRunes+3 {
		t.Fatalf("headline not truncated: %d", len([]rune(c.Headline)))
	}
	if len(c.Highlights) > aiListMaxItems {
		t.Fatalf("highlights not capped: %d", len(c.Highlights))
	}
}

func TestRenderDailyHTMLWithComment_UsesEmailSafeChartAndComment(t *testing.T) {
	data := &DailyReportData{
		Name:           "测试",
		Yesterday:      "2026-08-07",
		YesterdayCount: 3,
		Last7Days:      []DayCount{{Date: "2026-08-01", Count: 1}, {Date: "2026-08-02", Count: 2}, {Date: "2026-08-03", Count: 0}, {Date: "2026-08-04", Count: 4}, {Date: "2026-08-05", Count: 0}, {Date: "2026-08-06", Count: 1}, {Date: "2026-08-07", Count: 3}},
		YesterdayLogs:  []SubmitItem{{Platform: "NowCoder", Problem: "P1", Status: "AC"}},
	}
	comment := AIReportComment{
		Headline:    "状态不错 🚀",
		Highlights:  []string{"昨日提交 3 次"},
		Issues:      []string{"AC 率可提高"},
		Suggestions: []string{"多做中等题"},
	}
	html := RenderDailyHTMLWithComment(data, "GoAlgo", comment)
	if strings.Contains(html, "<svg") || strings.Contains(html, "<polyline") || strings.Contains(html, "<rect") || strings.Contains(html, "<canvas") || strings.Contains(strings.ToLower(html), ".png") {
		t.Fatal("email chart must not contain SVG tags")
	}
	if strings.Contains(html, "data-chart=\"bar\"") {
		t.Fatal("daily email must not contain a chart")
	}
	if !strings.Contains(html, "提交") || !strings.Contains(html, "AC") {
		t.Fatal("daily email must retain trend data")
	}
	if !strings.Contains(html, "状态不错 🚀") {
		t.Fatal("missing headline")
	}
	if !strings.Contains(html, "AC 率可提高") {
		t.Fatal("missing issue item")
	}
	if !strings.Contains(html, "昨日比赛") {
		t.Fatal("missing contest section title")
	}
}

func TestRenderTrainingReportVariants_SplitsAttachmentAndEmailCharts(t *testing.T) {
	data := &TrainingReportData{
		OrgID:            1,
		ScopeLabel:       "整组织",
		StartDate:        "2026-07-27",
		EndDate:          "2026-08-02",
		MemberCount:      10,
		TotalSubmits:     42,
		PrevTotalSubmits: 30,
		TotalAC:          18,
		DailyTrend:       []DayCount{{Date: "2026-07-27", Count: 5}, {Date: "2026-07-28", Count: 8}, {Date: "2026-07-29", Count: 3}},
		DailyACTrend:     []DayCount{{Date: "2026-07-27", Count: 2}, {Date: "2026-07-28", Count: 5}, {Date: "2026-07-29", Count: 1}},
		ActiveRanking:    []MemberStat{{Rank: 1, UserID: 1, Name: "张三", Submits: 10, AC: 5}},
		ActiveMembers:    3,
	}
	comment := AIReportComment{
		Headline:          "整体活跃上升 🔥",
		TrendChanges:      []string{"提交节奏回升"},
		Highlights:        []string{"训练质量改善"},
		Issues:            []string{"部分成员未提交"},
		DimensionAnalysis: []string{"活跃度覆盖仍有提升空间"},
		Suggestions:       []string{"组织统一训练日"},
	}
	attachment, email := RenderTrainingReportVariants(data, "GoAlgo", comment, DetailModeCompact)
	if !strings.Contains(attachment, "<svg") || !strings.Contains(attachment, "<polyline") {
		t.Fatal("attachment must contain the SVG line chart")
	}
	if strings.Contains(email, "<svg") || strings.Contains(email, "<polyline") || strings.Contains(email, "<rect") || strings.Contains(email, "<canvas") || strings.Contains(strings.ToLower(email), ".png") {
		t.Fatal("email chart must not contain SVG tags")
	}
	if strings.Contains(email, "data-chart=\"bar\"") {
		t.Fatal("report email must not contain a chart")
	}
	if !strings.Contains(email, "1. 活跃度与趋势") {
		t.Fatal("report email must retain the trend section")
	}
	for _, output := range []string{attachment, email} {
		for _, want := range []string{"总体判断", "趋势变化", "亮点", "问题", "分维度分析", "可执行建议", "整体活跃上升 🔥", "42"} {
			if !strings.Contains(output, want) {
				t.Errorf("report missing %q", want)
			}
		}
	}
}

func TestRenderTemplateHTMLWithComment_DefaultsToAttachment(t *testing.T) {
	html := RenderTemplateHTMLWithComment(fixtureTrainingData(), "GoAlgo", AIReportComment{Headline: "总体稳定"}, DetailModeCompact)
	if !strings.Contains(html, "<svg") || !strings.Contains(html, "<polyline") {
		t.Fatal("default render must remain the downloadable attachment variant")
	}
}

func TestEmailBarChart_UsesTables(t *testing.T) {
	chart := EmailBarChart([]string{"08-01", "08-02"}, [][]int64{{1, 5}}, []string{"提交"}, []string{"#171717"})
	if !strings.Contains(chart, `data-chart="bar"`) || !strings.Contains(chart, "08-01") || !strings.Contains(chart, "width:100%") {
		t.Fatal("bad email chart: " + chart)
	}
	if strings.Contains(chart, "<svg") || strings.Contains(chart, "<rect") {
		t.Fatal("chart contains unsupported drawing tags")
	}
}

func TestEmailBarChart_HandlesSinglePointAndMissingValues(t *testing.T) {
	chart := EmailBarChart(
		[]string{"08-15"},
		[][]int64{{3}, {2, 4}},
		[]string{"提交", "AC"},
		[]string{"#171717", "#f97316"},
	)
	for _, want := range []string{`data-series="提交"`, `data-series="AC"`, "08-15", ">3<", ">2<"} {
		if !strings.Contains(chart, want) {
			t.Errorf("missing %q in %s", want, chart)
		}
	}
	if strings.Contains(chart, "NaN") || strings.Contains(chart, "Inf") {
		t.Fatalf("invalid chart: %s", chart)
	}
}

func TestEmailBarChart_EscapesLabels(t *testing.T) {
	chart := EmailBarChart(
		[]string{"08-09", "08-15"},
		[][]int64{{12, 7}},
		[]string{"提交", "AC"},
		[]string{"#171717", "#f97316"},
	)
	if !strings.Contains(chart, "08-09") || strings.Contains(chart, "<svg") {
		t.Fatalf("unexpected chart: %s", chart)
	}
}

func TestDailyACTrend_FallsBackToAlignedZeros(t *testing.T) {
	days := []DayCount{{Date: "2026-08-14", Count: 3}, {Date: "2026-08-15", Count: 5}}
	got := dailyACTrend(days, nil, errors.New("rpc unavailable"))
	if len(got) != len(days) {
		t.Fatalf("got %d days, want %d", len(got), len(days))
	}
	for i := range got {
		if got[i].Date != days[i].Date || got[i].Count != 0 {
			t.Fatalf("day %d = %+v, want date %s count 0", i, got[i], days[i].Date)
		}
	}
}
