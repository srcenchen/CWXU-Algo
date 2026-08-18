package task

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// spiderDisplayName 资料页/失败文案用的平台中文名
func spiderDisplayName(platform string) string {
	switch strings.TrimSpace(platform) {
	case "CodeForces", "codeforces", "CF", "cf":
		return "Codeforces"
	case "NowCoder", "nowcoder":
		return "牛客"
	case "AtCoder", "atcoder":
		return "AtCoder"
	case "LuoGu", "Luogu", "luogu":
		return "洛谷"
	case "LeetCode", "leetcode":
		return "力扣"
	case "QOJ", "qoj":
		return "QOJ"
	case "LOJ", "loj":
		return "LOJ"
	case "UOJ", "uoj":
		return "UOJ"
	case "POJ", "poj":
		return "POJ"
	default:
		if platform == "" {
			return "OJ"
		}
		return platform
	}
}

// FormatSpiderLastError 把爬虫原始 error 收成资料页可展示的短中文。
// 区分「多半是用户名/绑定」与「多半是站点/网络/爬虫」，避免把 HTML 与 all platforms failed 甩给用户。
func FormatSpiderLastError(platform string, err error) string {
	if err == nil {
		return ""
	}
	raw := strings.TrimSpace(err.Error())
	if raw == "" {
		return spiderDisplayName(platform) + "同步失败"
	}
	name := spiderDisplayName(platform)
	fault, reason := classifySpiderErr(raw)
	switch fault {
	case spiderFaultUser:
		return fmt.Sprintf("%s：%s。请检查绑定的用户名是否正确", name, reason)
	case spiderFaultTransient:
		return fmt.Sprintf("%s：%s。请稍后再试", name, reason)
	case spiderFaultSystem:
		return fmt.Sprintf("%s：%s。一般不是账号问题，请稍后再试", name, reason)
	default:
		return fmt.Sprintf("%s：%s", name, reason)
	}
}

// IsUserSideSpiderErr 判断爬虫失败是否属于「用户侧」（绑定用户名错误等）。
// 用户侧失败不算平台/系统异常：不计入今日失败、不写 OJ 级最近失败、MQ 不再重试。
// transient（页面结构变化/需登录/风控）同样按用户侧处理：只提示本人，不刷运维。
func IsUserSideSpiderErr(platform string, err error) bool {
	if err == nil {
		return false
	}
	raw := strings.TrimSpace(err.Error())
	if raw == "" {
		return false
	}
	fault, _ := classifySpiderErr(raw)
	return fault == spiderFaultUser || fault == spiderFaultTransient
}

type spiderFaultKind int

const (
	spiderFaultUnknown spiderFaultKind = iota
	spiderFaultUser
	spiderFaultTransient
	spiderFaultSystem
)

