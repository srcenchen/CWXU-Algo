package service

import (
	"fmt"
	"html"
	"sort"
	"strings"

	"cwxu-algo/app/common/mail"
)

// RenderDailyRuleHTML 非 AI 日报：规则文案参数 + 统一模板。
func RenderDailyRuleHTML(data *DailyReportData, brand string) string {
	return renderDailyHTML(data, brand, RuleDailyComment(data))
}

// RenderDailyHTMLWithComment AI 日报：LLM 文案参数 + 统一模板。
// 数字/表格/走势图全部由数据层渲染，comment 仅作点评/建议。
func RenderDailyHTMLWithComment(data *DailyReportData, brand string, comment AIReportComment) string {
	return renderDailyHTML(data, brand, comment)
}

func renderDailyHTML(data *DailyReportData, brand string, comment AIReportComment) string {
	if data == nil {
		return ""
	}
	if brand == "" {
		brand = mail.DefaultBrand
	}
	name := data.Name
	if name == "" {
		name = "同学"
	}
	home := SiteBaseURL + "/"
	profile := SiteBaseURL + "/profile"

	subtitle := formatCNDate(data.Yesterday) + " 训练回顾"
	extra := fmt.Sprintf(
		`<div style="font-size:11px;margin-top:10px;"><a href="%s" style="color:%s;text-decoration:underline;text-underline-offset:2px;opacity:0.9;">打开主站</a></div>`,
		html.EscapeString(home), mail.ColorPrimaryFg,
	)
	var b strings.Builder
	b.WriteString(mail.DocShellOpen(
		brand+" 日报",
		brand+" · 个人日报",
		"你好，"+name,
		subtitle,
		extra,
	))

	// KPI
	b.WriteString(`<tr><td style="padding:16px 14px 4px;background:`)
	b.WriteString(mail.ColorCard)
	b.WriteString(`;">`)
	b.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="6" border="0"><tr>`)
	b.WriteString(mail.KPICell(fmt.Sprintf("%d", data.YesterdayCount), "昨日提交", ""))
	zeroNote := "保持节奏"
	if data.YesterdayCount == 0 {
		zeroNote = fmt.Sprintf("已连续 %d 天未交", data.ConsecutiveZeros)
	}
	b.WriteString(mail.KPICell(html.EscapeString(zeroNote), "状态", ""))
	b.WriteString(`</tr></table></td></tr>`)

	// 近 7 日
	writeDailySection(&b, "近 7 日提交走势", func() {
		if len(data.Last7Days) == 0 {
			b.WriteString(`<p style="margin:0;color:`)
			b.WriteString(mail.ColorMutedFg)
			b.WriteString(`;font-size:13px;">暂无数据</p>`)
			return
		}
		labels := make([]string, 0, len(data.Last7Days))
		subSeries := make([]int64, 0, len(data.Last7Days))
		acSeries := make([]int64, 0, len(data.Last7Days))
		for i, d := range data.Last7Days {
			labels = append(labels, shortDate(d.Date))
			subSeries = append(subSeries, d.Count)
			ac := int64(0)
			if i < len(data.Last7DaysAC) {
				ac = data.Last7DaysAC[i].Count
			}
			acSeries = append(acSeries, ac)
		}
		b.WriteString(EmailBarChart(labels, [][]int64{subSeries, acSeries}, []string{"提交", "AC"}, []string{"#171717", "#f97316"}))
		b.WriteString(`<div style="margin-top:12px;overflow-x:auto;-webkit-overflow-scrolling:touch;">`)
		b.WriteString(mail.DataTableOpen())
		b.WriteString(`<tr>`)
		b.WriteString(mail.TH("日期", "left"))
		b.WriteString(mail.TH("提交", "right"))
		b.WriteString(mail.TH("AC", "right"))
		b.WriteString(`</tr>`)
		for i, label := range labels {
			b.WriteString(`<tr>`)
			b.WriteString(mail.TD(html.EscapeString(label), "left"))
			b.WriteString(mail.TD(fmt.Sprintf("%d", subSeries[i]), "right"))
			b.WriteString(mail.TD(fmt.Sprintf("%d", acSeries[i]), "right"))
			b.WriteString(`</tr>`)
		}
		b.WriteString(`</table></div>`)
	})

	// 昨日明细
	writeDailySection(&b, "昨日提交明细", func() {
		if len(data.YesterdayLogs) == 0 {
			msg := "昨天没有提交记录。"
			if data.ConsecutiveZeros > 0 {
				msg = fmt.Sprintf("昨天 0 提交，已连续 %d 天未交。今天开一题就好。", data.ConsecutiveZeros)
			}
			b.WriteString(`<p style="margin:0;color:`)
			b.WriteString(mail.ColorMutedFg)
			b.WriteString(`;font-size:13px;">`)
			b.WriteString(html.EscapeString(msg))
			b.WriteString(`</p>`)
			return
		}
		b.WriteString(`<div style="width:100%;overflow-x:auto;-webkit-overflow-scrolling:touch;">`)
		b.WriteString(`<table cellpadding="0" cellspacing="0" border="0" style="border-collapse:collapse;font-size:13px;width:100%;min-width:760px;">`)
		b.WriteString(`<tr>`)
		b.WriteString(mail.TH("题目", "left"))
		b.WriteString(mail.TH("平台", "left"))
		b.WriteString(mail.TH("结果", "left"))
		b.WriteString(mail.TH("提交时间", "left"))
		b.WriteString(mail.TH("题目标签", "left"))
		b.WriteString(mail.TH("难度", "left"))
		b.WriteString(`</tr>`)
		capN := 20
		for i, log := range data.YesterdayLogs {
			if i >= capN {
				b.WriteString(`<tr><td colspan="6" style="padding:8px;color:`)
				b.WriteString(mail.ColorMutedFg)
				b.WriteString(`;font-size:12px;">…共 `)
				b.WriteString(fmt.Sprintf("%d", len(data.YesterdayLogs)))
				b.WriteString(` 条 · `)
				b.WriteString(mail.Link(profile, "主站查看"))
				b.WriteString(`</td></tr>`)
				break
			}
			title := log.Title
			if title == "" {
				title = log.Problem
			}
			if title == "" {
				title = "—"
			}
			timeText := strings.TrimSpace(log.Time)
			if timeText == "" {
				timeText = "—"
			}
			tags := "—"
			if len(log.Tags) > 0 {
				tags = strings.Join(log.Tags, "、")
			}
			difficulty := strings.TrimSpace(log.Difficulty)
			if difficulty == "" {
				difficulty = "—"
			}
			b.WriteString(`<tr>`)
			b.WriteString(mail.TD(html.EscapeString(title), "left"))
			b.WriteString(mail.TD(html.EscapeString(log.Platform), "left"))
			b.WriteString(mail.TD(html.EscapeString(log.Status), "left"))
			b.WriteString(mail.TD(html.EscapeString(timeText), "left"))
			b.WriteString(mail.TD(html.EscapeString(tags), "left"))
			b.WriteString(mail.TD(html.EscapeString(difficulty), "left"))
			b.WriteString(`</tr>`)
		}
		b.WriteString(`</table></div>`)
	})

	// 标签
	writeDailySection(&b, "知识点 / 标签", func() {
		if len(data.YesterdayTagHits) > 0 {
			type kv struct {
				k string
				v int
			}
			var list []kv
			for k, v := range data.YesterdayTagHits {
				list = append(list, kv{k, v})
			}
			sort.Slice(list, func(i, j int) bool {
				if list[i].v != list[j].v {
					return list[i].v > list[j].v
				}
				return list[i].k < list[j].k
			})
			b.WriteString(`<p style="margin:0 0 6px;font-size:12px;color:`)
			b.WriteString(mail.ColorMutedFg)
			b.WriteString(`;">昨日涉及：</p><p style="margin:0;line-height:1.9;">`)
			for i, it := range list {
				if i >= 12 {
					break
				}
				b.WriteString(mail.BadgeSecondary(fmt.Sprintf("%s %d", it.k, it.v)))
			}
			b.WriteString(`</p>`)
		} else if len(data.TagRadar) > 0 {
			b.WriteString(`<p style="margin:0;line-height:1.9;">`)
			for i, t := range data.TagRadar {
				if i >= 10 {
					break
				}
				b.WriteString(mail.BadgeSecondary(t.Tag))
			}
			b.WriteString(`</p>`)
		} else {
			b.WriteString(`<p style="margin:0;color:`)
			b.WriteString(mail.ColorMutedFg)
			b.WriteString(`;font-size:13px;">暂无标签画像</p>`)
		}
	})

	// 比赛（数据层已按昨日过滤）
	writeDailySection(&b, "昨日比赛", func() {
		if len(data.RecentContests) == 0 {
			b.WriteString(`<p style="margin:0;color:`)
			b.WriteString(mail.ColorMutedFg)
			b.WriteString(`;font-size:13px;">昨日暂无比赛记录</p>`)
			return
		}
		b.WriteString(mail.DataTableOpen())
		b.WriteString(`<tr>`)
		b.WriteString(mail.TH("比赛", "left"))
		b.WriteString(mail.TH("名次", "right"))
		b.WriteString(mail.TH("过题", "right"))
		b.WriteString(`</tr>`)
		for i, c := range data.RecentContests {
			if i >= 8 {
				break
			}
			rank := "—"
			if c.Rank > 0 {
				rank = fmt.Sprintf("%d", c.Rank)
			}
			b.WriteString(`<tr>`)
			b.WriteString(mail.TD(html.EscapeString(c.ContestName), "left"))
			b.WriteString(mail.TD(rank, "right"))
			b.WriteString(mail.TD(fmt.Sprintf("%d", c.ACCount), "right"))
			b.WriteString(`</tr>`)
		}
		b.WriteString(`</table>`)
	})

	// 小结（评论参数：AI 或规则生成）
	writeDailySection(&b, "小结", func() {
		if strings.TrimSpace(comment.Headline) != "" {
			b.WriteString(`<p style="margin:0;font-size:14px;font-weight:600;color:`)
			b.WriteString(mail.ColorForeground)
			b.WriteString(`;">`)
			b.WriteString(html.EscapeString(comment.Headline))
			b.WriteString(`</p>`)
		}
		if len(comment.Highlights) > 0 {
			b.WriteString(`<p style="margin:8px 0 2px;font-size:12px;color:`)
			b.WriteString(mail.ColorMutedFg)
			b.WriteString(`;">亮点</p><ul style="margin:4px 0 0;padding-left:18px;font-size:13px;color:#0a0a0a;">`)
			for _, h := range comment.Highlights {
				b.WriteString(`<li style="margin-bottom:3px;">` + html.EscapeString(h) + `</li>`)
			}
			b.WriteString(`</ul>`)
		}
		if len(comment.Issues) > 0 {
			b.WriteString(`<p style="margin:8px 0 2px;font-size:12px;color:`)
			b.WriteString(mail.ColorMutedFg)
			b.WriteString(`;">问题</p><ul style="margin:4px 0 0;padding-left:18px;font-size:13px;color:#0a0a0a;">`)
			for _, it := range comment.Issues {
				b.WriteString(`<li style="margin-bottom:3px;">` + html.EscapeString(it) + `</li>`)
			}
			b.WriteString(`</ul>`)
		}
		if len(comment.Suggestions) > 0 {
			b.WriteString(`<p style="margin:8px 0 2px;font-size:12px;color:`)
			b.WriteString(mail.ColorMutedFg)
			b.WriteString(`;">建议</p><ul style="margin:4px 0 0;padding-left:18px;font-size:13px;color:#0a0a0a;">`)
			for _, s := range comment.Suggestions {
				b.WriteString(`<li style="margin-bottom:3px;">` + html.EscapeString(s) + `</li>`)
			}
			b.WriteString(`</ul>`)
		}
		b.WriteString(`<p style="margin:14px 0 0;font-size:12px;">`)
		b.WriteString(mail.Link(home, "在主站查看完整提交 →"))
		b.WriteString(`</p>`)
	})

	b.WriteString(mail.DocShellClose())
	return b.String()
}

