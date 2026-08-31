package service

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAIReportComment_Valid(t *testing.T) {
	raw := `{"headline":"状态不错 🚀","trendChanges":["训练节奏连续回升"],"highlights":["昨日提交 5 次","标签 DP 有进步"],"issues":[],"dimensionAnalysis":["活跃度覆盖较好"],"suggestions":["今天做 1 题","复盘错题"]}`
	c, err := ParseAIReportComment(raw)
	if err != nil {
		t.Fatalf("want ok: %v", err)
	}
	if c.Headline != "状态不错 🚀" {
		t.Fatalf("headline=%q", c.Headline)
	}
	if len(c.TrendChanges) != 1 || len(c.Highlights) != 2 || len(c.DimensionAnalysis) != 1 || len(c.Suggestions) != 2 || len(c.Issues) != 0 {
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
		`{"headline":"","highlights":[],"issues":[],"suggestions":[]}`,
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

func TestRenderDailyHTMLWithComment_HasSVGAndComment(t *testing.T) {
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
	if !strings.Contains(html, "<svg") {
		t.Fatal("missing svg chart")
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

func TestRenderTemplateHTMLWithComment_HasSVGAndComment(t *testing.T) {
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
		TrendChanges:      []string{"近期训练节奏持续改善"},
		Highlights:        []string{"提交表现优于上期"},
		Issues:            []string{"仍有成员未提交"},
		DimensionAnalysis: []string{"活跃度：核心成员保持稳定输出"},
		Suggestions:       []string{"组织统一训练日"},
	}
	html := RenderTemplateHTMLWithComment(data, "GoAlgo", comment, DetailModeCompact)
	if !strings.Contains(html, "<svg") {
		t.Fatal("missing svg chart")
	}
	if !strings.Contains(html, "整体活跃上升 🔥") {
		t.Fatal("missing headline")
	}
	if !strings.Contains(html, "有 7 名成员未提交") {
		if !strings.Contains(html, "仍有成员未提交") {
			t.Fatal("missing issue")
		}
	}
	if !strings.Contains(html, "42") {
		t.Fatal("missing data-driven number")
	}
}

func TestTrainingReportRenderModes_UseCompatibleCharts(t *testing.T) {
	data := fixtureTrainingData()
	comment := AIReportComment{Headline: "同一份判断", TrendChanges: []string{"趋势结论唯一"}}
	attachment, email := RenderTrainingReportVariants(data, "GoAlgo", comment, DetailModeFull)

	if !strings.Contains(strings.ToLower(attachment), "<svg") || !strings.Contains(attachment, `data-series="提交"`) {
		t.Fatalf("attachment should retain SVG line chart: %s", attachment)
	}
	lowerEmail := strings.ToLower(email)
	for _, forbidden := range []string{"<svg", "<img", ".png", "<canvas"} {
		if strings.Contains(lowerEmail, forbidden) {
			t.Fatalf("email contains forbidden chart format %q", forbidden)
		}
	}
	for _, want := range []string{`data-report-chart="email-bar"`, "同一份判断", "趋势结论唯一", "42", "Alice"} {
		if !strings.Contains(email, want) {
			t.Fatalf("email missing shared data/comment marker %q", want)
		}
	}
}

func TestRenderTrainingComment_HasSixSections(t *testing.T) {
	comment := AIReportComment{
		Headline:          "总体判断内容",
		TrendChanges:      []string{"趋势变化内容"},
		Highlights:        []string{"亮点内容"},
		Issues:            []string{"问题内容"},
		DimensionAnalysis: []string{"分维度内容"},
		Suggestions:       []string{"可执行建议内容"},
	}
	html := RenderTemplateHTMLWithComment(fixtureTrainingData(), "GoAlgo", comment, DetailModeFull)
	for _, want := range []string{"总体判断", "趋势变化", "亮点", "问题", "分维度分析", "可执行建议", "总体判断内容", "分维度内容"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing six-section content %q", want)
		}
	}
}

func TestRuleReportComment_EmptyDataAvoidsHollowDimensions(t *testing.T) {
	c := RuleReportComment(&TrainingReportData{}, 0)
	if len(c.TrendChanges) != 0 || len(c.Highlights) != 0 || len(c.DimensionAnalysis) != 0 {
		t.Fatalf("empty data should not produce hollow analysis: %+v", c)
	}
}

func TestBarChartSVG_OutputsSVG(t *testing.T) {
	svg := BarChartSVG([]string{"08-01", "08-02"}, []int64{1, 5}, "#171717")
	if !strings.HasPrefix(svg, "<svg") || !strings.Contains(svg, "viewBox") {
		t.Fatal("bad svg: " + svg)
	}
}

func TestLineChartSVG_HasYAxisAndHandlesSinglePoint(t *testing.T) {
	svg := LineChartSVG(
		[]string{"08-15"},
		[][]int64{{3}, {2}},
		[]string{"提交", "AC"},
		[]string{"#171717", "#f97316"},
	)
	for _, want := range []string{`data-axis="y"`, `data-series="提交"`, `data-series="AC"`, ">0</text>"} {
		if !strings.Contains(svg, want) {
			t.Errorf("missing %q in %s", want, svg)
		}
	}
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Inf") {
		t.Fatalf("invalid single-point coordinates: %s", svg)
	}
}

func TestLineChartSVG_KeepsEdgeLabelsInsidePlot(t *testing.T) {
	svg := LineChartSVG(
		[]string{"08-09", "08-15"},
		[][]int64{{12, 7}, {5, 4}},
		[]string{"提交", "AC"},
		[]string{"#171717", "#f97316"},
	)
	for _, want := range []string{`data-point-edge="first" cx="38.0"`, `data-point-edge="last" cx="532.0"`, `data-value-label="top"`, `y="22.0" text-anchor="middle"`} {
		if !strings.Contains(svg, want) {
			t.Errorf("missing safe chart geometry %q in %s", want, svg)
		}
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
