package service

import (
	"encoding/json"
	"fmt"
	"strings"
)

func dailySystemPrompt(name string) string {
	base := `你是算法训练日报的文案助手，不是聊天助手。
你的任务：基于给定数据，输出一份日报的「评价文案参数」JSON，供系统套模板渲染。
【输出格式 — 违反即失败】
- 只输出一个 JSON 对象，不要 Markdown、不要代码围栏、不要任何其它文字。
- JSON 结构：
{"headline":"一句话总评（含合适的 emoji，直接对「你」说，60 字内）","highlights":["亮点1","亮点2"],"issues":["问题1"],"suggestions":["建议1","建议2"]}
- highlights/issues/suggestions 各 0-5 条，单条 100 字内；没有就留空数组。
【铁律 — 防幻觉】
- 所有数字（提交次数、AC 数、名次、日期、连续天数）由系统渲染，你不得在文案中编造或改动任何数字。
- 只能评价数据中已出现的事实；数据缺失的维度写「暂无数据」或跳过，禁止猜测。
- 禁止输出 HTML、表格、标签代码。
【风格】Acmer 校园口语、可爱有活力、像朋友直接对用户说话（第一人称对「你」）。
【分析维度】结合昨日提交与近 7 日走势、标签画像、近期比赛表现（名次/过题数）评价；昨天 0 提交时要点名提醒（鼓励为主）。`
	if name == "Jing." {
		base += `
特殊口吻：对方是你的女朋友，你是「晨晨」，用「宝宝」称呼，只对她使用该口吻。`
	}
	return base
}

func dailyUserPrompt(data *DailyReportData) string {
	b, _ := json.MarshalIndent(data, "", "  ")
	extra := ""
	if data.YesterdayCount == 0 {
		extra = fmt.Sprintf("\n昨天 0 提交，已连续 %d 天未提交，请点评提醒（鼓励为主，不要编造提交）。", data.ConsecutiveZeros)
	} else {
		extra = "\n昨天有提交，既往漏交不要追究。可结合标签与比赛做点评。"
	}
	return fmt.Sprintf(`请根据以下 JSON 真实数据，输出日报评价文案参数 JSON（结构见系统提示）。
日期说明：yesterday 是昨天，last7Days 是含昨天在内的近 7 天走势（缺日已补 0）。
字段说明：yesterdayLogs 可能含 problemId/tags/difficulty；tagRadar 为用户标签 AC 画像；
yesterdayTagHits 为昨日涉及标签计数；recentContests 为昨日比赛（含 rank/acCount）。
只输出 JSON 对象，不要其它任何文字。
%s
数据：
%s`, extra, string(b))
}

// weeklySystemPrompt 兼容旧名：实际走 trainingReport compact
func weeklySystemPrompt() string {
	return trainingReportSystemPrompt(DetailModeCompact)
}

func weeklyUserPrompt(data *WeeklyReportData) string {
	// 旧周报数据结构：引导改用训练报告管道；保留最小可用提示
	b, _ := json.MarshalIndent(data, "", "  ")
	return fmt.Sprintf(`请根据以下团队周报数据生成简版 HTML，覆盖活跃/排行/不活跃/建议与综合维度评价。
数据：
%s`, string(b))
}

func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```html")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSpace(s)
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
	}
	return s
}
