package platform

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"cwxu-algo/app/common/utils/ojhttp"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"
)

// UOJ 提交列表对访客要求登录；本爬虫只解析公开主页「AC 过的题目」合成 AC 记录。
// submit_id 约定：uoj-ac-{userId}-{problemId}（见 model.IsUOJSyntheticAC）

const (
	uojProfileURL   = "https://uoj.ac/user/profile/"
	uojACSubmitPref = "uoj-ac-"
)

var (
	// <li><a href="https://uoj.ac/problem/1">#1. A + B Problem</a></li>
	reUOJACProblem = regexp.MustCompile(`(?i)href="(?:https?://[^"]+)?/problem/(\d+)"[^>]*>([^<]*)`)
	reUOJRating    = regexp.MustCompile(`(?is)list-group-item-heading[^>]*>\s*Rating\s*</h4>\s*<p[^>]*>\s*<strong[^>]*>\s*(\d+)\s*</strong>`)
	// 宽松兜底：Rating 标题后首个纯数字评分
	reUOJRatingLoose = regexp.MustCompile(`(?is)>\s*Rating\s*<.*?<strong[^>]*>\s*(\d{3,5})\s*</strong>`)
)

// NewUOJ Universal Online Judge（uoj.ac）AC 列表爬虫。
type NewUOJ struct{}

func (p NewUOJ) Name() string { return spider.UOJ }

func uojFetchProfileHTML(username string) (string, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return "", fmt.Errorf("uoj username 为空")
	}
	u := uojProfileURL + url.PathEscape(username)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")

	resp, err := ojhttp.Do(req)
	if err != nil {
		return "", fmt.Errorf("uoj profile: %w", err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("uoj profile status %d", resp.StatusCode)
	}
	html := string(rb)
	// 用户不存在时常见 404 页或跳转；标题含「用户信息」且有用户名更稳
	if strings.Contains(html, "登录 - Universal Online Judge") && !strings.Contains(html, username) {
		return "", fmt.Errorf("uoj 无法访问用户主页（可能需登录或用户不存在）: %s", username)
	}
	return html, nil
}

func parseUOJACProblems(html string) []struct {
	ID    string
	Title string
} {
	// 优先只扫 AC 列表区块，减少其它 problem 链接干扰
	section := html
	if i := strings.Index(html, "uoj-ac-problems-list"); i >= 0 {
		section = html[i:]
		// 截到下一 list-group-item 或 panel 结束附近
		if j := strings.Index(section[20:], `list-group-item`); j > 0 {
			// 保留本块内 ul
		}
		if k := strings.Index(section, "</ul>"); k > 0 {
			section = section[:k]
		}
	}
	seen := map[string]struct{}{}
	var out []struct {
		ID    string
		Title string
	}
	for _, m := range reUOJACProblem.FindAllStringSubmatch(section, -1) {
		id := m[1]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		title := strings.TrimSpace(htmlUnescapeBasic(m[2]))
		if title == "" {
			title = "#" + id
		}
		out = append(out, struct {
			ID    string
			Title string
		}{ID: id, Title: title})
	}
	return out
}

func htmlUnescapeBasic(s string) string {
	r := strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&#39;", "'",
		"&nbsp;", " ",
	)
	return strings.TrimSpace(r.Replace(s))
}

func parseUOJRating(html string) (int, bool) {
	if m := reUOJRating.FindStringSubmatch(html); len(m) > 1 {
		if v, err := strconv.Atoi(m[1]); err == nil && v > 0 {
			return v, true
		}
	}
	if m := reUOJRatingLoose.FindStringSubmatch(html); len(m) > 1 {
		if v, err := strconv.Atoi(m[1]); err == nil && v > 0 {
			return v, true
		}
	}
	return 0, false
}

// FetchSubmitLog 从公开主页拉 AC 题目集。
// needAll 与增量语义相同（单页全量 AC）；新 AC 靠 submit_id 去重插入。
func (p NewUOJ) FetchSubmitLog(userId int64, username string, needAll bool) ([]model.SubmitLog, error) {
	_ = needAll
	html, err := uojFetchProfileHTML(username)
	if err != nil {
		return nil, err
	}
	problems := parseUOJACProblems(html)
	if len(problems) == 0 {
		// 区分「真的 0 AC」与「解析失败」：有用户信息标题但无列表
		if !strings.Contains(html, username) && !strings.Contains(html, "用户信息") {
			return nil, fmt.Errorf("uoj 用户不存在或主页异常: %s", username)
		}
		return nil, nil
	}
	// 无真实提交时间：固定 epoch，避免首次绑定把全部历史 AC 砸进当天热力
	sentinel := time.Unix(0, 0).UTC()
	res := make([]model.SubmitLog, 0, len(problems))
	for _, pr := range problems {
		res = append(res, model.SubmitLog{
			UserID:     userId,
			Platform:   spider.UOJ,
			SubmitID:   fmt.Sprintf("%s%d-%s", uojACSubmitPref, userId, pr.ID),
			Problem:    pr.Title,
			ExternalID: pr.ID,
			Lang:       "",
			Status:     "AC",
			Time:       sentinel,
		})
	}
	return res, nil
}

// FetchRating 主页 Rating 区块。
func (p NewUOJ) FetchRating(username string) (int, bool, error) {
	html, err := uojFetchProfileHTML(username)
	if err != nil {
		return 0, false, err
	}
	r, ok := parseUOJRating(html)
	return r, ok, nil
}

func init() {
	spider.Register(&NewUOJ{})
}
