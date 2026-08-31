package platform

import (
	"context"
	"crypto/md5"
	"cwxu-algo/app/common/utils/ojhttp"
	"encoding/hex"
	"fmt"
	stdhtml "html"
	"io"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"

	"github.com/go-kratos/kratos/v2/log"
)

const (
	// qojLoginMaxRetry 登录重试上限（原 20 次过多，风控下反复打接口）
	qojLoginMaxRetry = 5
	// qojLoginBaseDelay 登录重试退避基数（指数递增）
	qojLoginBaseDelay = 500 * time.Millisecond
	// qojProductionMaxPages 为 0 时按远端 next 持续抓取，资源上限由调用方 ctx 控制。
	qojProductionMaxPages = 0
)

type NewQOJ struct {
	mu       sync.RWMutex
	client   *http.Client
	lastUsed time.Time
	username string
	password string
}

const (
	qojMaxVerdictLength   = 64
	qojMaxReasonableScore = 1_000_000
)

var (
	qojReasonableVerdictRe = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} .,:;()/_+-]*$`)
	qojErrorResultTerms    = []string{
		"ACCESS DENIED", "UNAUTHORIZED", "AUTHENTICATION", "FORBIDDEN",
		"LOGIN", "SIGN IN", "SESSION EXPIRED", "CENTRAL FAILURE",
		"SERVICE UNAVAILABLE", "SERVER ERROR", "INTERNAL ERROR", "GATEWAY ERROR",
		"BAD GATEWAY", "REQUEST TIMEOUT", "NETWORK ERROR",
	}
)

func isQOJReasonableVerdict(result string) bool {
	if utf8.RuneCountInString(result) > qojMaxVerdictLength || !qojReasonableVerdictRe.MatchString(result) {
		return false
	}
	for _, term := range qojErrorResultTerms {
		if strings.Contains(result, term) {
			return false
		}
	}
	return true
}

func normalizeQOJResult(raw string) (string, error) {
	result := strings.TrimSpace(stdhtml.UnescapeString(raw))
	if result == "" {
		return "", fmt.Errorf("QOJ 结果为空")
	}
	resultWithoutMarker := strings.TrimRight(result, "✓✔✅")
	hasAcceptedMarker := resultWithoutMarker != result
	upper := strings.ToUpper(strings.TrimSpace(resultWithoutMarker))
	if upper == "" {
		return "", fmt.Errorf("QOJ 结果为空")
	}
	switch upper {
	case "AC", "ACCEPTED":
		return "AC", nil
	case "CE", "COMPILE ERROR", "COMPILATION ERROR":
		return "CE", nil
	case "WA", "WRONG ANSWER":
		return "WA", nil
	case "TLE", "TIME LIMIT EXCEEDED":
		return "TLE", nil
	case "MLE", "MEMORY LIMIT EXCEEDED":
		return "MLE", nil
	case "RE", "RUNTIME ERROR":
		return "RE", nil
	case "OLE", "OUTPUT LIMIT EXCEEDED":
		return "OLE", nil
	case "PE", "PRESENTATION ERROR":
		return "PE", nil
	case "ILE", "IDLENESS LIMIT EXCEEDED":
		return "ILE", nil
	case "SV", "SECURITY VIOLATED":
		return "SV", nil
	case "JF", "JUDGEMENT FAILED", "JUDGMENT FAILED":
		return "JF", nil
	case "JUDGING", "PENDING", "TESTING", "JUDGED, JUDGING", "…", "...":
		return "JUDGING", nil
	case "WAITING", "WAITING REJUDGE", "JUDGED, WAITING", "WJ", "QUEUE", "IN QUEUE", "IN_QUEUE":
		return "WAITING", nil
	}
	if score, err := strconv.ParseFloat(upper, 64); err == nil {
		if math.IsNaN(score) || math.IsInf(score, 0) || score < 0 || score > qojMaxReasonableScore {
			return "", fmt.Errorf("异常 QOJ 分数: %q", upper)
		}
		if hasAcceptedMarker {
			return "AC", nil
		}
		return "WA", nil
	}
	if isQOJReasonableVerdict(upper) {
		// QOJ may add compact verdict labels independently. Preserve those labels so
		// one new verdict cannot abort the entire submissions page.
		return upper, nil
	}
	return "", fmt.Errorf("未知或异常 QOJ 结果: %q", result)
}

// 模拟真实浏览器的请求头
func setBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Sec-Ch-Ua", `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`)
	req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	req.Header.Set("Sec-Ch-Ua-Platform", `"Windows"`)
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Connection", "keep-alive")
}

func (q *NewQOJ) doLogin(
	client *http.Client,
	username, password string,
) (success bool, body string, err error) {
	loginURL := "https://qoj.ac/login"

	// 第一步：访问 GET /login
	getReq, err := http.NewRequest("GET", loginURL, nil)
	if err != nil {
		return false, "", fmt.Errorf("create GET login request failed: %w", err)
	}
	setBrowserHeaders(getReq)

	getResp, err := ojhttp.DoWithClient(client, getReq)
	if err != nil {
		return false, "", fmt.Errorf("GET login page failed: %w", err)
	}
	defer getResp.Body.Close()

	getPageBytes, err := io.ReadAll(getResp.Body)
	if err != nil {
		return false, "", fmt.Errorf("read login page failed: %w", err)
	}
	pageContent := string(getPageBytes)

	// 使用正则匹配提取 _token
	re := regexp.MustCompile(`_token\s*:\s*"([^"]+)"`)
	matches := re.FindStringSubmatch(pageContent)
	if len(matches) < 2 {
		return false, pageContent, fmt.Errorf("failed to extract _token from page, might be blocked by Cloudflare")
	}
	token := matches[1]

	// 第二步：MD5 加密
	hasher := md5.New()
	hasher.Write([]byte(password))
	passwordMD5 := hex.EncodeToString(hasher.Sum(nil))

	formData := url.Values{}
	formData.Set("_token", token)
	formData.Set("login", "")
	formData.Set("username", username)
	formData.Set("password", passwordMD5)
	formData.Set("trust", "")

	// 发起 POST 登录请求
	postReq, err := http.NewRequest("POST", loginURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return false, "", fmt.Errorf("create POST request failed: %w", err)
	}
	setBrowserHeaders(postReq)
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Origin", "https://qoj.ac")
	postReq.Header.Set("Referer", "https://qoj.ac/login")

	postResp, err := ojhttp.DoWithClient(client, postReq)
	if err != nil {
		return false, "", fmt.Errorf("POST login failed: %w", err)
	}
	defer postResp.Body.Close()

	bodyBytes, err := io.ReadAll(postResp.Body)
	if err != nil {
		return false, "", fmt.Errorf("read POST response failed: %w", err)
	}
	body = string(bodyBytes)

	if strings.TrimSpace(body) == "ok" {
		return true, body, nil
	}
	return false, body, nil
}

