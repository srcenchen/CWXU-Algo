package service

import (
	"cwxu-algo/app/core_data/internal/spider"
	"fmt"
	"regexp"
	"strings"
)

// ParsedProblem 从提交记录解析出的题目身份
type ParsedProblem struct {
	Platform   string
	ExternalID string
	Title      string
	URL        string
	SkipFetch  bool // 不可爬取（若仍入库则 SKIPPED）
	SkipBank   bool // 不进入题库
	// FallbackURLs 抓取候选（如牛客比赛页 /acm/contest/{id}/{A}），题库页无权限时回退
	FallbackURLs []string
	// ContestID / ContestLabel：来自比赛题页 URL 时写入 contest_problems 映射
	ContestID    string
	ContestLabel string
}

var (
	reCFProblem = regexp.MustCompile(`^([A-Za-z0-9]+)\s*-\s*(.+)$`)
	// 洛谷题号：P1001 / B2001 / CF1A / AT_abc123_a / SP1 / UVA100 …
	reLuoGuPID    = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9_]*)\s+(.+)$`)
	reLuoGuBare   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	reAtCoderTask = regexp.MustCompile(`^[a-z0-9_]+$`)
	reQOJNum      = regexp.MustCompile(`#?\s*(\d+)`)
	reLOJNum      = regexp.MustCompile(`#?\s*(\d+)`)
	reUOJNum      = regexp.MustCompile(`#?\s*(\d+)`)
	rePOJNum      = regexp.MustCompile(`#?\s*(\d+)`)
	reDigits      = regexp.MustCompile(`^\d+$`)
	// CF API：gym 提交的 contestId 为负数
	reCFNegativeContest = regexp.MustCompile(`^-\d+$`)
	reNowCoderProblemURL  = regexp.MustCompile(`(?i)(?:https?://[^/\s]+)?/acm/problem/(\d+)`)
	reNowCoderPracticeURL = regexp.MustCompile(`(?i)(?:https?://[^/\s]+)?/practice/([0-9a-fA-F-]{32,36})`)
	reNowCoderUUID        = regexp.MustCompile(`(?i)^[0-9a-f]{8}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{4}-?[0-9a-f]{12}$|^[0-9a-f]{32}$`)
	// 力扣 titleSlug：常规题 two-sum；剑指 Offer II / LCR 为混合大小写短码如 iIQa4I、8Zf90G
	reLeetCodeSlug = regexp.MustCompile(`^[a-zA-Z0-9]+(?:-[a-zA-Z0-9]+)*$`)
)

// ParseProblemIdentity 从 SubmitLog 字段解析 (platform, external_id)
func ParseProblemIdentity(platform, contest, problem string) (*ParsedProblem, error) {
	platform = strings.TrimSpace(platform)
	contest = strings.TrimSpace(contest)
	problem = strings.TrimSpace(problem)
	if platform == "" || problem == "" {
		return nil, fmt.Errorf("empty platform or problem")
	}

	switch platform {
	case spider.CodeForces:
		return parseCodeforces(contest, problem)
	case spider.AtCoder:
		return parseAtCoder(contest, problem)
	case spider.LuoGu:
		return parseLuoGu(problem)
	case spider.NowCoder:
		return parseNowCoder(contest, problem)
	case spider.QOJ:
		return parseQOJ(problem)
	case spider.LOJ:
		return parseLOJ(problem)
	case spider.UOJ:
		return parseUOJ(problem)
	case spider.POJ:
		return parsePOJ(problem)
	case spider.LeetCode:
		return parseLeetCode(problem)
	default:
		// 兜底：用 problem 文本 hash 级 id
		ext := sanitizeExternalID(problem)
		if ext == "" {
			return nil, fmt.Errorf("unsupported platform %s", platform)
		}
		return &ParsedProblem{
			Platform:   platform,
			ExternalID: ext,
			Title:      problem,
		}, nil
	}
}

