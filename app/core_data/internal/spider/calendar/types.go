package calendar

import "strings"

// Item 归一化后的公开赛程
type Item struct {
	Platform     string
	PlatformName string
	ExternalID   string
	Name         string
	URL          string
	StartTime    int64 // Unix 秒
	EndTime      int64
	Source       string
	IconURL      string
}

// NormalizePlatform 将 cpolar/leetcode 源 id 规范为与爬虫一致的展示名
//（AtCoder / CodeForces / …），避免日历 platform=atcoder 与参赛记录 AtCoder 对不上。
func NormalizePlatform(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "atcoder":
		return "AtCoder"
	case "codeforces", "cf":
		return "CodeForces"
	case "nowcoder":
		return "NowCoder"
	case "leetcode":
		return "LeetCode"
	case "luogu":
		return "LuoGu"
	case "qoj":
		return "QOJ"
	case "loj":
		return "LOJ"
	case "uoj":
		return "UOJ"
	case "poj":
		return "POJ"
	default:
		return strings.TrimSpace(raw)
	}
}