func (q *NewQOJ) login(username, password string) (*http.Client, error) {
	jar, _ := cookiejar.New(nil)
	client := ojhttp.NewWithJar(jar)

	for attempt := 1; attempt <= qojLoginMaxRetry; attempt++ {
		ok, body, err := q.doLogin(client, username, password)
		if err != nil {
			return nil, err
		}
		if ok {
			return client, err
		}
		log.Info(fmt.Sprintf("retry %d/%d, resp=%s\n", attempt, qojLoginMaxRetry, body))
		// 指数退避，减轻风控/验证码接口压力
		time.Sleep(qojLoginBaseDelay << (attempt - 1))
	}
	return nil, fmt.Errorf("login failed after %d retries", qojLoginMaxRetry)
}

// isSessionValid 校验给定客户端会话是否有效（锁内快照传入，网络请求不持锁读共享字段）
func (q *NewQOJ) isSessionValid(client *http.Client) bool {
	if client == nil {
		return false
	}

	req, err := http.NewRequest("GET", "https://qoj.ac/", nil)
	if err != nil {
		return false
	}
	setBrowserHeaders(req)

	resp, err := ojhttp.DoWithClient(client, req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return false
	}
	return strings.Contains(string(b), "/logout?_token=")
}

func (q *NewQOJ) getClient() (*http.Client, error) {
	q.mu.RLock()
	cached := q.client
	expired := time.Since(q.lastUsed) >= 30*time.Minute
	q.mu.RUnlock()

	if cached != nil && !expired {
		if q.isSessionValid(cached) {
			// 会话命中：刷新 lastUsed，活跃会话不被 30 分钟窗口误判过期
			q.mu.Lock()
			if q.client == cached {
				q.lastUsed = time.Now()
			}
			q.mu.Unlock()
			return cached, nil
		}
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	if q.client != nil && time.Since(q.lastUsed) < 30*time.Minute && q.isSessionValid(q.client) {
		q.lastUsed = time.Now()
		return q.client, nil
	}

	u, p := q.username, q.password
	if u == "" || p == "" {
		return nil, fmt.Errorf("QOJ 爬虫账号未配置，请在站点设置中填写")
	}
	client, err := q.login(u, p)
	if err != nil {
		return nil, err
	}
	q.client = client
	q.lastUsed = time.Now()
	return client, nil
}

func stripTags(s string) string {
	re := regexp.MustCompile(`(?s)<[^>]*>`)
	return strings.TrimSpace(re.ReplaceAllString(s, ""))
}

// qojProblemLinkRe 提交表「题目」列：<a href="/problem/19004">#19004. Title</a>
var qojProblemLinkRe = regexp.MustCompile(`(?is)<a[^>]+href=["'][^"']*/problem/(\d+)["'][^>]*>(.*?)</a>`)

// qojProblemFromCell 从提交列表题目单元格提取「#id. 题名」文本。
func qojProblemFromCell(cellHTML string) string {
	if m := qojProblemLinkRe.FindStringSubmatch(cellHTML); len(m) >= 3 {
		text := strings.Join(strings.Fields(stripTags(m[2])), " ")
		if text != "" {
			// 链接文本只有题号时补全 #id
			if text == m[1] || text == "#"+m[1] {
				return "#" + m[1]
			}
			return text
		}
		return "#" + m[1]
	}
	return strings.Join(strings.Fields(stripTags(cellHTML)), " ")
}

func (q *NewQOJ) FetchSubmitLog(ctx context.Context, userId int64, username string, needAll bool) ([]model.SubmitLog, error) {
	logs, _, err := q.FetchSubmitLogComplete(ctx, userId, username, needAll)
	return logs, err
}

func (q *NewQOJ) FetchSubmitLogComplete(ctx context.Context, userId int64, username string, needAll bool) ([]model.SubmitLog, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	baseUrl := fmt.Sprintf("https://qoj.ac/submissions?submitter=%s&page=", url.QueryEscape(username))
	client, err := q.getClient()
	if err != nil {
		return nil, false, err
	}
	return fetchQOJSubmitLogs(ctx, client, baseUrl, userId, needAll, qojProductionMaxPages, func() { time.Sleep(500 * time.Millisecond) })
}

func fetchQOJSubmitLogs(ctx context.Context, client *http.Client, baseURL string, userID int64, needAll bool, maxPages int, pause func()) ([]model.SubmitLog, bool, error) {
	var res []model.SubmitLog
	seen := make(map[string]struct{})
	for page := 1; maxPages <= 0 || page <= maxPages; page++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		reqURL := fmt.Sprintf("%s%d", baseURL, page)
		req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
		if err != nil {
			return nil, false, err
		}
		setBrowserHeaders(req)
		req.Header.Set("Referer", "https://qoj.ac/")

		resp, err := client.Do(req)
		if err != nil {
			return nil, false, err
		}

		rb, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, false, err
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return nil, false, fmt.Errorf("QOJ submissions page %d HTTP %d", page, resp.StatusCode)
		}

		html := string(rb)
		logs, hasNext, err := parseQOJSubmissionPage(html, userID, page+1, seen)
		if err != nil {
			return nil, false, fmt.Errorf("QOJ submissions page %d: %w", page, err)
		}
		res = append(res, logs...)

		if !needAll {
			return res, false, nil
		}
		if !hasNext {
			return res, true, nil
		}
		if (maxPages <= 0 || page < maxPages) && pause != nil {
			pause()
		}
	}
	return res, false, nil
}

