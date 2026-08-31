package problem_fetch

import (
	"cwxu-algo/app/common/utils/ojhttp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/PuerkitoBio/goquery"
)

// FetchedContent 爬取结果
type FetchedContent struct {
	Title     string
	ContentMD string
	// Source records which upstream supplied the content. It is intentionally
	// optional so existing platform fetchers remain source-compatible.
	Source            string
	SourceURL         string
	SourceProblemID   string
	SourceStatementID string
	Language          string
}

// Fetch 按平台爬取题面 Markdown。
// 已支持：CodeForces / AtCoder / LuoGu / NowCoder / QOJ / LeetCode / UOJ / POJ；
// LOJ 为 SPA，走 generic 或占位（提交 identity 已可入库）。
func Fetch(platform, externalID, problemURL string) (*FetchedContent, error) {
	return FetchWithFallbacks(platform, externalID, problemURL, nil)
}

// FetchWithFallbacks 同 Fetch；fallbackURLs 供 NowCoder 在题库页暂无权限时改走比赛页
// （/acm/contest/{contestId}/{A}），成功后仍按 external_id=problemId 入库。
func FetchWithFallbacks(platform, externalID, problemURL string, fallbackURLs []string) (*FetchedContent, error) {
	switch platform {
	case "CodeForces":
		return fetchCodeforces(externalID, problemURL)
	case "AtCoder":
		return fetchAtCoderWithID(problemURL, externalID)
	case "LuoGu":
		return fetchLuoGu(externalID, problemURL)
	case "QOJ":
		return fetchQOJ(externalID, problemURL)
	case "UOJ":
		return fetchUOJ(externalID, problemURL)
	case "NowCoder":
		return fetchNowCoder(externalID, problemURL, fallbackURLs...)
	case "LeetCode":
		return fetchLeetCode(externalID, problemURL)
	case "POJ":
		return fetchPOJ(externalID, problemURL)
	case "LOJ":
		// LibreOJ 前端 SPA；公开页无稳定服务端 HTML 题面，generic 往往只有壳
		if problemURL == "" && externalID != "" {
			problemURL = "https://loj.ac/p/" + externalID
		}
		if problemURL != "" {
			return fetchGeneric(problemURL)
		}
		return nil, fmt.Errorf("loj empty url")
	default:
		if problemURL != "" {
			return fetchGeneric(problemURL)
		}
		return nil, fmt.Errorf("不支持的平台: %s", platform)
	}
}

// 浏览器指纹池：UA 与 Sec-CH-UA 版本必须一致，轮换降低特征
type browserProfile struct {
	UA       string
	SecCHUA  string
	Platform string // "Windows" | "macOS"
}

var browserProfiles = []browserProfile{
	{
		UA:       `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36`,
		SecCHUA:  `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
		Platform: `"Windows"`,
	},
	{
		UA:       `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36`,
		SecCHUA:  `"Chromium";v="130", "Google Chrome";v="130", "Not?A_Brand";v="99"`,
		Platform: `"Windows"`,
	},
	{
		UA:       `Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36`,
		SecCHUA:  `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`,
		Platform: `"macOS"`,
	},
	{
		UA:       `Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Safari/605.1.15`,
		SecCHUA:  "", // Safari 不发 Sec-CH-UA
		Platform: "",
	},
}

var (
	browserProfileMu  sync.Mutex
	browserProfileIdx int
)

func nextBrowserProfile() browserProfile {
	browserProfileMu.Lock()
	defer browserProfileMu.Unlock()
	p := browserProfiles[browserProfileIdx%len(browserProfiles)]
	browserProfileIdx++
	return p
}

const browserUA = `Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36`

func setBrowserHeaders(req *http.Request, referer string) {
	setBrowserHeadersWithProfile(req, referer, browserProfiles[0])
}

func setBrowserHeadersWithProfile(req *http.Request, referer string, p browserProfile) {
	req.Header.Set("User-Agent", p.UA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en-US;q=0.8,en;q=0.7")
	// 不要手动设 Accept-Encoding：由 Transport 自动协商并解压，避免指纹/解压不一致
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Connection", "keep-alive")
	if p.SecCHUA != "" {
		req.Header.Set("Sec-Ch-Ua", p.SecCHUA)
		req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
		if p.Platform != "" {
			req.Header.Set("Sec-Ch-Ua-Platform", p.Platform)
		}
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-User", "?1")
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
		if p.SecCHUA != "" {
			req.Header.Set("Sec-Fetch-Site", "same-origin")
		}
	} else if p.SecCHUA != "" {
		req.Header.Set("Sec-Fetch-Site", "none")
	}
}

func httpGet(rawURL string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	setBrowserHeaders(req, "")
	client := &http.Client{Timeout: 30 * time.Second}
	return ojhttp.DoWithClient(client, req)
}

// 牛客 WAF：无 Cookie 会返回阿里云滑块页；先访问对应站点首页拿 Cookie 再抓题面
// AC 站 ac.nowcoder.com；主站 www.nowcoder.com（/practice/{uuid}）
// 双站分 Client + Cookie，主站串行限速，避免并发/串 Cookie 触发 405
var (
	nowcoderACOnce     sync.Once
	nowcoderMainOnce   sync.Once
	nowcoderACClient   *http.Client
	nowcoderMainClient *http.Client
	nowcoderMainMu     sync.Mutex
	nowcoderMainLast   time.Time
)

func newNowCoderHTTPClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	// 使用默认 Transport（含 HTTP/2）：强关 HTTP/2 反而会和 ALPN 冲突导致异常响应
	tr := http.DefaultTransport.(*http.Transport).Clone()
	return &http.Client{
		Timeout:   45 * time.Second,
		Jar:       jar,
		Transport: tr,
		// 禁止把 GET 跟成非 GET（部分网关 302 后会 405）
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("too many redirects")
			}
			if req.Method != http.MethodGet && req.Method != http.MethodHead {
				req.Method = http.MethodGet
				req.Body = nil
				req.GetBody = nil
				req.ContentLength = 0
			}
			setBrowserHeaders(req, via[len(via)-1].URL.String())
			return nil
		},
	}
}

func getNowCoderACClient() *http.Client {
	nowcoderACOnce.Do(func() {
		nowcoderACClient = newNowCoderHTTPClient()
	})
	return nowcoderACClient
}

func getNowCoderMainClient() *http.Client {
	nowcoderMainOnce.Do(func() {
		nowcoderMainClient = newNowCoderHTTPClient()
	})
	return nowcoderMainClient
}

func nowcoderSiteOrigin(rawURL string) string {
	if strings.Contains(rawURL, "www.nowcoder.com") {
		return "https://www.nowcoder.com"
	}
	return "https://ac.nowcoder.com"
}

func nowcoderClientFor(rawURL string) *http.Client {
	if isMainNowCoderURL(rawURL) {
		return getNowCoderMainClient()
	}
	return getNowCoderACClient()
}

// nowcoderSession 绑定 Client + 当前浏览器指纹（一轮请求内保持一致）
type nowcoderSession struct {
	client  *http.Client
	profile browserProfile
	origin  string
}

func nowcoderClearCookies(client *http.Client, origin string) {
	if client == nil || client.Jar == nil {
		return
	}
	if origin == "" {
		origin = "https://ac.nowcoder.com"
	}
	if u, e := url.Parse(origin + "/"); e == nil {
		client.Jar.SetCookies(u, nil)
	}
}

func (s *nowcoderSession) get(rawURL, referer string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	setBrowserHeadersWithProfile(req, referer, s.profile)
	return ojhttp.DoWithClient(s.client, req)
}

// warmup 模拟真人进站：首页 →（主站）题库列表，拿 Cookie
func (s *nowcoderSession) warmup() {
	resp, err := s.get(s.origin+"/", "")
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !strings.Contains(s.origin, "www.nowcoder.com") {
		return
	}
	// 主站再点一层列表页，Referer 链更像浏览器
	time.Sleep(200*time.Millisecond + time.Duration(time.Now().UnixNano()%300)*time.Millisecond)
	resp2, err := s.get(s.origin+"/practice", s.origin+"/")
	if err != nil {
		return
	}
	io.Copy(io.Discard, resp2.Body)
	resp2.Body.Close()
}

func nowcoderGet(rawURL string) (*http.Response, error) {
	client := nowcoderClientFor(rawURL)
	origin := nowcoderSiteOrigin(rawURL)
	sess := &nowcoderSession{client: client, profile: nextBrowserProfile(), origin: origin}

	// 主站：串行 + 1.2~2.0s 抖动间隔，降低 405
	if isMainNowCoderURL(rawURL) {
		nowcoderMainMu.Lock()
		defer nowcoderMainMu.Unlock()
		minGap := 1200 * time.Millisecond
		jitter := time.Duration(time.Now().UnixNano()%800) * time.Millisecond
		if d := time.Since(nowcoderMainLast); d < minGap+jitter {
			time.Sleep(minGap + jitter - d)
		}
		nowcoderMainLast = time.Now()
	}

	// 无 Cookie 时先预热
	if u, err := url.Parse(origin + "/"); err == nil {
		if len(client.Jar.Cookies(u)) == 0 {
			sess.warmup()
		}
	}
	// 主站 Referer 用 /practice 列表，更像从题库点进题
	referer := origin + "/"
	if isMainNowCoderURL(rawURL) {
		referer = origin + "/practice"
	}
	return sess.get(rawURL, referer)
}

func isNowCoderUUID(s string) bool {
	s = strings.TrimSpace(strings.ToLower(strings.ReplaceAll(s, "-", "")))
	if len(s) != 32 {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func isNowCoderWAF(html string) bool {
	return strings.Contains(html, "aliyun_waf") ||
		strings.Contains(html, "aliyunCaptcha") ||
		strings.Contains(html, "访问验证") ||
		strings.Contains(html, "请进行验证")
}

// isNowCoderNoPermission 比赛/私有题：未登录或无权时返回的壳页（无 subject-question）
// 例 title:「牛客网-没有查看题目的权限哦」——链接可能正确，只是暂时不可匿名读题面
func isNowCoderNoPermission(html string) bool {
	if html == "" {
		return false
	}
	return strings.Contains(html, "没有查看题目的权限") ||
		strings.Contains(html, "没有权限查看该题目") ||
		strings.Contains(html, "无权查看该题目") ||
		(strings.Contains(html, "暂无权限") && strings.Contains(html, "题目"))
}

func fetchCodeforces(externalID, problemURL string) (*FetchedContent, error) {
	if problemURL == "" {
		contest, index, isGym := splitCF(externalID)
		if contest == "" || index == "" {
			return nil, fmt.Errorf("无法解析 CF external_id: %s", externalID)
		}
		if isGym {
			problemURL = fmt.Sprintf("https://codeforces.com/gym/%s/problem/%s", contest, index)
		} else {
			// problemset 路径有时比 contest 路径更稳定
			problemURL = fmt.Sprintf("https://codeforces.com/problemset/problem/%s/%s", contest, index)
		}
	}
	resp, err := httpGet(problemURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("CF status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	html := string(body)
	// Cloudflare 拦截
	if strings.Contains(html, "Just a moment") || strings.Contains(html, "cf-browser-verification") {
		return nil, fmt.Errorf("CF 被 Cloudflare 拦截，请稍后重试或换网络")
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}
	stmt := doc.Find("div.problem-statement").First()
	if stmt.Length() == 0 {
		return nil, fmt.Errorf("CF 未找到题面")
	}

	title := strings.TrimSpace(stmt.Find("div.header div.title").First().Text())
	if title == "" {
		title = strings.TrimSpace(doc.Find("div.title").First().Text())
	}
	if i := strings.Index(title, ". "); i >= 0 && i < 6 {
		title = strings.TrimSpace(title[i+2:])
	}

	// 按语义块转 Markdown，避免整段 Text 粘连
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(title)
	b.WriteString("\n\n")

	// 时间/内存限制
	if tl := strings.TrimSpace(stmt.Find("div.time-limit").Text()); tl != "" {
		b.WriteString("**")
		b.WriteString(normalizeSpace(tl))
		b.WriteString("**\n\n")
	}
	if ml := strings.TrimSpace(stmt.Find("div.memory-limit").Text()); ml != "" {
		b.WriteString("**")
		b.WriteString(normalizeSpace(ml))
		b.WriteString("**\n\n")
	}

	// 去掉 header（含 title/limits），保留正文结构
	clone := stmt.Clone()
	clone.Find("div.header").Remove()
	clone.Children().Each(func(_ int, s *goquery.Selection) {
		class, _ := s.Attr("class")
		class = strings.TrimSpace(class)
		switch {
		case class == "input-specification":
			b.WriteString("## 输入\n\n")
			b.WriteString(selectionToMD(s))
			b.WriteString("\n\n")
		case class == "output-specification":
			b.WriteString("## 输出\n\n")
			b.WriteString(selectionToMD(s))
			b.WriteString("\n\n")
		case class == "sample-tests":
			b.WriteString("## 样例\n\n")
			s.Find("div.sample-test").Each(func(i int, sample *goquery.Selection) {
				b.WriteString(fmt.Sprintf("### 样例 %d\n\n", i+1))
				in := sample.Find("div.input pre").First()
				out := sample.Find("div.output pre").First()
				if in.Length() > 0 {
					b.WriteString("**输入**\n\n```\n")
					b.WriteString(strings.TrimRight(in.Text(), "\n"))
					b.WriteString("\n```\n\n")
				}
				if out.Length() > 0 {
					b.WriteString("**输出**\n\n```\n")
					b.WriteString(strings.TrimRight(out.Text(), "\n"))
					b.WriteString("\n```\n\n")
				}
			})
		case class == "note":
			b.WriteString("## 说明\n\n")
			b.WriteString(selectionToMD(s))
			b.WriteString("\n\n")
		default:
			// 主描述段落
			txt := selectionToMD(s)
			if strings.TrimSpace(txt) != "" {
				b.WriteString(txt)
				b.WriteString("\n\n")
			}
		}
	})

	md := strings.TrimSpace(b.String())
	if md == "" || len(md) < 10 {
		// fallback
		md = "# " + title + "\n\n" + selectionToMD(clone)
	}
	// 清理 CF 公式 $$$...$$$ → $...$
	md = strings.ReplaceAll(md, "$$$", "$")
	md = collapseBlankLines(md)
	return &FetchedContent{Title: title, ContentMD: md}, nil
}