func parseCodeforces(contest, problem string) (*ParsedProblem, error) {
	// Problem 形如 "C-Name" 或 "C1-Name"
	m := reCFProblem.FindStringSubmatch(problem)
	var index, title string
	if m != nil {
		index, title = m[1], strings.TrimSpace(m[2])
	} else {
		// 尝试仅 index
		parts := strings.SplitN(problem, " ", 2)
		index = strings.TrimSpace(parts[0])
		title = problem
		if len(parts) > 1 {
			title = strings.TrimSpace(parts[1])
		}
	}
	if index == "" || contest == "" {
		return nil, fmt.Errorf("cf parse fail: %s %s", contest, problem)
	}
	// CF API：gym 的 contestId 为负数 → 归一为 gym{id}{index}
	if reCFNegativeContest.MatchString(contest) {
		gymID := strings.TrimPrefix(contest, "-")
		return &ParsedProblem{
			Platform:   spider.CodeForces,
			ExternalID: "gym" + gymID + index,
			Title:      title,
			URL:        fmt.Sprintf("https://codeforces.com/gym/%s/problem/%s", gymID, index),
		}, nil
	}
	// 已带 gym 前缀的 contest 字段（兼容）
	if strings.HasPrefix(strings.ToLower(contest), "gym") {
		gymID := contest[3:]
		if gymID == "" {
			return nil, fmt.Errorf("cf gym missing id")
		}
		return &ParsedProblem{
			Platform:   spider.CodeForces,
			ExternalID: "gym" + gymID + index,
			Title:      title,
			URL:        fmt.Sprintf("https://codeforces.com/gym/%s/problem/%s", gymID, index),
		}, nil
	}
	return &ParsedProblem{
		Platform:   spider.CodeForces,
		ExternalID: contest + index,
		Title:      title,
		URL:        fmt.Sprintf("https://codeforces.com/contest/%s/problem/%s", contest, index),
	}, nil
}

func parseAtCoder(contest, problem string) (*ParsedProblem, error) {
	// problem 多为 problem_id 如 abc123_a
	pid := strings.TrimSpace(problem)
	if !reAtCoderTask.MatchString(pid) {
		pid = sanitizeExternalID(pid)
	}
	if pid == "" {
		return nil, fmt.Errorf("atcoder empty problem")
	}
	url := atCoderTaskURLWithContest(contest, pid)
	return &ParsedProblem{
		Platform:   spider.AtCoder,
		ExternalID: pid,
		Title:      pid,
		URL:        url,
	}, nil
}

func parseLuoGu(problem string) (*ParsedProblem, error) {
	// "P1001 标题" / "CF1A 标题" / "AT_abc123_a 标题" 或纯题号
	m := reLuoGuPID.FindStringSubmatch(problem)
	var pid, title string
	if m != nil {
		pid, title = m[1], strings.TrimSpace(m[2])
	} else {
		parts := strings.Fields(problem)
		if len(parts) == 0 {
			return nil, fmt.Errorf("luogu empty")
		}
		pid = parts[0]
		if len(parts) > 1 {
			title = strings.Join(parts[1:], " ")
		} else {
			title = pid
		}
	}
	if !reLuoGuBare.MatchString(pid) {
		return nil, fmt.Errorf("luogu invalid pid: %q", pid)
	}
	pid = normalizeLuoGuPID(pid)
	if title == "" {
		title = pid
	}
	return &ParsedProblem{
		Platform:   spider.LuoGu,
		ExternalID: pid,
		Title:      title,
		URL:        "https://www.luogu.com.cn/problem/" + pid,
	}, nil
}

// normalizeLuoGuPID 统一洛谷题号大小写（AT_ 后段保持原样风格：小写 task id）。
func normalizeLuoGuPID(pid string) string {
	pid = strings.TrimSpace(pid)
	if pid == "" {
		return pid
	}
	// AT_xxx：前缀 AT_ 大写，其余保持
	if len(pid) > 3 && strings.EqualFold(pid[:3], "AT_") {
		return "AT_" + pid[3:]
	}
	// 纯字母前缀 + 数字/字母：常见 P/B/T/SP/CF/UVA → 整体大写
	return strings.ToUpper(pid)
}

// atCoderTaskURL 仅 task id 时尽量还原 contest URL（abc123_a → contests/abc123/tasks/abc123_a）。
func atCoderTaskURL(taskID string) string {
	return atCoderTaskURLWithContest("", taskID)
}

func atCoderTaskURLWithContest(contest, taskID string) string {
	taskID = strings.TrimSpace(taskID)
	contest = strings.TrimSpace(contest)
	if taskID == "" {
		return ""
	}
	if contest != "" {
		return fmt.Sprintf("https://atcoder.jp/contests/%s/tasks/%s", contest, taskID)
	}
	// 从 task id 推断 contest：最后一个 _ 前为 contest（abc123_a / arc111_b / ahc001_a）
	if i := strings.LastIndex(taskID, "_"); i > 0 {
		c := taskID[:i]
		if reAtCoderTask.MatchString(c) {
			return fmt.Sprintf("https://atcoder.jp/contests/%s/tasks/%s", c, taskID)
		}
	}
	// 兜底：/tasks/{id}（站内会跳转）
	return "https://atcoder.jp/tasks/" + taskID
}

