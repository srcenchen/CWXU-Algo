package problem_fetch

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// VJudgeStatementBrief is the lightweight statement record embedded in a
// VJudge problem page.
type VJudgeStatementBrief struct {
	Key           int64  `json:"key"`
	Version       int64  `json:"version"`
	Lang          string `json:"lang"`
	Type          string `json:"type"`
	Official      bool   `json:"official"`
	MainOfficial  bool   `json:"mainOfficial"`
	PublicVisible bool   `json:"publicVisible"`
}

type VJudgeSectionValue struct {
	Format  string `json:"format"`
	Content string `json:"content"`
}

type VJudgeSection struct {
	Title string             `json:"title"`
	Value VJudgeSectionValue `json:"value"`
}

type VJudgeDescription struct {
	Lang      string          `json:"lang"`
	Trustable bool            `json:"trustable"`
	Sections  []VJudgeSection `json:"sections"`
}

type VJudgeProblem struct {
	OJ           string                 `json:"oj"`
	Prob         string                 `json:"prob"`
	ProblemID    int64                  `json:"problemId"`
	HasStatement bool                   `json:"hasStatementAccess"`
	Statements   []VJudgeStatementBrief `json:"descBriefs"`
}

// FetchVJudge fetches and normalizes one VJudge statement. The returned URL
// is the exact problem URL after platform/id normalization, not a guessed
// origin URL.
func FetchVJudge(ctx context.Context, platform, externalID, username, password string) (*FetchedContent, error) {
	if strings.TrimSpace(username) == "" || strings.TrimSpace(password) == "" {
		return nil, fmt.Errorf("VJudge 账号未配置")
	}
	oj, err := vjudgeOriginOJ(platform)
	if err != nil {
		return nil, err
	}
	id, err := normalizeVJudgeProblemID(platform, externalID)
	if err != nil {
		return nil, err
	}
	// PathEscape leaves some separators encoded differently across Go releases;
	// quote the complete path segment explicitly so the Chinese OJ name stays
	// a single VJudge route segment.
	problemURL := "https://vjudge.net/problem/" + url.QueryEscape(oj+"-"+id)
	problemURL = strings.ReplaceAll(problemURL, "+", "%20")
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	loginForm := url.Values{"username": {username}, "password": {password}}
	loginReq, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://vjudge.net/user/login", strings.NewReader(loginForm.Encode()))
	if err != nil {
		return nil, err
	}
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginReq.Header.Set("User-Agent", browserUA)
	resp, err := client.Do(loginReq)
	if err != nil {
		return nil, fmt.Errorf("VJudge 登录失败: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("VJudge 登录失败: status %d", resp.StatusCode)
	}
	page, err := vjudgeGet(ctx, client, problemURL)
	if err != nil {
		return nil, err
	}
	problem, systemVersion, err := parseVJudgeProblemPage(page)
	if err != nil {
		return nil, err
	}
	if !problem.HasStatement {
		return nil, fmt.Errorf("VJudge 题面不可访问")
	}
	if err := validateVJudgeIdentity(platform, externalID, problem.OJ, problem.Prob); err != nil {
		return nil, err
	}
	brief, ok := selectVJudgeStatement(problem.Statements)
	if !ok {
		return nil, fmt.Errorf("VJudge 没有可用中文或英文题面")
	}
	descURL := fmt.Sprintf("https://vjudge.net/problem/description/%d?%d", brief.Key, brief.Version+systemVersion)
	descPage, err := vjudgeGet(ctx, client, descURL)
	if err != nil {
		return nil, err
	}
	desc, err := parseVJudgeEmbeddedDescription(descPage)
	if err != nil {
		return nil, err
	}
	content, err := vjudgeDescriptionToMarkdown(desc, "https://cdn.vjudge.net.cn/")
	if err != nil {
		return nil, err
	}
	title := ""
	if m := regexp.MustCompile(`(?s)<h2[^>]*>.*?</i>\s*(.*?)</h2>`).FindStringSubmatch(page); len(m) == 2 {
		title = strings.TrimSpace(html.UnescapeString(regexp.MustCompile(`<[^>]+>`).ReplaceAllString(m[1], "")))
	}
	return &FetchedContent{Title: title, ContentMD: content, Source: "vjudge", SourceURL: problemURL,
		SourceProblemID: fmt.Sprint(problem.ProblemID), SourceStatementID: fmt.Sprint(brief.Key), Language: desc.Lang}, nil
}

// VerifyVJudgeCredentials only checks the account session and does not fetch
// a problem statement.
func VerifyVJudgeCredentials(ctx context.Context, username, password string) error {
	if strings.TrimSpace(username) == "" || strings.TrimSpace(password) == "" {
		return fmt.Errorf("VJudge 账号未配置")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	form := url.Values{"username": {username}, "password": {password}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://vjudge.net/user/login", strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", browserUA)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("VJudge 登录失败: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("VJudge 登录失败: status %d", resp.StatusCode)
	}
	status, err := vjudgeGet(ctx, client, "https://vjudge.net/user/checkLogInStatus")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "true" {
		return fmt.Errorf("VJudge 登录状态未确认")
	}
	return nil
}

func vjudgeGet(ctx context.Context, client *http.Client, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.7")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("VJudge 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("VJudge status %d", resp.StatusCode)
	}
	return string(body), nil
}

func parseVJudgeProblemPage(page string) (VJudgeProblem, int64, error) {
	var p VJudgeProblem
	m := regexp.MustCompile(`(?s)<textarea[^>]*name="dataJson"[^>]*>(.*?)</textarea>`).FindStringSubmatch(page)
	if len(m) != 2 {
		return p, 0, fmt.Errorf("VJudge 未找到题目元数据")
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &p); err != nil {
		return p, 0, fmt.Errorf("VJudge 题目元数据解析失败: %w", err)
	}
	sm := regexp.MustCompile(`name="systemVersion" type="hidden" value="(\d+)"`).FindStringSubmatch(page)
	if len(sm) != 2 {
		return p, 0, fmt.Errorf("VJudge 缺少 systemVersion")
	}
	var version int64
	if _, err := fmt.Sscan(sm[1], &version); err != nil {
		return p, 0, err
	}
	return p, version, nil
}

func parseVJudgeEmbeddedDescription(page string) (VJudgeDescription, error) {
	m := regexp.MustCompile(`(?s)<textarea[^>]*class="data-json-container"[^>]*>(.*?)</textarea>`).FindStringSubmatch(page)
	if len(m) != 2 {
		return VJudgeDescription{}, fmt.Errorf("VJudge 未找到题面正文")
	}
	return parseVJudgeDescriptionJSON(html.UnescapeString(m[1]))
}

func vjudgeOriginOJ(platform string) (string, error) {
	switch strings.TrimSpace(platform) {
	case "LuoGu":
		return "洛谷", nil
	case "CodeForces":
		return "CodeForces", nil
	case "AtCoder":
		return "AtCoder", nil
	case "QOJ":
		return "QOJ", nil
	default:
		return "", fmt.Errorf("VJudge 不支持平台: %s", platform)
	}
}

func normalizeVJudgeProblemID(platform, externalID string) (string, error) {
	id := strings.TrimSpace(externalID)
	if id == "" {
		return "", fmt.Errorf("题号为空")
	}
	switch platform {
	case "LuoGu":
		if !regexp.MustCompile(`^[Pp]\d+$`).MatchString(id) {
			return "", fmt.Errorf("洛谷题号格式错误: %s", id)
		}
		return "P" + id[1:], nil
	case "CodeForces":
		id = strings.TrimPrefix(strings.TrimPrefix(id, "gym"), "GYM")
		if !regexp.MustCompile(`^(?:\d+[A-Za-z]\d*|\d+)$`).MatchString(id) {
			return "", fmt.Errorf("Codeforces 题号格式错误: %s", externalID)
		}
		return id, nil
	case "AtCoder":
		if !regexp.MustCompile(`^[A-Za-z0-9]+_[A-Za-z0-9]+$`).MatchString(id) {
			return "", fmt.Errorf("AtCoder 题号格式错误: %s", id)
		}
		return strings.ToLower(id), nil
	case "QOJ":
		if !regexp.MustCompile(`^\d+$`).MatchString(id) {
			return "", fmt.Errorf("QOJ 题号格式错误: %s", id)
		}
		return id, nil
	default:
		return "", fmt.Errorf("VJudge 不支持平台: %s", platform)
	}
}

func selectVJudgeStatement(briefs []VJudgeStatementBrief) (VJudgeStatementBrief, bool) {
	ordered := append([]VJudgeStatementBrief(nil), briefs...)
	score := func(b VJudgeStatementBrief) int {
		if !b.PublicVisible {
			return -1
		}
		s := 0
		if b.Lang == "zh" {
			s += 1000
		} else if b.Lang == "en" {
			s += 100
		}
		if b.MainOfficial && b.Type == "main" {
			s += 80
		}
		if b.Official {
			s += 40
		}
		if b.Type == "translator" {
			s += 20
		}
		if b.Type == "user" {
			s += 10
		}
		return s
	}
	sort.SliceStable(ordered, func(i, j int) bool { return score(ordered[i]) > score(ordered[j]) })
	for _, b := range ordered {
		if score(b) >= 0 && b.Key != 0 {
			return b, true
		}
	}
	return VJudgeStatementBrief{}, false
}

func validateVJudgeIdentity(platform, externalID, actualOJ, actualProb string) error {
	expectedOJ := map[string]string{"LuoGu": "洛谷", "CodeForces": "CodeForces", "AtCoder": "AtCoder", "QOJ": "QOJ"}[platform]
	if expectedOJ == "" || !strings.EqualFold(strings.TrimSpace(expectedOJ), strings.TrimSpace(actualOJ)) {
		return fmt.Errorf("VJudge 平台不匹配: want %s got %s", expectedOJ, actualOJ)
	}
	normalize := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.TrimPrefix(strings.ToLower(s), "gym")
		return strings.ToLower(strings.ReplaceAll(s, "-", ""))
	}
	if normalize(externalID) != normalize(actualProb) {
		return fmt.Errorf("VJudge 题号不匹配: want %s got %s", externalID, actualProb)
	}
	return nil
}