func selectionToMD(s *goquery.Selection) string {
	// 处理常见子节点（CF / AtCoder 等 HTML 题面共用）
	var b strings.Builder
	s.Contents().Each(func(_ int, n *goquery.Selection) {
		if goquery.NodeName(n) == "#text" {
			b.WriteString(n.Text())
			return
		}
		name := goquery.NodeName(n)
		switch name {
		case "p":
			b.WriteString(normalizeSpace(inlineToMD(n)))
			b.WriteString("\n\n")
		case "h1", "h2", "h3", "h4", "h5", "h6":
			// AtCoder 用 <h3>Problem Statement</h3><p>... 紧挨，必须换行
			level := int(name[1] - '0')
			if level < 1 {
				level = 3
			}
			b.WriteString("\n")
			b.WriteString(strings.Repeat("#", level))
			b.WriteString(" ")
			b.WriteString(normalizeSpace(inlineToMD(n)))
			b.WriteString("\n\n")
		case "ul", "ol":
			n.Find("li").Each(func(_ int, li *goquery.Selection) {
				b.WriteString("- ")
				b.WriteString(normalizeSpace(inlineToMD(li)))
				b.WriteString("\n")
			})
			b.WriteString("\n")
		case "pre":
			b.WriteString("\n```\n")
			b.WriteString(strings.TrimRight(n.Text(), "\n"))
			b.WriteString("\n```\n\n")
		case "section", "article":
			b.WriteString(selectionToMD(n))
			b.WriteString("\n")
		case "div":
			// section-title 等
			if n.HasClass("section-title") {
				b.WriteString("### ")
				b.WriteString(normalizeSpace(inlineToMD(n)))
				b.WriteString("\n\n")
			} else {
				b.WriteString(selectionToMD(n))
			}
		case "var":
			// AtCoder 公式节点
			b.WriteString("$")
			b.WriteString(strings.TrimSpace(n.Text()))
			b.WriteString("$")
		case "code":
			b.WriteString("`")
			b.WriteString(n.Text())
			b.WriteString("`")
		case "span":
			// tex math often in span
			if alt, ok := n.Attr("class"); ok && strings.Contains(alt, "tex") {
				b.WriteString("$")
				b.WriteString(normalizeSpace(n.Text()))
				b.WriteString("$")
			} else {
				b.WriteString(inlineToMD(n))
			}
		case "br":
			b.WriteString("\n")
		case "hr":
			b.WriteString("\n\n---\n\n")
		default:
			b.WriteString(inlineToMD(n))
		}
	})
	return strings.TrimSpace(b.String())
}

// inlineToMD 行内节点：保留 var/code/tex，避免整段 Text() 把公式标签剥光又粘连标题。
func inlineToMD(s *goquery.Selection) string {
	var b strings.Builder
	s.Contents().Each(func(_ int, n *goquery.Selection) {
		name := goquery.NodeName(n)
		switch name {
		case "#text":
			b.WriteString(n.Text())
		case "var":
			b.WriteString("$")
			b.WriteString(strings.TrimSpace(n.Text()))
			b.WriteString("$")
		case "code":
			b.WriteString("`")
			b.WriteString(n.Text())
			b.WriteString("`")
		case "span":
			if cls, ok := n.Attr("class"); ok && strings.Contains(cls, "tex") {
				b.WriteString("$")
				b.WriteString(normalizeSpace(n.Text()))
				b.WriteString("$")
			} else {
				b.WriteString(inlineToMD(n))
			}
		case "br":
			b.WriteString("\n")
		case "a":
			// Editorial 等页内链接：标题区已 clean，正文忽略纯链接噪音
			t := strings.TrimSpace(n.Text())
			if t != "" && !strings.EqualFold(t, "Editorial") && t != "解説" {
				b.WriteString(t)
			}
		default:
			b.WriteString(inlineToMD(n))
		}
	})
	return b.String()
}

