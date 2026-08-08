package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// AIReportComment LLM 输出的报告文案参数（仅评价性文案）。
// 所有数字由数据层注入模板渲染；LLM 文案只作点评/建议，禁止作为数据来源。
// 规则模式生成同构结构，与 AI 模式共用同一模板渲染。
type AIReportComment struct {
	// Headline 一句话总评（含 emoji）
	Headline string `json:"headline"`
	// Highlights 亮点（0-5 条）
	Highlights []string `json:"highlights,omitempty"`
	// Issues 问题/风险（0-5 条）
	Issues []string `json:"issues,omitempty"`
	// Suggestions 建议（0-5 条）
	Suggestions []string `json:"suggestions,omitempty"`
}

const (
	aiHeadlineMaxRunes = 80
	aiListItemMaxRunes = 100
	aiListMaxItems     = 5
)

// ParseAIReportComment 解析并校验 LLM JSON 输出；失败返回 error（调用方回退规则文案）。
// 容忍代码围栏与前后缀文字（提取首个 JSON 对象），单条超长截断。
func ParseAIReportComment(raw string) (AIReportComment, error) {
	var c AIReportComment
	s := strings.TrimSpace(stripCodeFence(raw))
	// 提取首个 { ... }：LLM 可能输出前言/后记
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return c, fmt.Errorf("输出中无 JSON 对象")
	}
	// 从 '{' 起解析（json.Unmarshal 遇尾随内容报错，用 Decoder 截断到对象结尾）
	dec := json.NewDecoder(strings.NewReader(s[start:]))
	if err := dec.Decode(&c); err != nil {
		return c, fmt.Errorf("JSON 解析失败: %w", err)
	}

	c.Headline = truncateRunes(strings.TrimSpace(c.Headline), aiHeadlineMaxRunes)
	c.Highlights = normalizeCommentList(c.Highlights)
	c.Issues = normalizeCommentList(c.Issues)
	c.Suggestions = normalizeCommentList(c.Suggestions)

	if c.Headline == "" && len(c.Highlights) == 0 && len(c.Issues) == 0 && len(c.Suggestions) == 0 {
		return c, fmt.Errorf("评论参数为空")
	}
	return c, nil
}

func normalizeCommentList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if utf8.RuneCountInString(s) > aiListItemMaxRunes {
			s = string([]rune(s)[:aiListItemMaxRunes])
		}
		out = append(out, s)
		if len(out) >= aiListMaxItems {
			break
		}
	}
	return out
}

// RuleDailyComment 规则模式：个人日报评论参数（与 LLM 输出同构）
func RuleDailyComment(data *DailyReportData) AIReportComment {
	c := AIReportComment{}
	if data.YesterdayCount == 0 {
		c.Headline = fmt.Sprintf("昨天没有提交，已连续 %d 天未交，休息够了就动起来 💪", data.ConsecutiveZeros)
		c.Issues = append(c.Issues, fmt.Sprintf("已连续 %d 天未提交，节奏断档", data.ConsecutiveZeros))
		c.Suggestions = append(c.Suggestions, "今天先做 1-2 道热手题恢复节奏")
	} else {
		c.Headline = fmt.Sprintf("昨天提交 %d 次，状态不错，继续保持 🚀", data.YesterdayCount)
		c.Highlights = append(c.Highlights, fmt.Sprintf("昨日提交 %d 次", data.YesterdayCount))
	}
	if len(data.YesterdayLogs) > 0 {
		plats := map[string]int{}
		for _, l := range data.YesterdayLogs {
			plats[l.Platform]++
		}
		if len(plats) == 1 {
			for p := range plats {
				c.Highlights = append(c.Highlights, "平台："+p)
			}
		}
	}
	if len(data.RecentContests) > 0 {
		best := data.RecentContests[0]
		if best.Rank > 0 {
			c.Highlights = append(c.Highlights, fmt.Sprintf("近期比赛 %s 排名 %d（AC %d）", best.ContestName, best.Rank, best.ACCount))
		}
	}
	if len(c.Suggestions) == 0 {
		c.Suggestions = append(c.Suggestions, "保持节奏，注意 AC 率与错题复盘")
	}
	return c
}

// RuleReportComment 规则模式：训练报告/周报评论参数（与 LLM 输出同构）
func RuleReportComment(data *TrainingReportData, delta int64) AIReportComment {
	emoji, lines, advice := ruleComprehensiveEval(data, delta)
	c := AIReportComment{}
	if len(lines) > 0 {
		c.Headline = emoji + " " + strings.TrimPrefix(lines[0], "· ")
	}
	for _, l := range lines[1:] {
		item := strings.TrimPrefix(l, "· ")
		if strings.Contains(item, "上升") || strings.Contains(item, "稳定") || strings.Contains(item, "已剔除教练") {
			continue
		}
		if strings.Contains(item, "环比") || strings.Contains(item, "AC 率") || strings.Contains(item, "活跃度") {
			c.Highlights = append(c.Highlights, item)
			continue
		}
		c.Issues = append(c.Issues, item)
	}
	c.Suggestions = advice
	return c
}