func parseNowCoder(contest, problem string) (*ParsedProblem, error) {
	// AC 站 external_id = 数字 id → https://ac.nowcoder.com/acm/problem/{id}
	// 主站 external_id = questionUuid（32 hex）→ https://www.nowcoder.com/practice/{uuid}
	// 禁止用 questionNum(ACM413) 或 main|uid 当 id
	title := strings.TrimSpace(problem)
	ext := ""
	isMainSite := false

	// 1) 主站 practice URL
	if m := reNowCoderPracticeURL.FindStringSubmatch(problem); m != nil {
		if u := normalizeNowCoderUUID(m[1]); u != "" {
			ext = u
			isMainSite = true
			title = strings.TrimSpace(reNowCoderPracticeURL.ReplaceAllString(problem, ""))
		}
	}

	// 2) AC 站 /acm/problem/123
	if ext == "" {
		if m := reNowCoderProblemURL.FindStringSubmatch(problem); m != nil {
			ext = m[1]
			title = strings.TrimSpace(reNowCoderProblemURL.ReplaceAllString(problem, ""))
		}
	}

	// 3) 字段首 token：UUID 或纯数字
	if ext == "" {
		fields := strings.Fields(problem)
		if len(fields) > 0 {
			if u := normalizeNowCoderUUID(fields[0]); u != "" {
				ext = u
				isMainSite = true
				if len(fields) > 1 {
					title = strings.Join(fields[1:], " ")
				} else {
					title = ext
				}
			} else if reDigits.MatchString(fields[0]) {
				ext = fields[0]
				if len(fields) > 1 {
					title = strings.Join(fields[1:], " ")
				} else {
					title = ext
				}
			}
		}
	}

	// 4) 整段 UUID / 纯数字
	if ext == "" {
		raw := strings.TrimSpace(problem)
		if u := normalizeNowCoderUUID(raw); u != "" {
			ext = u
			isMainSite = true
			title = ext
		} else if reDigits.MatchString(raw) {
			ext = raw
			title = ext
		}
	}

	// 5) contest 若是纯数字比赛 id 且 problem 无稳定 id：竞赛题无法去重 → 跳过
	if ext == "" {
		c := contest
		if i := strings.Index(c, "|"); i >= 0 {
			c = ""
		}
		if reDigits.MatchString(c) {
			return nil, fmt.Errorf("nowcoder contest problem without stable id")
		}
		return nil, fmt.Errorf("nowcoder missing problem id: %q", problem)
	}

	if title == "" {
		title = ext
	}
	problemURL := "https://ac.nowcoder.com/acm/problem/" + ext
	if isMainSite {
		problemURL = "https://www.nowcoder.com/practice/" + ext
	}
	return &ParsedProblem{
		Platform:   spider.NowCoder,
		ExternalID: ext,
		Title:      title,
		URL:        problemURL,
	}, nil
}

// normalizeNowCoderUUID 主站 questionUuid：32 位 hex（可带连字符），统一去掉连字符入库
func normalizeNowCoderUUID(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if !reNowCoderUUID.MatchString(s) {
		return ""
	}
	s = strings.ReplaceAll(s, "-", "")
	if len(s) != 32 {
		return ""
	}
	return s
}

func parseQOJ(problem string) (*ParsedProblem, error) {
	problem = strings.TrimSpace(problem)
	ext := sanitizeExternalID(problem)
	if m := reQOJNum.FindStringSubmatch(problem); m != nil {
		ext = m[1]
	}
	if ext == "" {
		return nil, fmt.Errorf("qoj parse fail")
	}
	title := cleanQOJSubmitTitle(problem, ext)
	return &ParsedProblem{
		Platform:   spider.QOJ,
		ExternalID: ext,
		Title:      title,
		URL:        "https://qoj.ac/problem/" + ext,
	}, nil
}

// cleanQOJSubmitTitle 从提交列表题名（如「#19004. Foo」）得到展示标题；过滤站点品牌「QOJ.ac」。
func cleanQOJSubmitTitle(problem, ext string) string {
	t := strings.Join(strings.Fields(strings.TrimSpace(problem)), " ")
	if t == "" || isQOJBrandTitleLocal(t) {
		if ext != "" {
			return "#" + ext
		}
		return t
	}
	// 「#19004. Real Title」/「# 19004 Real Title」
	re := regexp.MustCompile(`(?i)^#?\s*` + regexp.QuoteMeta(ext) + `\s*[\.．]?\s*(.*)$`)
	if m := re.FindStringSubmatch(t); m != nil {
		rest := strings.TrimSpace(m[1])
		if rest != "" && !isQOJBrandTitleLocal(rest) {
			return "#" + ext + ". " + rest
		}
		return "#" + ext
	}
	return t
}