// splitCF 解析 external_id：
//   - 正式赛 "1791A" → contest=1791 index=A isGym=false
//   - Gym "gym102861A" / "gym102861A1" → contest=102861 index=A isGym=true
//   - 负号兼容 "-102861A"（旧数据）→ gym
func splitCF(externalID string) (contest, index string, isGym bool) {
	id := strings.TrimSpace(externalID)
	if id == "" {
		return "", "", false
	}
	lower := strings.ToLower(id)
	if strings.HasPrefix(lower, "gym") {
		isGym = true
		id = id[3:]
	} else if strings.HasPrefix(id, "-") {
		isGym = true
		id = strings.TrimPrefix(id, "-")
	}
	for i := 0; i < len(id); i++ {
		if (id[i] >= 'A' && id[i] <= 'Z') || (id[i] >= 'a' && id[i] <= 'z') {
			return id[:i], id[i:], isGym
		}
	}
	return "", "", isGym
}

// atCoderURLFromExternalID 无完整 URL 时由 task id 还原题面地址。
func atCoderURLFromExternalID(externalID string) string {
	task := strings.TrimSpace(externalID)
	if task == "" {
		return ""
	}
	if i := strings.LastIndex(task, "_"); i > 0 {
		contest := task[:i]
		if contest != "" {
			return fmt.Sprintf("https://atcoder.jp/contests/%s/tasks/%s", contest, task)
		}
	}
	return "https://atcoder.jp/tasks/" + task
}

func fetchAtCoder(problemURL string) (*FetchedContent, error) {
	return fetchAtCoderWithID(problemURL, "")
}

func fetchAtCoderWithID(problemURL, externalID string) (*FetchedContent, error) {
	if problemURL == "" {
		problemURL = atCoderURLFromExternalID(externalID)
	}
	if problemURL == "" {
		return nil, fmt.Errorf("AtCoder 缺少 URL 与 external_id")
	}
	resp, err := httpGet(problemURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("AtCoder status %d", resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}
	title := cleanProblemTitle(doc.Find("span.h2").First().Text())
	if title == "" {
		title = cleanProblemTitle(doc.Find("h2").First().Text())
	}
	// AtCoder 页头 h2 常夹带 "Editorial" 与换行，再清一次
	title = cleanProblemTitle(title)
	// 优先英文块；AtCoder 是 span.lang-en 嵌套，Each 只取最外层避免重复
	var mdParts []string
	if en := doc.Find("#task-statement span.lang-en").First(); en.Length() > 0 {
		if t := selectionToMD(en); t != "" {
			mdParts = append(mdParts, t)
		}
	}
	if len(mdParts) == 0 {
		if ja := doc.Find("#task-statement span.lang-ja").First(); ja.Length() > 0 {
			if t := selectionToMD(ja); t != "" {
				mdParts = append(mdParts, t)
			}
		}
	}
	if len(mdParts) == 0 {
		t := selectionToMD(doc.Find("#task-statement"))
		if t != "" {
			mdParts = append(mdParts, t)
		}
	}
	if len(mdParts) == 0 {
		return nil, fmt.Errorf("AtCoder 未找到题面")
	}
	body := collapseBlankLines(strings.Join(mdParts, "\n\n"))
	// 去掉正文里误带的 Editorial 行
	body = stripEditorialNoise(body)
	md := body
	if title != "" && !strings.HasPrefix(strings.TrimSpace(body), "# ") {
		md = "# " + title + "\n\n" + body
	}
	return &FetchedContent{Title: title, ContentMD: md}, nil
}

func stripEditorialNoise(md string) string {
	var lines []string
	for _, line := range strings.Split(md, "\n") {
		t := strings.TrimSpace(line)
		if t == "" {
			lines = append(lines, line)
			continue
		}
		if strings.EqualFold(t, "Editorial") || t == "解説" {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func fetchLuoGu(externalID, problemURL string) (*FetchedContent, error) {
	if problemURL == "" {
		problemURL = "https://www.luogu.com.cn/problem/" + externalID
	}
	apiURL := problemURL
	if !strings.Contains(apiURL, "_contentOnly") {
		if strings.Contains(apiURL, "?") {
			apiURL += "&_contentOnly=1"
		} else {
			apiURL += "?_contentOnly=1"
		}
	}
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := ojhttp.DoWithClient(client, req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("洛谷 status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "json") || strings.HasPrefix(strings.TrimSpace(string(body)), "{") {
		return parseLuoGuJSON(body)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	article := doc.Find("#app article").First()
	if article.Length() == 0 {
		article = doc.Find("article").First()
	}
	if article.Length() == 0 {
		return nil, fmt.Errorf("洛谷未找到题面")
	}
	title := strings.TrimSpace(article.Find("h1").First().Text())
	article.Find("script,style,noscript").Remove()
	content := selectionToMD(article)
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("洛谷题面内容为空")
	}
	return &FetchedContent{Title: title, ContentMD: cleanLuoGuStatement(content)}, nil
}

// cleanLuoGuStatement removes the browser-only shell that can be returned by
// older Luogu HTML responses. Never persist noscript warnings or toolbar text
// as problem content, even if an upstream fallback bypasses article parsing.
func cleanLuoGuStatement(content string) string {
	content = regexp.MustCompile(`(?is)<(?:script|style|noscript)\b[^>]*>.*?</(?:script|style|noscript)>`).ReplaceAllString(content, "")
	content = regexp.MustCompile(`(?is)<h[1-6][^>]*>\s*请\s*<b[^>]*>不要禁用</b>脚本，否则网页无法正常加载\s*</h[1-6]>`).ReplaceAllString(content, "")
	content = regexp.MustCompile(`(?im)^\s*#{1,6}\s*请\s*不要禁用脚本，否则网页无法正常加载\s*$`).ReplaceAllString(content, "")
	lines := make([]string, 0)
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if t == "plain text" || t == "复制" || t == "请不要禁用脚本，否则网页无法正常加载" {
			continue
		}
		lines = append(lines, line)
	}
	return collapseBlankLines(strings.TrimSpace(strings.Join(lines, "\n")))
}

func parseLuoGuJSON(body []byte) (*FetchedContent, error) {
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		// fallback 旧逻辑
		s := string(body)
		return &FetchedContent{
			Title:     extractJSONString(s, `"title"`),
			ContentMD: firstNonEmpty(extractJSONString(s, `"description"`), extractJSONString(s, `"content"`), truncate(s, 8000)),
		}, nil
	}
	// 常见结构 currentData.problem
	title, desc := "", ""
	if cd, ok := root["currentData"].(map[string]interface{}); ok {
		if p, ok := cd["problem"].(map[string]interface{}); ok {
			if t, ok := p["title"].(string); ok {
				title = t
			}
			if d, ok := p["description"].(string); ok {
				desc = d
			}
			if desc == "" {
				if d, ok := p["content"].(string); ok {
					desc = d
				}
			}
		}
	}
	if title == "" {
		title = extractJSONString(string(body), `"title"`)
	}
	if desc == "" {
		desc = firstNonEmpty(extractJSONString(string(body), `"description"`), extractJSONString(string(body), `"content"`))
	}
	if desc == "" {
		return nil, fmt.Errorf("洛谷 JSON 无题面")
	}
	return &FetchedContent{Title: title, ContentMD: cleanLuoGuStatement(desc)}, nil
}

// NowCoderContestProblemURL 比赛内题面：https://ac.nowcoder.com/acm/contest/{contestId}/{A}
// 刚结束比赛时题库 /acm/problem/{id} 常「没有查看题目的权限」，比赛页仍可读；
// pageInfo.problemId 与题库 id 一致，入库 external_id 仍用数字 problemId。
func NowCoderContestProblemURL(contestID, index string) string {
	contestID = strings.TrimSpace(contestID)
	index = strings.TrimSpace(index)
	if contestID == "" || index == "" {
		return ""
	}
	return "https://ac.nowcoder.com/acm/contest/" + contestID + "/" + index
}

// NowCoderBankProblemURL 题库规范链接（长期可访问形态）
func NowCoderBankProblemURL(problemID string) string {
	problemID = strings.TrimSpace(problemID)
	if problemID == "" || !isAllDigits(problemID) {
		return ""
	}
	return "https://ac.nowcoder.com/acm/problem/" + problemID
}

var reNowCoderContestPath = regexp.MustCompile(`(?i)/acm/contest/(\d+)/([A-Za-z0-9]+)`)

// IsNowCoderContestURL 是否比赛内题面路径 /acm/contest/{id}/{A}
func IsNowCoderContestURL(raw string) bool {
	return reNowCoderContestPath.MatchString(raw)
}

func isNowCoderContestURL(raw string) bool {
	return IsNowCoderContestURL(raw)
}

func fetchNowCoder(externalID, problemURL string, fallbackURLs ...string) (*FetchedContent, error) {
	id := strings.TrimSpace(externalID)
	// 规范化 id / 主站 UUID
	if isNowCoderUUID(id) || isNowCoderUUID(extractUUIDFromURL(problemURL)) {
		uuid := id
		if !isNowCoderUUID(uuid) {
			uuid = extractUUIDFromURL(problemURL)
		}
		uuid = strings.ToLower(strings.ReplaceAll(uuid, "-", ""))
		return fetchNowCoderOne("https://www.nowcoder.com/practice/"+uuid, uuid)
	}
	if id != "" && !isAllDigits(id) {
		if m := regexp.MustCompile(`^\d+`).FindString(id); m != "" {
			id = m
		}
	}

	// 候选 URL：有比赛页时优先比赛路径（赛后题库常无权限），再题库规范链接
	var candidates []string
	seen := map[string]struct{}{}
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" {
			return
		}
		if _, ok := seen[u]; ok {
			return
		}
		seen[u] = struct{}{}
		candidates = append(candidates, u)
	}

	// 1) 所有比赛页候选（primary + fallbacks）
	if isNowCoderContestURL(problemURL) {
		add(problemURL)
	}
	for _, u := range fallbackURLs {
		if isNowCoderContestURL(u) {
			add(u)
		}
	}
	// 2) 题库页
	if bank := NowCoderBankProblemURL(id); bank != "" {
		add(bank)
	}
	if problemURL != "" && !isNowCoderContestURL(problemURL) {
		add(problemURL)
	}
	// 3) 其余 fallback（非比赛）
	for _, u := range fallbackURLs {
		if !isNowCoderContestURL(u) {
			add(u)
		}
	}
	if len(candidates) == 0 {
		if id == "" {
			return nil, fmt.Errorf("NowCoder 缺少题面 URL 与 external_id")
		}
		return nil, fmt.Errorf("NowCoder 无稳定题面 URL，跳过爬取")
	}

	var lastErr error
	for _, u := range candidates {
		fc, err := fetchNowCoderOne(u, id)
		if err == nil {
			return fc, nil
		}
		lastErr = err
		// 仅权限/DOM/空题面等可回退；WAF/网络错误也试下一个候选
		if !nowcoderShouldTryNextURL(err) {
			return nil, err
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("NowCoder 未找到题面 DOM，请稍后重试")
}

func nowcoderShouldTryNextURL(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "暂无访问权限") ||
		strings.Contains(msg, "未找到题面") ||
		strings.Contains(msg, "题面为空") ||
		strings.Contains(msg, "需要登录") ||
		strings.Contains(msg, "WAF") ||
		strings.Contains(msg, "被拦截") ||
		strings.Contains(msg, "请稍后重试") ||
		strings.Contains(msg, "status ")
}

func fetchNowCoderOne(problemURL, expectProblemID string) (*FetchedContent, error) {
	html, err := nowcoderFetchHTML(problemURL)
	if err != nil {
		return nil, err
	}
	// 比赛页 window.pageInfo.problemId 与题库 id 对齐时校验（防错题）
	if isNowCoderContestURL(problemURL) && expectProblemID != "" && isAllDigits(expectProblemID) {
		if pid := extractNowCoderPageInfoProblemID(html); pid != "" && pid != expectProblemID {
			return nil, fmt.Errorf("NowCoder 比赛页 problemId=%s 与期望 %s 不一致，请稍后重试", pid, expectProblemID)
		}
	}
	if isMainNowCoderURL(problemURL) {
		return parseNowCoderMainHTML(html)
	}
	return parseNowCoderACHHTML(html)
}

// extractNowCoderPageInfoProblemID 从比赛题面页 window.pageInfo.problemId 提取
func extractNowCoderPageInfoProblemID(html string) string {
	// problemId: '319811' 或 problemId: "319811"
	m := regexp.MustCompile(`(?i)problemId\s*:\s*['"](\d+)['"]`).FindStringSubmatch(html)
	if m == nil {
		return ""
	}
	return m[1]
}

func extractUUIDFromURL(raw string) string {
	m := regexp.MustCompile(`(?i)/practice/([0-9a-fA-F-]{32,36})`).FindStringSubmatch(raw)
	if m == nil {
		return ""
	}
	return m[1]
}

func isMainNowCoderURL(raw string) bool {
	return strings.Contains(raw, "www.nowcoder.com") || isNowCoderUUID(extractUUIDFromURL(raw))
}

// nowcoderFetchHTML GET 题面页；405/WAF 换指纹 + 清 Cookie + 退避，最多 3 次
func nowcoderFetchHTML(problemURL string) (string, error) {
	origin := nowcoderSiteOrigin(problemURL)
	client := nowcoderClientFor(problemURL)
	do := func() (int, string, error) {
		resp, err := nowcoderGet(problemURL)
		if err != nil {
			return 0, "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return resp.StatusCode, "", err
		}
		return resp.StatusCode, string(body), nil
	}

	var code int
	var html string
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			nowcoderClearCookies(client, origin)
			// 换 UA 指纹：下次 nowcoderGet 会 nextBrowserProfile
			// 退避 2s / 4s
			time.Sleep(time.Duration(attempt*2) * time.Second)
		}
		code, html, err = do()
		if err != nil {
			if attempt == 2 {
				return "", err
			}
			continue
		}
		if code == 200 && !isNowCoderWAF(html) {
			break
		}
	}
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", fmt.Errorf("NowCoder status %d，请稍后重试", code)
	}
	if isNowCoderWAF(html) {
		return "", fmt.Errorf("NowCoder 被 WAF 拦截，请稍后重试")
	}
	if isNowCoderNoPermission(html) {
		// 题库页常无匿名权限；立刻 FAILED_PERM，后补 contest 映射后走比赛页再爬
		return "", fmt.Errorf("NowCoder 题面暂无访问权限")
	}
	if strings.Contains(html, "请先登录") &&
		!strings.Contains(html, "subject-question") &&
		!strings.Contains(html, "输入描述") &&
		!strings.Contains(html, "question-oi") {
		return "", fmt.Errorf("NowCoder 需要登录，请稍后重试")
	}
	return html, nil
}

