package model

import (
	"strings"
	"time"
)

type SubmitLog struct {
	ID         uint      `gorm:"comment:ID"`
	Platform   string    `gorm:"comment:平台"`
	UserID     int64     `gorm:"comment:用户ID;index;index:idx_submit_user_time,priority:1;index:idx_submit_user_isac_time,priority:1"`
	SubmitID   string    `gorm:"comment:提交ID;unique"`
	Contest    string    `gorm:"comment:比赛名称"`
	Problem    string    `gorm:"comment:问题"`
	Lang       string    `gorm:"comment:语言"`
	Status     string    `gorm:"size:64;comment:状态;index:idx_submit_status_time,priority:1"`
	// IsAC 写入时由 status 归一化；统计读路径只扫 is_ac，避免 UPPER(BTRIM(status)) 全表表达式
	IsAC       bool      `gorm:"column:is_ac;default:false;index;index:idx_submit_user_isac_time,priority:2;comment:是否AC"`
	Time       time.Time `gorm:"comment:提交时间;index;index:idx_submit_user_time,priority:2;index:idx_submit_status_time,priority:2;index:idx_submit_user_isac_time,priority:3"`
	ProblemID  *uint     `gorm:"comment:关联题库ID;index"`
	ExternalID string    `gorm:"comment:平台题号;size:128;index"`
}

// acceptedStatusNorm 归一化后的 AC 状态集合（大写/去空白）
var acceptedStatusNorm = map[string]struct{}{
	"AC":       {},
	"OK":       {},
	"ACCEPTED": {},
	"正确":       {},
	"答案正确":     {},
}

// IsAcceptedStatus 判断提交状态是否为 AC（兼容各 OJ 字面量）
func IsAcceptedStatus(status string) bool {
	s := strings.ToUpper(strings.TrimSpace(status))
	_, ok := acceptedStatusNorm[s]
	if ok {
		return true
	}
	// 中文不受 ToUpper 影响，再试一次原 trim
	_, ok = acceptedStatusNorm[strings.TrimSpace(status)]
	return ok
}

// IsPendingSubmitStatus 评测中 / 无终态（可被后续爬虫回写 status）。
// 覆盖 CF TESTING/IN_QUEUE、通用 Judging、牛客「正在评测/评测中」等。
func IsPendingSubmitStatus(status string) bool {
	s := strings.TrimSpace(status)
	if s == "" {
		return true
	}
	// 中文状态不受 ToUpper 影响，先匹配常见字面量
	switch s {
	case "正在评测", "评测中", "等待评测", "排队中":
		return true
	}
	switch strings.ToUpper(s) {
	case "TESTING", "PENDING", "JUDGING", "IN_QUEUE", "IN QUEUE", "WAITING", "WJ", "QUEUE":
		return true
	default:
		return false
	}
}

// FillIsAC 根据 Status 填充 IsAC（写入前调用）
func (s *SubmitLog) FillIsAC() {
	s.IsAC = IsAcceptedStatus(s.Status)
}

// FillIsACBatch 批量填充
func FillIsACBatch(logs []SubmitLog) {
	for i := range logs {
		logs[i].FillIsAC()
	}
}

// 力扣 submit_id 前缀约定（见 spider/platform/leetcode.go）：
//   lc-cal-*  日历提交次数 → 计入提交统计；不进动态
//   lc-pad-*  生涯提交补齐 → 计入提交统计；不进动态
//   lc-ac-*   合成 AC（无题号）→ 不计提交，计 AC；不进动态
//   lc-prob-* 最近通过明细 → 不计提交（避免与日历双计）；计 AC + 题库；**进动态/提交历史**（无代码）

// CountsTowardSubmitStat 是否计入提交次数 / 提交热力
// 力扣仅 lc-cal / lc-pad 计入；lc-ac / lc-prob 只服务 AC 与题库（lc-prob 另进活动流）。
func CountsTowardSubmitStat(platform, submitID string) bool {
	if platform != "LeetCode" {
		return true
	}
	return !IsLeetCodeNonSubmitCountID(submitID)
}

// IsLeetCodeNonSubmitCountID 力扣不计入提交数的 submit_id（合成 AC + 最近通过明细）
func IsLeetCodeNonSubmitCountID(submitID string) bool {
	return strings.HasPrefix(submitID, "lc-ac-") || strings.HasPrefix(submitID, "lc-prob-")
}

// IsLeetCodeSyntheticSubmit 力扣合成/补齐行：不进活动流与提交明细列表
// 最近通过 lc-prob-* 返回 false（应展示）。
func IsLeetCodeSyntheticSubmit(platform, submitID string) bool {
	if platform != "LeetCode" {
		return false
	}
	// 仅真实最近通过进动态；其余 lc-* 均为合成
	if strings.HasPrefix(submitID, "lc-prob-") {
		return false
	}
	return true
}

// SQLExcludeLeetCodeNonSubmit 提交统计 SQL 片段：排除力扣合成 AC 与最近通过明细
const SQLExcludeLeetCodeNonSubmit = `NOT (platform = 'LeetCode' AND (submit_id LIKE 'lc-ac-%' OR submit_id LIKE 'lc-prob-%'))`

// knownPlatformSubmitPrefixes 历史脏数据/误拼接：submit_id 写成 "LuoGu:123456"
// 真实洛谷链接应为 /record/123456。力扣合成 id 用 lc-*，不会命中下列前缀。
var knownPlatformSubmitPrefixes = []string{
	"LuoGu:", "Luogu:", "LUOGU:",
	"CodeForces:", "Codeforces:", "CODEFORCES:", "CF:",
	"AtCoder:", "Atcoder:", "ATCODER:",
	"NowCoder:", "Nowcoder:", "NOWCODER:",
	"LeetCode:", "Leetcode:", "LEETCODE:",
	"QOJ:", "Qoj:",
}

// NormalizeSubmitID 去掉误写入的「平台:」前缀；不改动力扣 lc-* 合成 id。
func NormalizeSubmitID(platform, submitID string) string {
	id := strings.TrimSpace(submitID)
	if id == "" {
		return id
	}
	// 力扣合成 / 最近通过：保持原样
	if platform == "LeetCode" && strings.HasPrefix(id, "lc-") {
		return id
	}
	// 优先按本行 platform 剥离
	plat := strings.TrimSpace(platform)
	if plat != "" {
		for _, cand := range []string{plat + ":", strings.ToLower(plat) + ":", strings.ToUpper(plat) + ":"} {
			if strings.HasPrefix(id, cand) {
				return strings.TrimSpace(strings.TrimPrefix(id, cand))
			}
		}
	}
	for _, p := range knownPlatformSubmitPrefixes {
		if strings.HasPrefix(id, p) {
			return strings.TrimSpace(strings.TrimPrefix(id, p))
		}
	}
	return id
}

// NormalizeSubmitIDs 批量归一化 submit_id
func NormalizeSubmitIDs(logs []SubmitLog) {
	for i := range logs {
		logs[i].SubmitID = NormalizeSubmitID(logs[i].Platform, logs[i].SubmitID)
	}
}
