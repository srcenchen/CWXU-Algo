package service

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"cwxu-algo/app/core_data/internal/spider"
)

var (
	reURLCFContest    = regexp.MustCompile(`(?i)/contest/(\d+)/problem/([A-Za-z0-9]+)`)
	reURLCFProblemset = regexp.MustCompile(`(?i)/problemset/problem/(\d+)/([A-Za-z0-9]+)`)
	reURLCFGym        = regexp.MustCompile(`(?i)/gym/(\d+)/problem/([A-Za-z0-9]+)`)
	// group/{hash}/contest/{id}/problem/{index}
	reURLCFGroup = regexp.MustCompile(`(?i)/group/[A-Za-z0-9]+/contest/(\d+)/problem/([A-Za-z0-9]+)`)
	reURLAtCoder = regexp.MustCompile(`(?i)/contests/([a-z0-9_]+)/tasks/([a-z0-9_]+)`)
	// 短链 /tasks/{task_id}（无 contest 段）
	reURLAtCoderTask = regexp.MustCompile(`(?i)/tasks/([a-z0-9_]+)`)
	// 洛谷：P1001 / B2001 / CF1A / AT_abc123_a / SP1 / UVA100 / T1000…
	reURLLuoGu        = regexp.MustCompile(`(?i)/problem/([A-Za-z][A-Za-z0-9_]*)`)
	reURLLeetCode     = regexp.MustCompile(`(?i)/problems/([a-z0-9]+(?:-[a-z0-9]+)*)`)
	reURLNowCoderACM     = regexp.MustCompile(`(?i)/acm/problem/(\d+)`)
	reURLNowCoderPrac    = regexp.MustCompile(`(?i)/practice/([0-9a-fA-F-]{32,36})`)
	// 比赛单题：/acm/contest/{contestId}/{A|F|1}（路径无数字 problemId，需 problem-list 解析）
	reURLNowCoderContest = regexp.MustCompile(`(?i)/acm/contest/(\d+)/([A-Za-z0-9]+)`)
	reURLQOJ             = regexp.MustCompile(`(?i)/problem/(\d+)`)
	// LibreOJ：/p/{displayId} 或 /problem/{displayId}
	reURLLOJ             = regexp.MustCompile(`(?i)/(?:p|problem)/(\d+)`)
	reURLUOJ             = regexp.MustCompile(`(?i)/problem/(\d+)`)
)

// reLooseHTTPURL 从粘贴文本中抠出第一个 http(s) 链接（允许前后有说明文字）
var reLooseHTTPURL = regexp.MustCompile(`(?i)https?://[^\s<>"'\]]+`)