func classifySpiderErr(raw string) (spiderFaultKind, string) {
	lower := strings.ToLower(raw)
	// 剥掉 LoadData 包装，便于匹配内层
	inner := raw
	if i := strings.Index(lower, "all platforms failed"); i >= 0 {
		if j := strings.Index(raw[i:], ": "); j >= 0 {
			inner = strings.TrimSpace(raw[i+j+2:])
			lower = strings.ToLower(inner)
		}
	}
	// 剥 "CodeForces: " / "platform X:" 前缀
	if j := strings.Index(inner, ": "); j > 0 && j < 24 {
		prefix := inner[:j]
		if !strings.Contains(prefix, " ") || strings.Contains(strings.ToLower(prefix), "codeforces") {
			// 保留有信息的内层；若整段都是包装再往下
			rest := strings.TrimSpace(inner[j+2:])
			if rest != "" {
				inner = rest
				lower = strings.ToLower(inner)
			}
		}
	}

	// —— 用户侧：账号/句柄不存在 ——
	userHints := []string{
		"not found",
		"handle is empty",
		"handle 为空",
		"username 为空",
		"用户不存在",
		"用户名不存在",
		"unknown handle",
		"no such user",
		"invalid handle",
		"找不到用户",
	}
	for _, h := range userHints {
		if strings.Contains(lower, h) {
			return spiderFaultUser, "未找到该用户"
		}
	}
	if strings.Contains(lower, "resolve luogu uid") && strings.Contains(lower, "luogu search 解析失败") {
		return spiderFaultUser, "无法识别该用户"
	}

	// —— 暂态：页面结构变化 / 需要登录 / 风控页 ——
	// 不 blame 用户名，也不算平台故障；按用户侧处理（只提示本人，不刷运维）
	if strings.Contains(lower, "未找到 _feinjection") || strings.Contains(lower, "_feinjection") {
		return spiderFaultTransient, "对方页面暂时无法解析"
	}
	// 用户记录数 >0 但记录列表为空：多为该用户记录页不可见/需登录，属用户侧，不算平台异常
	if strings.Contains(lower, "records page empty") {
		return spiderFaultTransient, "对方记录页暂时不可见"
	}

	// —— 系统侧：封禁 / 网关 / 超时 / HTML 墙 ——
	if strings.Contains(lower, "403") || strings.Contains(lower, "forbidden") {
		return spiderFaultSystem, "对方站点暂时拒绝访问"
	}
	if strings.Contains(lower, "429") || strings.Contains(lower, "too many request") || strings.Contains(lower, "rate limit") {
		return spiderFaultSystem, "请求过于频繁，已被限流"
	}
	if strings.Contains(lower, "401") || strings.Contains(lower, "unauthorized") {
		return spiderFaultSystem, "对方站点鉴权失败"
	}
	if strings.Contains(lower, "502") || strings.Contains(lower, "503") || strings.Contains(lower, "504") ||
		strings.Contains(lower, "bad gateway") || strings.Contains(lower, "unavailable") {
		return spiderFaultSystem, "对方站点暂时不可用"
	}
	if strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded") ||
		strings.Contains(lower, "i/o timeout") || strings.Contains(lower, "context canceled") {
		return spiderFaultSystem, "拉取超时"
	}
	if strings.Contains(lower, "connection refused") || strings.Contains(lower, "connection reset") ||
		strings.Contains(lower, "no such host") || strings.Contains(lower, "network is unreachable") ||
		strings.Contains(lower, "tls") || strings.Contains(lower, "x509") {
		return spiderFaultSystem, "网络连接失败"
	}
	if strings.Contains(lower, "<html") || strings.Contains(lower, "nginx") ||
		strings.Contains(lower, "cloudflare") || strings.Contains(lower, "<!doctype") {
		return spiderFaultSystem, "对方站点返回了异常页面"
	}
	if strings.Contains(lower, "请求响应码错误") {
		// 带状态码但未命中上面的具体码
		return spiderFaultSystem, "对方站点返回异常状态"
	}
	if strings.Contains(lower, "平台插件不存在") || strings.Contains(lower, "未实现") {
		return spiderFaultSystem, "该平台同步能力异常"
	}
	if strings.Contains(lower, "写入锁占用") {
		return spiderFaultSystem, "同步繁忙"
	}

	// 默认：截断、去 HTML，不当作用户名问题
	clean := stripHTMLNoise(inner)
	clean = collapseSpace(clean)
	if utf8.RuneCountInString(clean) > 80 {
		clean = string([]rune(clean)[:80]) + "…"
	}
	if clean == "" {
		clean = "同步失败"
	}
	return spiderFaultSystem, clean
}

func stripHTMLNoise(s string) string {
	low := strings.ToLower(s)
	if i := strings.Index(low, "<html"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if i := strings.Index(low, "<!doctype"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	// 去掉残余标签碎片
	for {
		start := strings.Index(s, "<")
		if start < 0 {
			break
		}
		end := strings.Index(s[start:], ">")
		if end < 0 {
			s = s[:start]
			break
		}
		s = s[:start] + " " + s[start+end+1:]
	}
	return strings.TrimSpace(s)
}

func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for _, r := range s {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			if prevSpace {
				continue
			}
			prevSpace = true
			b.WriteByte(' ')
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	return strings.TrimSpace(b.String())
}