func cleanNowCoderTitle(title string) string {
	title = cleanProblemTitle(title)
	// 比赛页 title：A-小红的字符串处理_牛客周赛 Round 153
	for _, sep := range []string{"_牛客", "_NowCoder", "_nowcoder"} {
		if i := strings.Index(title, sep); i > 0 {
			title = strings.TrimSpace(title[:i])
		}
	}
	// 去掉题号前缀 A- / G1-
	title = regexp.MustCompile(`(?i)^[A-Z]\d*\s*[-–—]\s*`).ReplaceAllString(title, "")
	return strings.TrimSpace(title)
}

// cleanProblemTitle 去掉 AtCoder 等页头夹带的 Editorial / 换行 / 多余空白
func cleanProblemTitle(title string) string {
	title = strings.ReplaceAll(title, "\r", "\n")
	// 取第一行有效内容
	for _, line := range strings.Split(title, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// 单独的 Editorial 链接文案
		if strings.EqualFold(line, "Editorial") || strings.EqualFold(line, "解説") {
			continue
		}
		// 行尾粘着 Editorial
		for _, suf := range []string{"Editorial", "解説"} {
			if i := strings.LastIndex(line, suf); i > 0 {
				line = strings.TrimSpace(line[:i])
			}
		}
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			return line
		}
	}
	return strings.Join(strings.Fields(title), " ")
}

// parseNowCoderMainHTML 主站 /practice/{uuid}：题面在 body 顶部 SEO 隐藏区
func parseNowCoderMainHTML(html string) (*FetchedContent, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}
	title := cleanNowCoderTitle(doc.Find("title").First().Text())
	if title == "" {
		title = cleanNowCoderTitle(doc.Find("h1, .pop-title, .question-title").First().Text())
	}

	// SEO 隐藏区：position absolute 离屏 div，含描述 + 输入输出 + question-oi 样例
	var root *goquery.Selection
	doc.Find("body > div").Each(func(_ int, s *goquery.Selection) {
		if root != nil {
			return
		}
		style, _ := s.Attr("style")
		if strings.Contains(style, "-1000000") || strings.Contains(s.Text(), "输入描述") {
			root = s
		}
	})
	if root == nil {
		// 兜底：整页有 输入描述 / question-oi 也算
		if strings.Contains(html, "输入描述") || doc.Find(".question-oi").Length() > 0 {
			root = doc.Find("body")
		}
	}
	if root == nil {
		if isNowCoderNoPermission(html) {
			return nil, fmt.Errorf("NowCoder 题面暂无访问权限")
		}
		if isNowCoderWAF(html) {
			return nil, fmt.Errorf("NowCoder 被 WAF 拦截，请稍后重试")
		}
		// 可能是 SPA 壳 / 短暂空页：走退避重试，勿立刻永久失败
		return nil, fmt.Errorf("NowCoder 主站未找到题面 DOM，请稍后重试")
	}

	// 公式 img → $alt$
	replaceNowCoderMath(root)
	root.Find("br").Each(func(_ int, br *goquery.Selection) {
		br.ReplaceWithHtml("\n")
	})

	var b strings.Builder
	if title != "" {
		b.WriteString("# ")
		b.WriteString(title)
		b.WriteString("\n\n")
	}

	// 描述：隐藏区开头纯文本（到第一个 h5 输入描述之前）
	// 用 HTML 切分更稳
	raw, _ := root.Html()
	desc := raw
	if i := strings.Index(desc, "输入描述"); i > 0 {
		// 去掉 h5 之前的标签噪声：取 text of a temp selection
		pre := desc[:i]
		// 粗提文本
		preDoc, _ := goquery.NewDocumentFromReader(strings.NewReader("<div>" + pre + "</div>"))
		if preDoc != nil {
			t := normalizeNowCoderText(preDoc.Text())
			if t != "" {
				b.WriteString(t)
				b.WriteString("\n\n")
			}
		}
	} else {
		// 无输入描述时整块 text，去掉样例区再写
		t := normalizeNowCoderText(root.Clone().Find(".question-oi").Remove().End().Text())
		if t != "" {
			b.WriteString(t)
			b.WriteString("\n\n")
		}
	}

	// 输入 / 输出描述：仅 h5「输入描述/输出描述」，勿把样例「输入」「输出」当成描述
	appendNowCoderIODescFromHeadings(&b, root.Find("h5"))
	// 样例：.question-oi
	appendNowCoderSamples(&b, root)

	md := collapseBlankLines(strings.TrimSpace(b.String()))
	md = strings.ReplaceAll(md, "$$$", "$")
	if len(md) < 8 {
		return nil, fmt.Errorf("NowCoder 主站题面为空，请稍后重试")
	}
	return &FetchedContent{Title: title, ContentMD: md}, nil
}