// sanitizeProblemURLRaw 清洗用户粘贴：去空白/说明文字/追踪参数/尾部标点。
// 复制时常见：前后中文、?utm=、#fragment、Markdown 链接尾巴。
func sanitizeProblemURLRaw(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Markdown [text](url) → url
	if i := strings.Index(raw, "]("); i >= 0 {
		if j := strings.Index(raw[i+2:], ")"); j >= 0 {
			inner := strings.TrimSpace(raw[i+2 : i+2+j])
			if strings.Contains(inner, "://") || strings.Contains(inner, ".") {
				raw = inner
			}
		}
	}
	// 从整段文字里抠第一个 URL
	if m := reLooseHTTPURL.FindString(raw); m != "" {
		raw = m
	}
	raw = strings.TrimSpace(raw)
	// 去掉尾部常见粘连标点（中英文）
	raw = strings.TrimRight(raw, " \t\r\n。．，,;；:!！?？)'\"》>]")
	// 允许只贴 host/path
	if !strings.Contains(raw, "://") {
		raw = "https://" + strings.TrimPrefix(raw, "//")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	// 丢弃 query / fragment（追踪参数不影响 path 身份，但干扰部分平台）
	u.RawQuery = ""
	u.Fragment = ""
	// 规范化 path：去多余尾斜杠（根路径除外）
	if p := u.Path; len(p) > 1 && strings.HasSuffix(p, "/") {
		u.Path = strings.TrimRight(p, "/")
	}
	// Host 小写
	u.Host = strings.ToLower(u.Host)
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	return u.String()
}

// ParseProblemURL 从常见 OJ 链接解析题目身份；无法识别时返回 error（调用方忽略即可）
func ParseProblemURL(raw string) (*ParsedProblem, error) {
	raw = sanitizeProblemURLRaw(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	host := strings.ToLower(u.Host)
	// 去掉 www.
	host = strings.TrimPrefix(host, "www.")
	path := u.Path
	full := host + path

	// Codeforces（含 gym / group / problemset）
	if strings.Contains(host, "codeforces.com") {
		if m := reURLCFContest.FindStringSubmatch(path); m != nil {
			return &ParsedProblem{
				Platform:   spider.CodeForces,
				ExternalID: m[1] + m[2],
				Title:      m[1] + m[2],
				URL:        fmt.Sprintf("https://codeforces.com/contest/%s/problem/%s", m[1], m[2]),
			}, nil
		}
		if m := reURLCFProblemset.FindStringSubmatch(path); m != nil {
			return &ParsedProblem{
				Platform:   spider.CodeForces,
				ExternalID: m[1] + m[2],
				Title:      m[1] + m[2],
				URL:        fmt.Sprintf("https://codeforces.com/problemset/problem/%s/%s", m[1], m[2]),
			}, nil
		}
		if m := reURLCFGym.FindStringSubmatch(path); m != nil {
			return &ParsedProblem{
				Platform:   spider.CodeForces,
				ExternalID: "gym" + m[1] + m[2],
				Title:      m[1] + m[2],
				URL:        fmt.Sprintf("https://codeforces.com/gym/%s/problem/%s", m[1], m[2]),
			}, nil
		}
		if m := reURLCFGroup.FindStringSubmatch(path); m != nil {
			// group 赛与正式 contest 共用 contestId+index 作为 external_id
			return &ParsedProblem{
				Platform:   spider.CodeForces,
				ExternalID: m[1] + m[2],
				Title:      m[1] + m[2],
				URL:        fmt.Sprintf("https://codeforces.com/contest/%s/problem/%s", m[1], m[2]),
			}, nil
		}
		return nil, fmt.Errorf("unrecognized codeforces url")
	}

	// AtCoder
	if strings.Contains(host, "atcoder.jp") {
		if m := reURLAtCoder.FindStringSubmatch(path); m != nil {
			return &ParsedProblem{
				Platform:   spider.AtCoder,
				ExternalID: m[2],
				Title:      m[2],
				URL:        fmt.Sprintf("https://atcoder.jp/contests/%s/tasks/%s", m[1], m[2]),
			}, nil
		}
		if m := reURLAtCoderTask.FindStringSubmatch(path); m != nil {
			task := m[1]
			return &ParsedProblem{
				Platform:   spider.AtCoder,
				ExternalID: task,
				Title:      task,
				URL:        atCoderTaskURL(task),
			}, nil
		}
		return nil, fmt.Errorf("unrecognized atcoder url")
	}

	// 洛谷
	if strings.Contains(host, "luogu.com") {
		if m := reURLLuoGu.FindStringSubmatch(path); m != nil {
			pid := m[1]
			// 规范大小写：前缀字母大写，AT_ 后段保留
			pid = normalizeLuoGuPID(pid)
			return &ParsedProblem{
				Platform:   spider.LuoGu,
				ExternalID: pid,
				Title:      pid,
				URL:        "https://www.luogu.com.cn/problem/" + pid,
			}, nil
		}
		return nil, fmt.Errorf("unrecognized luogu url")
	}

	// 力扣
	if strings.Contains(host, "leetcode.cn") || strings.Contains(host, "leetcode.com") {
		if m := reURLLeetCode.FindStringSubmatch(path); m != nil {
			slug := m[1]
			base := "https://leetcode.cn/problems/"
			if strings.Contains(host, "leetcode.com") && !strings.Contains(host, "leetcode.cn") {
				base = "https://leetcode.com/problems/"
			}
			return &ParsedProblem{
				Platform:   spider.LeetCode,
				ExternalID: slug,
				Title:      slug,
				URL:        base + slug + "/",
			}, nil
		}
		return nil, fmt.Errorf("unrecognized leetcode url")
	}

	// 牛客
	if strings.Contains(host, "nowcoder.com") {
		if m := reURLNowCoderACM.FindStringSubmatch(path); m != nil {
			return &ParsedProblem{
				Platform:   spider.NowCoder,
				ExternalID: m[1],
				Title:      m[1],
				URL:        "https://ac.nowcoder.com/acm/problem/" + m[1],
			}, nil
		}
		// 比赛题页：https://ac.nowcoder.com/acm/contest/137561/F
		// problemId 不在路径里 → 走 problem-list（与比赛 ensure 同源）
		if m := reURLNowCoderContest.FindStringSubmatch(path); m != nil {
			return resolveNowCoderContestProblemURL(m[1], m[2])
		}
		if m := reURLNowCoderPrac.FindStringSubmatch(path); m != nil {
			uuid := strings.ToLower(strings.ReplaceAll(m[1], "-", ""))
			return &ParsedProblem{
				Platform:   spider.NowCoder,
				ExternalID: uuid,
				Title:      uuid,
				URL:        "https://www.nowcoder.com/practice/" + uuid,
			}, nil
		}
		return nil, fmt.Errorf("unrecognized nowcoder url")
	}

	// QOJ
	if strings.Contains(host, "qoj.ac") || strings.HasPrefix(host, "qoj.") {
		if m := reURLQOJ.FindStringSubmatch(path); m != nil {
			return &ParsedProblem{
				Platform:   spider.QOJ,
				ExternalID: m[1],
				Title:      m[1],
				URL:        "https://qoj.ac/problem/" + m[1],
			}, nil
		}
		return nil, fmt.Errorf("unrecognized qoj url")
	}

	// LibreOJ
	if strings.Contains(host, "loj.ac") || strings.HasPrefix(host, "loj.") {
		if m := reURLLOJ.FindStringSubmatch(path); m != nil {
			return &ParsedProblem{
				Platform:   spider.LOJ,
				ExternalID: m[1],
				Title:      m[1],
				URL:        "https://loj.ac/p/" + m[1],
			}, nil
		}
		return nil, fmt.Errorf("unrecognized loj url")
	}

	// UOJ
	if strings.Contains(host, "uoj.ac") || strings.HasPrefix(host, "uoj.") {
		if m := reURLUOJ.FindStringSubmatch(path); m != nil {
			return &ParsedProblem{
				Platform:   spider.UOJ,
				ExternalID: m[1],
				Title:      m[1],
				URL:        "https://uoj.ac/problem/" + m[1],
			}, nil
		}
		return nil, fmt.Errorf("unrecognized uoj url")
	}

	_ = full
	return nil, fmt.Errorf("unsupported problem url host=%s", host)
}

// resolveNowCoderContestProblemURL 比赛题页 → 数字 problemId（与 ensure 同源 problem-list）。
// 例：contest=137561 index=F → external_id=319875，bank URL=/acm/problem/319875，
// FallbackURLs 含比赛页供题库无权限时回退抓取。
func resolveNowCoderContestProblemURL(contestID, index string) (*ParsedProblem, error) {
	contestID = strings.TrimSpace(contestID)
	index = strings.TrimSpace(index)
	if contestID == "" || index == "" {
		return nil, fmt.Errorf("empty nowcoder contest id or index")
	}
	specs, err := listNowCoderContestProblems(contestID)
	if err != nil {
		return nil, fmt.Errorf("nowcoder contest problem-list: %w", err)
	}
	want := strings.ToUpper(index)
	var hit *ContestProblemSpec
	for i := range specs {
		sp := &specs[i]
		if strings.EqualFold(strings.TrimSpace(sp.Label), index) ||
			strings.ToUpper(strings.TrimSpace(sp.Label)) == want {
			hit = sp
			break
		}
	}
	if hit == nil {
		return nil, fmt.Errorf("nowcoder contest %s has no problem index %s", contestID, index)
	}
	pid := strings.TrimSpace(hit.ExternalID)
	if pid == "" || pid == "0" {
		return nil, fmt.Errorf("nowcoder contest %s/%s: empty problemId", contestID, index)
	}
	label := firstNonEmpty(strings.TrimSpace(hit.Label), index)
	contestURL := fmt.Sprintf("https://ac.nowcoder.com/acm/contest/%s/%s", contestID, label)
	bankURL := "https://ac.nowcoder.com/acm/problem/" + pid
	title := firstNonEmpty(strings.TrimSpace(hit.Title), label)
	return &ParsedProblem{
		Platform:     spider.NowCoder,
		ExternalID:   pid,
		Title:        title,
		URL:          bankURL,
		FallbackURLs: []string{contestURL},
		ContestID:    contestID,
		ContestLabel: label,
	}, nil
}