// shortDate 2006-01-02 → MM-DD（图表横轴标签）
func shortDate(ymd string) string {
	if len(ymd) >= 10 {
		return ymd[5:]
	}
	return ymd
}

func writeDailySection(b *strings.Builder, title string, body func()) {
	b.WriteString(`<tr><td style="padding:4px 14px 12px;background:`)
	b.WriteString(mail.ColorCard)
	b.WriteString(`;">`)
	b.WriteString(`<div style="background:`)
	b.WriteString(mail.ColorCard)
	b.WriteString(`;border:1px solid `)
	b.WriteString(mail.ColorBorder)
	b.WriteString(`;border-radius:`)
	b.WriteString(mail.RadiusMd)
	b.WriteString(`;padding:14px 12px;">`)
	b.WriteString(mail.SectionTitle(title))
	body()
	b.WriteString(`</div></td></tr>`)
}

// SanitizeDailyHTML 清洗日报 LLM 输出；失败时 ok=false，调用方应回退规则模板。
func SanitizeDailyHTML(raw string) (htmlOut string, ok bool, reason string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false, "空输出"
	}
	s = extractHTMLDocument(s)
	s = stripCodeFence(s)
	s = stripLLMPreamble(s)
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false, "清洗后为空"
	}
	if !strings.HasPrefix(s, "<") {
		return "", false, "未以 HTML 标签开头"
	}
	if strings.Contains(s, "```") {
		return "", false, "仍含 Markdown 代码围栏"
	}
	lower := strings.ToLower(s)
	if strings.Count(s, "**") >= 3 && !strings.Contains(lower, "<table") && !strings.Contains(lower, "<div") {
		return "", false, "疑似 Markdown"
	}
	hasStruct := strings.Contains(lower, "<table") ||
		strings.Contains(lower, "<div") ||
		strings.Contains(lower, "<p") ||
		strings.Contains(lower, "<html")
	if !hasStruct {
		return "", false, "缺少 HTML 结构"
	}
	if len(s) < 80 {
		return "", false, fmt.Sprintf("过短(%d)", len(s))
	}
	if !strings.Contains(lower, "<html") && !strings.HasPrefix(lower, "<!doctype") {
		s = mail.EnsureDocument(s)
	}
	return s, true, ""
}