// parseNowCoderACHHTML AC 站 /acm/problem/{id} 与 /acm/contest/{cid}/{A}
// 规范 URL：https://ac.nowcoder.com/acm/problem/{数字 external_id}
//
// DOM 结构（赛题）：
//
//	div.subject-question  → 题目描述（公式为 equation?tex= img）
//	h2 输入描述: + pre
//	h2 输出描述: + pre
//	div.question-oi       → 示例（h2 输入/输出 + a.复制 + pre）
//
// 旧逻辑用 Contains("输入") 匹配到样例 h2，且 next 文本是「复制」，会生成空的重复输入/输出描述。
func parseNowCoderACHHTML(html string) (*FetchedContent, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(doc.Find(".question-title h1, .question-title, .terminal-topic-title, .title, h1").First().Text())
	if title == "" {
		title = strings.TrimSpace(doc.Find("title").First().Text())
	}
	title = cleanNowCoderTitle(title)

	// 主内容：多 selector 兼容页面改版
	q := doc.Find("div.subject-question").First()
	if q.Length() == 0 {
		q = doc.Find(".question-content, .problem-content, .topic-sentence, #questionContent, .nc-post-content").First()
	}
	if q.Length() == 0 {
		if isNowCoderNoPermission(html) {
			return nil, fmt.Errorf("NowCoder 题面暂无访问权限")
		}
		if isNowCoderWAF(html) {
			return nil, fmt.Errorf("NowCoder 被 WAF 拦截，请稍后重试")
		}
		// 无 DOM 多数是权限壳/WAF 漏检/短暂空页，勿当永久黑名单
		return nil, fmt.Errorf("NowCoder 未找到题面 DOM，请稍后重试")
	}

	replaceNowCoderMath(q)
	q.Find("br").Each(func(_ int, br *goquery.Selection) {
		br.ReplaceWithHtml("\n")
	})

	var b strings.Builder
	if title != "" {
		b.WriteString("# ")
		b.WriteString(title)
		b.WriteString("\n\n")
	}
	bodyText := normalizeNowCoderText(q.Text())
	b.WriteString(bodyText)
	b.WriteString("\n\n")

	// 仅「输入描述 / 输出描述」标题；样例区的「输入」「输出」交给 question-oi
	appendNowCoderIODescFromHeadings(&b, doc.Find("h2, h3"))
	appendNowCoderSamples(&b, doc.Find("body"))

	md := collapseBlankLines(strings.TrimSpace(b.String()))
	md = strings.ReplaceAll(md, "$$$", "$")
	if len(md) < 8 {
		return nil, fmt.Errorf("NowCoder 题面为空，请稍后重试")
	}
	return &FetchedContent{Title: title, ContentMD: md}, nil
}

// nowcoderHeadingKind 归一化标题：输入描述 / 输出描述 / 其它。
// 样例区 h2 仅为「输入」「输出」，不得当成描述（否则会吃到旁边的「复制」按钮文案）。
func nowcoderHeadingKind(ht string) string {
	ht = strings.TrimSpace(ht)
	ht = strings.TrimRight(ht, ":： \t")
	ht = strings.Join(strings.Fields(ht), " ")
	switch {
	case ht == "输入描述" || strings.EqualFold(ht, "Input Description") || strings.EqualFold(ht, "InputDescription"):
		return "input_desc"
	case ht == "输出描述" || strings.EqualFold(ht, "Output Description") || strings.EqualFold(ht, "OutputDescription"):
		return "output_desc"
	default:
		return ""
	}
}

func isNowCoderCopyOnlyText(s string) bool {
	s = strings.TrimSpace(s)
	return s == "" || s == "复制" || strings.EqualFold(s, "Copy") || strings.EqualFold(s, "Copy code")
}

// nowcoderSectionBodyAfter 取标题后紧跟的 pre / 文本块，并替换公式 img；忽略「复制」按钮。
func nowcoderSectionBodyAfter(h *goquery.Selection) string {
	var target *goquery.Selection
	n := h.Next()
	for n.Length() > 0 {
		name := goquery.NodeName(n)
		if name == "h2" || name == "h3" || name == "h5" {
			break
		}
		if name == "div" {
			if n.HasClass("question-oi") || n.HasClass("subject-question") {
				break
			}
		}
		// 跳过复制按钮 / 隐藏 textarea
		if name == "a" || name == "button" || name == "textarea" || name == "script" || name == "style" {
			n = n.Next()
			continue
		}
		if name == "pre" {
			target = n
			break
		}
		// 包装层里找 pre
		if pre := n.Find("pre").First(); pre.Length() > 0 {
			target = pre
			break
		}
		// 纯文本块
		if t := strings.TrimSpace(n.Text()); t != "" && !isNowCoderCopyOnlyText(t) {
			target = n
			break
		}
		n = n.Next()
	}
	if target == nil || target.Length() == 0 {
		return ""
	}
	replaceNowCoderMath(target)
	target.Find("br").Each(func(_ int, br *goquery.Selection) {
		br.ReplaceWithHtml("\n")
	})
	// 去掉复制按钮残留
	target.Find("a.code-copy-btn, .js-clipboard, .code-copy-btn").Remove()
	text := normalizeNowCoderText(target.Text())
	if isNowCoderCopyOnlyText(text) {
		return ""
	}
	return text
}

func appendNowCoderIODescFromHeadings(b *strings.Builder, headings *goquery.Selection) {
	headings.Each(func(_ int, h *goquery.Selection) {
		kind := nowcoderHeadingKind(h.Text())
		if kind == "" {
			return
		}
		body := nowcoderSectionBodyAfter(h)
		if body == "" {
			return
		}
		switch kind {
		case "input_desc":
			b.WriteString("## 输入描述\n\n")
		case "output_desc":
			b.WriteString("## 输出描述\n\n")
		default:
			return
		}
		b.WriteString(body)
		b.WriteString("\n\n")
	})
}

func appendNowCoderSamples(b *strings.Builder, root *goquery.Selection) {
	root.Find(".question-oi").Each(func(_ int, oi *goquery.Selection) {
		hd := strings.TrimSpace(oi.Find(".question-oi-hd").First().Text())
		if hd == "" {
			hd = "示例"
		}
		b.WriteString("## ")
		b.WriteString(hd)
		b.WriteString("\n\n")
		oi.Find(".question-oi-mod").Each(func(_ int, mod *goquery.Selection) {
			// 去掉复制按钮，避免文案污染
			mod.Find("a.code-copy-btn, .js-clipboard, .code-copy-btn, textarea[data-clipboard-text-id]").Remove()
			h2 := strings.TrimSpace(mod.Find("h2, h3, h5").First().Text())
			cont := mod.Find(".question-oi-cont").First()
			if cont.Length() == 0 {
				cont = mod
			}
			replaceNowCoderMath(cont)
			pre := cont.Find("pre").First()
			body := ""
			if pre.Length() > 0 {
				body = strings.TrimRight(pre.Text(), "\n")
			} else {
				body = normalizeNowCoderText(cont.Text())
			}
			if isNowCoderCopyOnlyText(body) {
				return
			}
			if h2 != "" && !isNowCoderCopyOnlyText(h2) {
				b.WriteString("### ")
				b.WriteString(h2)
				b.WriteString("\n\n")
			}
			if body != "" {
				if pre.Length() > 0 {
					b.WriteString("```\n")
					b.WriteString(body)
					b.WriteString("\n```\n\n")
				} else {
					b.WriteString(body)
					b.WriteString("\n\n")
				}
			}
		})
	})
}