var (
	qojTbodyRe       = regexp.MustCompile(`(?is)<tbody[^>]*>(.*?)</tbody>`)
	qojRowRe         = regexp.MustCompile(`(?is)<tr[^>]*>(.*?)</tr>`)
	qojCellRe        = regexp.MustCompile(`(?is)<td[^>]*>(.*?)</td>`)
	qojColspanCellRe = regexp.MustCompile(`(?is)^<td\b[^>]*\scolspan\s*=\s*(?:"\d+"|'\d+'|\d+(?:\s|>))`)
)

func isQOJEmptySubmissionCell(cellTag, cellHTML string) bool {
	if !qojColspanCellRe.MatchString(strings.TrimSpace(cellTag)) {
		return false
	}
	text := strings.Join(strings.Fields(stdhtml.UnescapeString(stripTags(cellHTML))), " ")
	return strings.EqualFold(text, "none") || text == "无"
}

func parseQOJSubmissionPage(html string, userID int64, nextPage int, seen map[string]struct{}) ([]model.SubmitLog, bool, error) {
	tbody := qojTbodyRe.FindStringSubmatch(html)
	if len(tbody) < 2 {
		return nil, false, fmt.Errorf("submission table body missing")
	}
	rows := qojRowRe.FindAllStringSubmatch(tbody[1], -1)
	if len(rows) == 0 {
		return nil, false, fmt.Errorf("submission table body has no rows")
	}
	logs := make([]model.SubmitLog, 0, len(rows))
	for rowIndex, row := range rows {
		cells := qojCellRe.FindAllStringSubmatch(row[1], -1)
		if len(cells) < 9 {
			if len(rows) == 1 && len(cells) == 1 && isQOJEmptySubmissionCell(cells[0][0], cells[0][1]) {
				return logs, false, nil
			}
			return nil, false, fmt.Errorf("row %d has %d columns, want at least 9", rowIndex+1, len(cells))
		}
		submitID := strings.TrimSpace(strings.TrimLeft(stripTags(cells[0][1]), "#"))
		if submitID == "" {
			return nil, false, fmt.Errorf("row %d has empty submit ID", rowIndex+1)
		}
		if _, exists := seen[submitID]; exists {
			return nil, false, fmt.Errorf("duplicate submit ID %q", submitID)
		}
		seen[submitID] = struct{}{}
		status, err := normalizeQOJResult(stripTags(cells[3][1]))
		if err != nil {
			return nil, false, fmt.Errorf("submission %s: %w", submitID, err)
		}
		timeText := stripTags(cells[8][1])
		submittedAt, err := time.ParseInLocation("2006-01-02 15:04:05", timeText, time.Local)
		if err != nil {
			return nil, false, fmt.Errorf("submission %s time %q: %w", submitID, timeText, err)
		}
		logs = append(logs, model.SubmitLog{
			UserID: userID, Platform: spider.QOJ, SubmitID: submitID,
			Problem: qojProblemFromCell(cells[1][1]), Lang: stripTags(cells[6][1]), Status: status, Time: submittedAt,
		})
	}
	nextPageRe := regexp.MustCompile(fmt.Sprintf(`(?is)(?:[?&]|&amp;)page=%d(?:[^0-9]|$)`, nextPage))
	return logs, nextPageRe.MatchString(html), nil
}

// SetCredentials 注入爬虫登录凭证（从站点设置读取）
func (q *NewQOJ) SetCredentials(username, password string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.username != username || q.password != password {
		q.client = nil
		q.username = username
		q.password = password
	}
}

func (q *NewQOJ) Name() string {
	return spider.QOJ
}

func init() {
	spider.Register(&NewQOJ{})
}
