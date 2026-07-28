package platform

import (
	"context"
	"cwxu-algo/app/common/utils/ojhttp"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"

	"github.com/go-kratos/kratos/v2/log"
)

const (
	// qojLoginMaxRetry 登录重试上限（原 20 次过多，风控下反复打接口）
	qojLoginMaxRetry = 5
	// qojLoginBaseDelay 登录重试退避基数（指数递增）
	qojLoginBaseDelay = 500 * time.Millisecond
	// qojMaxPagesAll needAll 翻页硬顶（对齐洛谷 200 页），防无界翻页
	qojMaxPagesAll = 200
)

type NewQOJ struct {
	mu       sync.RWMutex
	client   *http.Client
	lastUsed time.Time
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

	getResp, err := client.Do(getReq)
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

	postResp, err := client.Do(postReq)
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

	resp, err := client.Do(req)
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

	client, err := q.login("sanenchen", "Sanenchen123")
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
	if ctx == nil {
		ctx = context.Background()
	}
	baseUrl := fmt.Sprintf("https://qoj.ac/submissions?submitter=%s&page=", url.QueryEscape(username))
	client, err := q.getClient()
	if err != nil {
		return nil, err
	}

	var res []model.SubmitLog
	page := 1

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reqURL := fmt.Sprintf("%s%d", baseUrl, page)
		req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
		if err != nil {
			return nil, err
		}
		setBrowserHeaders(req)
		req.Header.Set("Referer", "https://qoj.ac/")

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}

		rb, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		html := string(rb)

		tbodyRe := regexp.MustCompile(`(?s)<tbody>(.*?)</tbody>`)
		tbodyMatch := tbodyRe.FindStringSubmatch(html)
		if len(tbodyMatch) < 2 {
			break
		}

		rowRe := regexp.MustCompile(`(?s)<tr>(.*?)</tr>`)
		rows := rowRe.FindAllStringSubmatch(tbodyMatch[1], -1)

		if len(rows) == 0 {
			break
		}

		for _, row := range rows {
			cellRe := regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
			cells := cellRe.FindAllStringSubmatch(row[1], -1)

			if len(cells) < 9 {
				continue
			}

			submitID := strings.TrimLeft(stripTags(cells[0][1]), "#")
			// 题名优先取 /problem/{id} 链接文本（「#19004. Title」），避免单元格杂讯
			problem := qojProblemFromCell(cells[1][1])
			rawStatus := stripTags(cells[3][1])
			lang := stripTags(cells[6][1])
			timeStr := stripTags(cells[8][1])

			status := "WA"
			if strings.HasPrefix(rawStatus, "AC") || strings.HasPrefix(rawStatus, "Accepted") {
				status = "AC"
			} else if strings.HasPrefix(rawStatus, "CE") {
				status = "CE"
			}

			t, _ := time.ParseInLocation("2006-01-02 15:04:05", timeStr, time.Local)

			res = append(res, model.SubmitLog{
				UserID:   userId,
				Platform: "QOJ",
				SubmitID: submitID,
				Problem:  problem,
				Lang:     lang,
				Status:   status,
				Time:     t,
			})
		}

		if !needAll {
			break
		}
		// 翻页硬顶（对齐洛谷 200 页），防异常用户/解析漂移导致无界翻页
		if page >= qojMaxPagesAll {
			break
		}

		nextPageStr := fmt.Sprintf("page=%d", page+1)
		if !strings.Contains(html, nextPageStr) {
			break
		}

		page++
		time.Sleep(500 * time.Millisecond) // 防止翻页过快再次触发 Cloudflare 拦截
	}

	return res, nil
}

func (q *NewQOJ) Name() string {
	return spider.QOJ
}

func init() {
	spider.Register(&NewQOJ{})
}