func replaceNowCoderMath(q *goquery.Selection) {
	q.Find("img").Each(func(_ int, img *goquery.Selection) {
		alt, _ := img.Attr("alt")
		src, _ := img.Attr("src")
		tex := alt
		if tex == "" && strings.Contains(src, "equation?tex=") {
			if u, err := url.Parse(src); err == nil {
				tex, _ = url.QueryUnescape(u.Query().Get("tex"))
			}
		}
		if tex == "" && strings.Contains(src, "tex=") {
			if u, err := url.Parse(src); err == nil {
				tex, _ = url.QueryUnescape(u.Query().Get("tex"))
			}
		}
		tex = strings.TrimSpace(tex)
		if tex == "" || tex == `\hspace{15pt}` || (strings.HasPrefix(tex, `\hspace`) && !strings.Contains(tex, "bullet")) {
			img.ReplaceWithHtml("")
			return
		}
		if strings.Contains(tex, `bullet`) {
			img.ReplaceWithHtml("\n- ")
			return
		}
		// 避免公式内未转义的 $ 破坏定界
		tex = strings.ReplaceAll(tex, "$", "")
		img.ReplaceWithHtml("$" + tex + "$")
	})
}

func fetchUOJ(externalID, problemURL string) (*FetchedContent, error) {
	if problemURL == "" && externalID != "" {
		problemURL = "https://uoj.ac/problem/" + externalID
	}
	if problemURL == "" {
		return nil, fmt.Errorf("empty url")
	}
	resp, err := httpGet(problemURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("UOJ 无权限访问题面(403)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("UOJ status %d", resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}
	// 登录墙
	if t := strings.TrimSpace(doc.Find("title").First().Text()); strings.Contains(t, "登录") {
		return nil, fmt.Errorf("UOJ 题面需登录")
	}
	title := strings.TrimSpace(doc.Find("h1").First().Text())
	if title == "" {
		title = strings.TrimSpace(doc.Find(".page-header").First().Text())
	}
	contentSel := doc.Find(".problem-content, #problem-content, .uoj-content article, article").First()
	if contentSel.Length() == 0 {
		contentSel = doc.Find(".uoj-content").First()
	}
	md := strings.TrimSpace(contentSel.Text())
	if title == "" && md == "" {
		return nil, fmt.Errorf("UOJ 题面解析为空")
	}
	if title == "" {
		title = externalID
	}
	return &FetchedContent{Title: title, ContentMD: md}, nil
}

func fetchQOJ(externalID, problemURL string) (*FetchedContent, error) {
	if problemURL == "" && externalID != "" {
		problemURL = "https://qoj.ac/problem/" + externalID
	}
	if problemURL == "" {
		return nil, fmt.Errorf("empty url")
	}
	resp, err := httpGet(problemURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	// QOJ 403 = 无权限（比赛题/私有题），不可恢复，直接永久失效
	if resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("QOJ 无权限访问题面(403)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("QOJ status %d", resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}
	title := extractQOJTitle(doc, externalID)
	// 题面优先取 UOJ 系正文区，避免导航/页脚噪声
	contentSel := doc.Find(".problem-content, #problem-content, .uoj-content article, article").First()
	if contentSel.Length() == 0 {
		contentSel = doc.Find(".uoj-content").First()
	}
	var text string
	if contentSel.Length() > 0 {
		contentSel.Find("script,style").Remove()
		text = collapseBlankLines(strings.TrimSpace(contentSel.Text()))
	}
	if text == "" {
		doc.Find("script,style,nav,footer,header").Remove()
		text = collapseBlankLines(strings.TrimSpace(doc.Find("body").Text()))
	}
	if text == "" {
		return nil, fmt.Errorf("empty page")
	}
	if title == "" {
		title = externalID
	}
	return &FetchedContent{Title: title, ContentMD: truncate(text, 15000)}, nil
}

// qojBrandTitles 站点品牌/导航 h1，不能当作题目标题。
// QOJ 页结构：首个 h1 常为品牌「QOJ.ac」，真正题名在 h1.page-header（如「# 19004. Foo」）。
var qojBrandTitleSet = map[string]struct{}{
	"qoj.ac": {},
	"qoj":    {},
	"qoj ac": {},
}

// IsQOJBrandTitle 是否站点品牌名（误当标题的脏数据）
func IsQOJBrandTitle(title string) bool {
	t := strings.ToLower(normalizeSpace(title))
	t = strings.Trim(t, ".-–—_|")
	if t == "" {
		return true
	}
	_, ok := qojBrandTitleSet[t]
	return ok
}

var (
	reQOJTitlePage = regexp.MustCompile(`(?i)^#?\s*(\d+)\s*[\.．]?\s*(.+)$`)
	reQOJHTMLTitle = regexp.MustCompile(`(?i)^(.+?)\s*[-–—|]\s*Problem\b`)
)

// extractQOJTitle 从 QOJ 题面 HTML 提取真实标题（跳过品牌 h1）。
func extractQOJTitle(doc *goquery.Document, externalID string) string {
	if doc == nil {
		return ""
	}
	// 1) 题头：h1.page-header / .page-header
	for _, sel := range []string{"h1.page-header", ".page-header", "h1.page-header.text-center"} {
		if t := normalizeSpace(doc.Find(sel).First().Text()); t != "" && !IsQOJBrandTitle(t) {
			return normalizeQOJTitle(t, externalID)
		}
	}
	// 2) 任一非品牌 h1：优先匹配「#N. 题名」
	var fallback string
	doc.Find("h1").EachWithBreak(func(_ int, s *goquery.Selection) bool {
		t := normalizeSpace(s.Text())
		if t == "" || IsQOJBrandTitle(t) {
			return true
		}
		if reQOJTitlePage.MatchString(t) || strings.Contains(t, "#") {
			fallback = t
			return false
		}
		if fallback == "" {
			fallback = t
		}
		return true
	})
	if fallback != "" {
		return normalizeQOJTitle(fallback, externalID)
	}
	// 3) <title>：「I/O Test - Problem - QOJ.ac」
	raw := normalizeSpace(doc.Find("title").First().Text())
	if raw != "" {
		if m := reQOJHTMLTitle.FindStringSubmatch(raw); m != nil {
			raw = strings.TrimSpace(m[1])
		} else {
			for _, sep := range []string{" - QOJ.ac", " – QOJ.ac", " | QOJ.ac", " - QOJ", " | QOJ"} {
				if i := strings.LastIndex(strings.ToLower(raw), strings.ToLower(sep)); i > 0 {
					raw = strings.TrimSpace(raw[:i])
					break
				}
			}
		}
		if raw != "" && !IsQOJBrandTitle(raw) && !strings.EqualFold(raw, "Just a moment...") {
			return normalizeQOJTitle(raw, externalID)
		}
	}
	if externalID != "" {
		return "#" + externalID
	}
	return ""
}

// normalizeQOJTitle 折叠空白；保留「#N. 名」形态。
func normalizeQOJTitle(raw, externalID string) string {
	t := normalizeSpace(raw)
	if IsQOJBrandTitle(t) {
		if externalID != "" {
			return "#" + externalID
		}
		return ""
	}
	if m := reQOJTitlePage.FindStringSubmatch(t); m != nil {
		num, name := m[1], strings.TrimSpace(m[2])
		if name != "" && !IsQOJBrandTitle(name) {
			return "#" + num + ". " + name
		}
		return "#" + num
	}
	// 仅有题名、有 externalID 时补题号前缀，便于辨认
	if externalID != "" && !strings.Contains(t, externalID) && !strings.HasPrefix(t, "#") {
		return "#" + externalID + ". " + t
	}
	return t
}

// fetchLeetCode 通过 GraphQL 拉中文题面；付费题无公开 content → 永久错误
func fetchLeetCode(externalID, problemURL string) (*FetchedContent, error) {
	slug := strings.TrimSpace(externalID)
	if slug == "" && problemURL != "" {
		// https://leetcode.cn/problems/{slug}/
		if i := strings.Index(problemURL, "/problems/"); i >= 0 {
			rest := problemURL[i+len("/problems/"):]
			slug = strings.Split(strings.Trim(rest, "/"), "/")[0]
		}
	}
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return nil, fmt.Errorf("LeetCode 缺少 titleSlug")
	}

	payload := map[string]interface{}{
		"query": `query($titleSlug: String!) {
			question(titleSlug: $titleSlug) {
				questionFrontendId
				title
				titleSlug
				translatedTitle
				difficulty
				content
				translatedContent
				isPaidOnly
			}
		}`,
		"variables": map[string]string{"titleSlug": slug},
	}
	b, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, "https://leetcode.cn/graphql/", strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Referer", "https://leetcode.cn/problems/"+slug+"/")
	req.Header.Set("Origin", "https://leetcode.cn")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := ojhttp.DoWithClient(client, req)
	if err != nil {
		return nil, fmt.Errorf("leetcode question 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("leetcode question 读 body 失败: %w", err)
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("leetcode question status %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	var root struct {
		Data struct {
			Question *struct {
				QuestionFrontendID string `json:"questionFrontendId"`
				Title              string `json:"title"`
				TitleSlug          string `json:"titleSlug"`
				TranslatedTitle    string `json:"translatedTitle"`
				Difficulty         string `json:"difficulty"`
				Content            string `json:"content"`
				TranslatedContent  string `json:"translatedContent"`
				IsPaidOnly         bool   `json:"isPaidOnly"`
			} `json:"question"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("leetcode question 解析失败: %w", err)
	}
	if len(root.Errors) > 0 {
		return nil, fmt.Errorf("leetcode question graphql: %s", root.Errors[0].Message)
	}
	q := root.Data.Question
	if q == nil {
		return nil, fmt.Errorf("leetcode 题目不存在: %s", slug)
	}
	if q.IsPaidOnly {
		return nil, fmt.Errorf("力扣付费题/无公开题面")
	}
	html := strings.TrimSpace(q.TranslatedContent)
	if html == "" {
		html = strings.TrimSpace(q.Content)
	}
	if html == "" {
		return nil, fmt.Errorf("力扣付费题/无公开题面")
	}

	title := strings.TrimSpace(q.TranslatedTitle)
	if title == "" {
		title = strings.TrimSpace(q.Title)
	}
	if title == "" {
		title = slug
	}
	if q.QuestionFrontendID != "" {
		title = q.QuestionFrontendID + ". " + title
	}

	md := leetcodeHTMLToMD(title, html)
	if len(strings.TrimSpace(md)) < 8 {
		return nil, fmt.Errorf("leetcode 题面为空")
	}
	return &FetchedContent{Title: title, ContentMD: md}, nil
}

// leetcodeHTMLToMD 将力扣题面 HTML 转为简易 Markdown
func leetcodeHTMLToMD(title, html string) string {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader("<div id='lc-root'>" + html + "</div>"))
	if err != nil {
		// fallback：粗剥标签
		plain := regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(html, "")
		plain = collapseBlankLines(strings.TrimSpace(plain))
		return "# " + title + "\n\n" + plain
	}
	root := doc.Find("#lc-root")
	// 公式 img → $alt$
	root.Find("img").Each(func(_ int, img *goquery.Selection) {
		alt, _ := img.Attr("alt")
		alt = strings.TrimSpace(alt)
		if alt != "" {
			img.ReplaceWithHtml("$" + strings.ReplaceAll(alt, "$", "") + "$")
		} else {
			img.ReplaceWithHtml("")
		}
	})
	root.Find("br").Each(func(_ int, br *goquery.Selection) {
		br.ReplaceWithHtml("\n")
	})
	root.Find("pre").Each(func(_ int, pre *goquery.Selection) {
		code := strings.TrimRight(pre.Text(), "\n")
		pre.ReplaceWithHtml("\n```\n" + code + "\n```\n")
	})
	root.Find("code").Each(func(_ int, code *goquery.Selection) {
		// 跳过已在 pre 内被替换的
		if code.ParentsFiltered("pre").Length() > 0 {
			return
		}
		t := strings.TrimSpace(code.Text())
		if t != "" {
			code.ReplaceWithHtml("`" + t + "`")
		}
	})
	root.Find("strong,b").Each(func(_ int, s *goquery.Selection) {
		t := strings.TrimSpace(s.Text())
		if t != "" {
			s.ReplaceWithHtml("**" + t + "**")
		}
	})
	root.Find("em,i").Each(func(_ int, s *goquery.Selection) {
		t := strings.TrimSpace(s.Text())
		if t != "" {
			s.ReplaceWithHtml("*" + t + "*")
		}
	})
	root.Find("ul").Each(func(_ int, ul *goquery.Selection) {
		var b strings.Builder
		ul.Find("li").Each(func(_ int, li *goquery.Selection) {
			b.WriteString("- ")
			b.WriteString(normalizeSpace(li.Text()))
			b.WriteString("\n")
		})
		ul.ReplaceWithHtml("\n" + b.String() + "\n")
	})
	root.Find("ol").Each(func(_ int, ol *goquery.Selection) {
		var b strings.Builder
		i := 1
		ol.Find("li").Each(func(_ int, li *goquery.Selection) {
			b.WriteString(fmt.Sprintf("%d. ", i))
			b.WriteString(normalizeSpace(li.Text()))
			b.WriteString("\n")
			i++
		})
		ol.ReplaceWithHtml("\n" + b.String() + "\n")
	})
	root.Find("p").Each(func(_ int, p *goquery.Selection) {
		t := strings.TrimSpace(p.Text())
		p.ReplaceWithHtml(t + "\n\n")
	})

	body := collapseBlankLines(strings.TrimSpace(root.Text()))
	// 清理 HTML 实体常见残留
	body = strings.ReplaceAll(body, "\u00a0", " ")
	var b strings.Builder
	b.WriteString("# ")
	b.WriteString(title)
	b.WriteString("\n\n")
	b.WriteString(body)
	return collapseBlankLines(b.String())
}

// POJ 题面页结构（经典表格式 HTML，无需登录）：
//
//	<div class="ptt">A+B Problem</div>
//	<div class="plm">Time Limit / Memory Limit …</div>
//	<p class="pst">Description</p><div class="ptx">…</div>
//	<p class="pst">Sample Input</p><pre class="sio">…</pre>
//
// 标题优先 .ptt；<title>「1000 -- A+B Problem」作兜底。
const pojBaseURL = "http://poj.org"

var (
	rePOJHTMLTitle = regexp.MustCompile(`(?i)^\s*(\d+)\s*[-–—]+\s*(.+?)\s*$`)
	rePOJTitleNum  = regexp.MustCompile(`(?i)^#?\s*(\d+)\s*[\.．]?\s*(.*)$`)
)

// pojSectionHeading 将 POJ 英文小节名映射为中文 Markdown 标题（与 CF 等站内风格一致）。
func pojSectionHeading(raw string) string {
	s := normalizeSpace(raw)
	switch strings.ToLower(s) {
	case "description":
		return "题目描述"
	case "input":
		return "输入"
	case "output":
		return "输出"
	case "sample input":
		return "样例输入"
	case "sample output":
		return "样例输出"
	case "hint":
		return "提示"
	case "source":
		return "来源"
	default:
		if s == "" {
			return ""
		}
		return s
	}
}

// normalizePOJTitle 规范题目标题为「#题号. 题名」。
func normalizePOJTitle(raw, externalID string) string {
	t := normalizeSpace(raw)
	if t == "" || isPOJBadTitle(t) {
		if externalID != "" {
			return "#" + externalID
		}
		return ""
	}
	// 「1000 -- A+B Problem」来自 <title>
	if m := rePOJHTMLTitle.FindStringSubmatch(t); m != nil {
		num, name := m[1], strings.TrimSpace(m[2])
		if name != "" && !isPOJBadTitle(name) {
			return "#" + num + ". " + name
		}
		return "#" + num
	}
	// 已是「#1000. Name」或「1000. Name」
	if m := rePOJTitleNum.FindStringSubmatch(t); m != nil {
		num, rest := m[1], strings.TrimSpace(m[2])
		rest = strings.TrimLeft(rest, ".-–—． ")
		if rest != "" && !isPOJBadTitle(rest) {
			return "#" + num + ". " + rest
		}
		// 纯数字标题：若有更好 externalID 用它；否则只留题号
		if externalID != "" && externalID != num {
			return "#" + externalID
		}
		return "#" + num
	}
	if externalID != "" {
		return "#" + externalID + ". " + t
	}
	return t
}

func isPOJBadTitle(title string) bool {
	t := strings.ToLower(normalizeSpace(title))
	t = strings.Trim(t, ".-–—_|")
	switch t {
	case "", "error", "problem", "problem set", "poj", "online judge",
		"problem status list", "just a moment...":
		return true
	default:
		return false
	}
}

// absolutizePOJURL 把相对资源（images/xxx.jpg）补成 http://poj.org/...
func absolutizePOJURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") || strings.HasPrefix(lower, "data:") {
		return raw
	}
	if strings.HasPrefix(raw, "//") {
		return "http:" + raw
	}
	if strings.HasPrefix(raw, "/") {
		return pojBaseURL + raw
	}
	return pojBaseURL + "/" + raw
}

// pojInlineToMD 把 .ptx / pre.sio 内 HTML 转成 Markdown（br/pre/img/sup/公式风格标签）。
func pojInlineToMD(s *goquery.Selection) string {
	if s == nil || s.Length() == 0 {
		return ""
	}
	var b strings.Builder
	s.Contents().Each(func(_ int, n *goquery.Selection) {
		name := goquery.NodeName(n)
		switch name {
		case "#text":
			b.WriteString(n.Text())
		case "br":
			b.WriteString("\n")
		case "pre":
			code := strings.TrimRight(n.Text(), "\n")
			b.WriteString("\n```\n")
			b.WriteString(code)
			b.WriteString("\n```\n")
		case "img":
			src, _ := n.Attr("src")
			alt, _ := n.Attr("alt")
			src = absolutizePOJURL(src)
			alt = normalizeSpace(alt)
			if src == "" {
				return
			}
			// 导航/页脚图过滤（正常题面区很少出现 logo）
			low := strings.ToLower(src)
			if strings.Contains(low, "/logo") || strings.Contains(low, "home.gif") ||
				strings.Contains(low, "goback") || strings.HasSuffix(low, "/top.gif") {
				return
			}
			if alt == "" {
				alt = "image"
			}
			b.WriteString("\n![")
			b.WriteString(alt)
			b.WriteString("](")
			b.WriteString(src)
			b.WriteString(")\n")
		case "a":
			href, _ := n.Attr("href")
			text := normalizeSpace(n.Text())
			href = absolutizePOJURL(href)
			if text == "" {
				return
			}
			if href != "" && !strings.HasPrefix(strings.ToLower(href), "javascript:") {
				b.WriteString("[")
				b.WriteString(text)
				b.WriteString("](")
				b.WriteString(href)
				b.WriteString(")")
			} else {
				b.WriteString(text)
			}
		case "sup":
			b.WriteString("^")
			b.WriteString(strings.TrimSpace(n.Text()))
		case "sub":
			b.WriteString("_")
			b.WriteString(strings.TrimSpace(n.Text()))
		case "b", "strong":
			t := strings.TrimSpace(pojInlineToMD(n))
			if t != "" {
				b.WriteString("**")
				b.WriteString(t)
				b.WriteString("**")
			}
		case "i", "em":
			t := strings.TrimSpace(pojInlineToMD(n))
			if t != "" {
				b.WriteString("*")
				b.WriteString(t)
				b.WriteString("*")
			}
		case "code":
			b.WriteString("`")
			b.WriteString(n.Text())
			b.WriteString("`")
		case "center", "font", "span", "div", "p", "table", "tbody", "tr", "td", "th":
			b.WriteString(pojInlineToMD(n))
		default:
			b.WriteString(pojInlineToMD(n))
		}
	})
	return b.String()
}

func pojBlockToMD(s *goquery.Selection) string {
	raw := pojInlineToMD(s)
	// 压缩行尾空白，保留有意义换行
	lines := strings.Split(raw, "\n")
	var out []string
	for _, ln := range lines {
		out = append(out, strings.TrimRight(ln, " \t\r"))
	}
	return collapseBlankLines(strings.Join(out, "\n"))
}

// extractPOJTitle 从 .ptt / <title> 取真实题名。
func extractPOJTitle(doc *goquery.Document, externalID string) string {
	if doc == nil {
		return ""
	}
	if t := normalizeSpace(doc.Find("div.ptt").First().Text()); t != "" {
		return normalizePOJTitle(t, externalID)
	}
	// <title>1000 -- A+B Problem</title>
	if t := normalizeSpace(doc.Find("title").First().Text()); t != "" {
		return normalizePOJTitle(t, externalID)
	}
	if externalID != "" {
		return "#" + externalID
	}
	return ""
}

// parsePOJProblemHTML 解析 POJ 题面 HTML（可单测）。
func parsePOJProblemHTML(html, externalID string) (*FetchedContent, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}
	pageTitle := normalizeSpace(doc.Find("title").First().Text())
	if strings.EqualFold(pageTitle, "Error") ||
		strings.Contains(strings.ToLower(html), "can not find problem") ||
		strings.Contains(html, "Error Occurred") {
		return nil, fmt.Errorf("POJ 题目不存在")
	}
	// 无 .ptt 且无有效 title → 当作解析失败
	if doc.Find("div.ptt").Length() == 0 && isPOJBadTitle(pageTitle) {
		return nil, fmt.Errorf("POJ 题面解析失败（无标题块）")
	}

	title := extractPOJTitle(doc, externalID)
	var b strings.Builder
	if title != "" {
		// title 形如「#1000. A+B Problem」；Markdown H1 只保留一个 #
		h1 := strings.TrimSpace(strings.TrimPrefix(title, "#"))
		b.WriteString("# ")
		b.WriteString(h1)
		b.WriteString("\n\n")
	}

	// 时限 / 内存
	if plm := doc.Find("div.plm").First(); plm.Length() > 0 {
		// 按表格单元格顺序提取 "Time Limit: 1000MS" 等
		var limits []string
		plm.Find("td").Each(func(_ int, td *goquery.Selection) {
			t := normalizeSpace(td.Text())
			if t == "" {
				return
			}
			low := strings.ToLower(t)
			if strings.Contains(low, "time limit") || strings.Contains(low, "memory limit") {
				limits = append(limits, t)
			}
		})
		if len(limits) == 0 {
			// 兜底整块文本
			if t := normalizeSpace(plm.Text()); t != "" {
				limits = append(limits, t)
			}
		}
		for _, lim := range limits {
			b.WriteString("- **")
			// "Time Limit: 1000MS" → **Time Limit:** 1000MS
			if i := strings.Index(lim, ":"); i > 0 {
				b.WriteString(strings.TrimSpace(lim[:i]))
				b.WriteString(":** ")
				b.WriteString(strings.TrimSpace(lim[i+1:]))
			} else {
				b.WriteString(lim)
				b.WriteString("**")
			}
			b.WriteString("\n")
		}
		if len(limits) > 0 {
			b.WriteString("\n")
		}
	}

	// 按 .pst 小节顺序输出；正文紧跟 .ptx 或 pre.sio
	doc.Find("p.pst").Each(func(_ int, pst *goquery.Selection) {
		secName := normalizeSpace(pst.Text())
		heading := pojSectionHeading(secName)
		if heading == "" {
			return
		}
		// 找紧随其后的内容兄弟节点
		var body *goquery.Selection
		for sib := pst.Next(); sib.Length() > 0; sib = sib.Next() {
			// 跳过空白文本节点
			if goquery.NodeName(sib) == "#text" && strings.TrimSpace(sib.Text()) == "" {
				continue
			}
			if sib.Is("p.pst") {
				break
			}
			if sib.Is("div.ptx") || sib.Is("pre.sio") || sib.Is("pre") {
				body = sib
				break
			}
			// 偶发包一层
			if inner := sib.Find("div.ptx, pre.sio").First(); inner.Length() > 0 {
				body = inner
				break
			}
			break
		}
		bodyMD := ""
		if body != nil && body.Length() > 0 {
			if body.Is("pre.sio") || body.Is("pre") {
				code := strings.TrimRight(body.Text(), "\n")
				bodyMD = "```\n" + code + "\n```"
			} else {
				bodyMD = pojBlockToMD(body)
			}
		}
		// Source 空链接常见，跳过空节
		if strings.TrimSpace(bodyMD) == "" {
			return
		}
		b.WriteString("## ")
		b.WriteString(heading)
		b.WriteString("\n\n")
		b.WriteString(bodyMD)
		b.WriteString("\n\n")
	})

	md := collapseBlankLines(b.String())
	if title == "" && md == "" {
		return nil, fmt.Errorf("POJ 题面为空")
	}
	// 至少要有描述类内容，避免只抓到导航
	if len(md) < 20 && doc.Find("div.ptx").Length() == 0 {
		return nil, fmt.Errorf("POJ 题面过短或解析失败")
	}
	if title == "" {
		title = externalID
	}
	return &FetchedContent{Title: title, ContentMD: md}, nil
}

func fetchPOJ(externalID, problemURL string) (*FetchedContent, error) {
	externalID = strings.TrimSpace(externalID)
	if problemURL == "" && externalID != "" {
		problemURL = pojBaseURL + "/problem?id=" + externalID
	}
	if problemURL == "" {
		return nil, fmt.Errorf("poj empty url")
	}
	// 从 URL 补 externalID
	if externalID == "" {
		if u, err := url.Parse(problemURL); err == nil {
			externalID = strings.TrimSpace(u.Query().Get("id"))
		}
	}
	resp, err := httpGet(problemURL)
	if err != nil {
		return nil, fmt.Errorf("poj fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("poj status %d", resp.StatusCode)
	}
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("poj read: %w", err)
	}
	return parsePOJProblemHTML(string(rb), externalID)
}

func fetchGeneric(problemURL string) (*FetchedContent, error) {
	if problemURL == "" {
		return nil, fmt.Errorf("empty url")
	}
	resp, err := httpGet(problemURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(doc.Find("h1").First().Text())
	if title == "" {
		title = strings.TrimSpace(doc.Find("title").Text())
	}
	doc.Find("script,style,nav,footer,header").Remove()
	text := collapseBlankLines(strings.TrimSpace(doc.Find("body").Text()))
	if text == "" {
		return nil, fmt.Errorf("empty page")
	}
	return &FetchedContent{Title: title, ContentMD: truncate(text, 15000)}, nil
}

func extractJSONString(s, key string) string {
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key):]
	ci := strings.Index(rest, ":")
	if ci < 0 {
		return ""
	}
	rest = strings.TrimSpace(rest[ci+1:])
	if !strings.HasPrefix(rest, `"`) {
		return ""
	}
	var b strings.Builder
	escaped := false
	for i := 1; i < len(rest); i++ {
		c := rest[i]
		if escaped {
			switch c {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"', '\\', '/':
				b.WriteByte(c)
			case 'u':
				// 简化：保留
				b.WriteByte(c)
			default:
				b.WriteByte(c)
			}
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			break
		}
		b.WriteByte(c)
	}
	return b.String()
}

func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func normalizeNowCoderText(s string) string {
	// 压缩空白，保留换行
	lines := strings.Split(s, "\n")
	var out []string
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			if len(out) > 0 && out[len(out)-1] != "" {
				out = append(out, "")
			}
			continue
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func collapseBlankLines(s string) string {
	re := regexp.MustCompile(`\n{3,}`)
	return strings.TrimSpace(re.ReplaceAllString(s, "\n\n"))
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