func isQOJBrandTitleLocal(title string) bool {
	t := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(title)), " "))
	t = strings.Trim(t, ".-–—_|")
	switch t {
	case "", "qoj.ac", "qoj", "qoj ac":
		return true
	default:
		return false
	}
}

func parseLOJ(problem string) (*ParsedProblem, error) {
	title := strings.TrimSpace(problem)
	ext := ""
	if m := reLOJNum.FindStringSubmatch(problem); m != nil {
		ext = m[1]
	} else if reDigits.MatchString(strings.TrimSpace(problem)) {
		ext = strings.TrimSpace(problem)
	}
	if ext == "" {
		return nil, fmt.Errorf("loj parse fail: %q", problem)
	}
	if title == "" {
		title = ext
	}
	return &ParsedProblem{
		Platform:   spider.LOJ,
		ExternalID: ext,
		Title:      title,
		URL:        "https://loj.ac/p/" + ext,
	}, nil
}

func parseUOJ(problem string) (*ParsedProblem, error) {
	title := strings.TrimSpace(problem)
	ext := ""
	if m := reUOJNum.FindStringSubmatch(problem); m != nil {
		ext = m[1]
	} else if reDigits.MatchString(strings.TrimSpace(problem)) {
		ext = strings.TrimSpace(problem)
	}
	if ext == "" {
		return nil, fmt.Errorf("uoj parse fail: %q", problem)
	}
	if title == "" {
		title = ext
	}
	return &ParsedProblem{
		Platform:   spider.UOJ,
		ExternalID: ext,
		Title:      title,
		URL:        "https://uoj.ac/problem/" + ext,
	}, nil
}

// parsePOJ 提交列表题名多为「#1000」或纯题号；真实题名由 problem_fetch 回填。
func parsePOJ(problem string) (*ParsedProblem, error) {
	title := strings.TrimSpace(problem)
	ext := ""
	if m := rePOJNum.FindStringSubmatch(problem); m != nil {
		ext = m[1]
	} else if reDigits.MatchString(strings.TrimSpace(problem)) {
		ext = strings.TrimSpace(problem)
	}
	if ext == "" {
		return nil, fmt.Errorf("poj parse fail: %q", problem)
	}
	// 提交侧只有题号时标题用 #id 占位，避免空标题；爬题面后会换成「#id. 真名」
	if title == "" || title == ext {
		title = "#" + ext
	} else if !strings.HasPrefix(title, "#") && reDigits.MatchString(title) {
		title = "#" + title
	}
	return &ParsedProblem{
		Platform:   spider.POJ,
		ExternalID: ext,
		Title:      title,
		URL:        "http://poj.org/problem?id=" + ext,
	}, nil
}

func sanitizeExternalID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	if len(s) > 120 {
		s = s[:120]
	}
	return s
}

// parseLeetCode 解析最近通过明细：problem 形如 "{titleSlug} {中文标题}"
// 合成 AC（lc-ac-problem-* / leetcode-submit）无 slug → 跳过题库
func parseLeetCode(problem string) (*ParsedProblem, error) {
	problem = strings.TrimSpace(problem)
	if problem == "" || problem == "leetcode-submit" || strings.HasPrefix(problem, "lc-ac-problem-") {
		return nil, fmt.Errorf("leetcode synthetic skip bank: %q", problem)
	}
	// URL 中的 slug
	if i := strings.Index(problem, "leetcode.cn/problems/"); i >= 0 {
		rest := problem[i+len("leetcode.cn/problems/"):]
		slug := strings.Split(rest, "/")[0]
		slug = strings.TrimSpace(slug)
		if reLeetCodeSlug.MatchString(slug) {
			return &ParsedProblem{
				Platform:   spider.LeetCode,
				ExternalID: slug,
				Title:      slug,
				URL:        "https://leetcode.cn/problems/" + slug + "/",
			}, nil
		}
	}
	fields := strings.Fields(problem)
	if len(fields) == 0 {
		return nil, fmt.Errorf("leetcode empty problem")
	}
	slug := fields[0]
	if !reLeetCodeSlug.MatchString(slug) {
		return nil, fmt.Errorf("leetcode missing titleSlug: %q", problem)
	}
	title := slug
	if len(fields) > 1 {
		title = strings.Join(fields[1:], " ")
	}
	return &ParsedProblem{
		Platform:   spider.LeetCode,
		ExternalID: slug,
		Title:      title,
		URL:        "https://leetcode.cn/problems/" + slug + "/",
	}, nil
}