func vjudgeDescriptionToMarkdown(d VJudgeDescription, cdnBase string) (string, error) {
	var out strings.Builder
	for _, section := range d.Sections {
		content := strings.TrimSpace(section.Value.Content)
		if content == "" {
			continue
		}
		content = strings.ReplaceAll(content, "CDN_BASE_URL/", strings.TrimRight(cdnBase, "/")+"/")
		if section.Value.Format == "HTML" {
			converted, err := vjudgeHTMLToMarkdown(content)
			if err != nil {
				return "", err
			}
			content = converted
		}
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}
		if title := strings.TrimSpace(section.Title); title != "" {
			out.WriteString("## ")
			out.WriteString(title)
			out.WriteString("\n\n")
		}
		out.WriteString(content)
		out.WriteString("\n\n")
	}
	result := collapseBlankLines(strings.TrimSpace(out.String()))
	if result == "" {
		return "", fmt.Errorf("VJudge 题面为空")
	}
	return result, nil
}

func vjudgeHTMLToMarkdown(raw string) (string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div id='vjudge-root'>" + raw + "</div>"))
	if err != nil {
		return "", err
	}
	root := doc.Find("#vjudge-root")
	var out strings.Builder
	root.Find("table.vjudge_sample").Each(func(_ int, table *goquery.Selection) {
		table.Find("tr").Each(func(i int, tr *goquery.Selection) {
			cells := tr.Find("th,td")
			if i == 0 && cells.Filter("th").Length() > 0 {
				return
			}
			if cells.Length() >= 2 {
				for j := 0; j < 2; j++ {
					value := strings.TrimSpace(cells.Eq(j).Find("pre").Text())
					if value == "" {
						value = strings.TrimSpace(cells.Eq(j).Text())
					}
					label := "输出"
					if j == 0 {
						label = "输入"
					}
					out.WriteString("**" + label + "**\n\n```\n" + value + "\n```\n\n")
				}
			}
		})
		table.Remove()
	})
	root.Find("p,div,li").Each(func(_ int, s *goquery.Selection) {
		if s.Find("p,div,li").Length() > 0 {
			return
		}
		text := strings.TrimSpace(vjudgeInlineMarkdown(s))
		if text != "" {
			out.WriteString(text + "\n\n")
		}
	})
	if out.Len() == 0 {
		out.WriteString(strings.TrimSpace(vjudgeInlineMarkdown(root)))
	}
	return collapseBlankLines(strings.TrimSpace(html.UnescapeString(out.String()))), nil
}

func vjudgeInlineMarkdown(s *goquery.Selection) string {
	var out strings.Builder
	s.Contents().Each(func(_ int, n *goquery.Selection) {
		switch goquery.NodeName(n) {
		case "#text":
			out.WriteString(n.Text())
		case "br":
			out.WriteString("\n")
		case "var":
			out.WriteString("$" + strings.TrimSpace(n.Text()) + "$")
		case "code":
			out.WriteString("`" + n.Text() + "`")
		case "img":
			alt, _ := n.Attr("alt")
			src, _ := n.Attr("src")
			if alt != "" {
				out.WriteString("$" + strings.Trim(alt, "$ ") + "$")
			} else if src != "" {
				out.WriteString("![image](" + src + ")")
			}
		default:
			out.WriteString(vjudgeInlineMarkdown(n))
		}
	})
	return strings.TrimSpace(regexp.MustCompile(`[ \t]+`).ReplaceAllString(out.String(), " "))
}

func parseVJudgeDescriptionJSON(raw string) (VJudgeDescription, error) {
	var d VJudgeDescription
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return d, err
	}
	return d, nil
}
