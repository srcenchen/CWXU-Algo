package platform

import (
	"bytes"
	"context"
	"cwxu-algo/app/common/utils/ojhttp"
	"cwxu-algo/app/core_data/internal/data/model"
	"cwxu-algo/app/core_data/internal/spider"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-kratos/kratos/v2/log"
)

const (
	// luoguLoginMaxRetry 登录重试上限（原 20 次过多，OCR/验证码接口反复打）
	luoguLoginMaxRetry = 5
	// luoguLoginBaseDelay 登录重试退避基数（指数递增）
	luoguLoginBaseDelay = 500 * time.Millisecond
)

type NewLuoGu struct {
	mu       sync.RWMutex
	client   *http.Client
	lastUsed time.Time
	username string
	password string
}

func (lg *NewLuoGu) ocrImage(client *http.Client, url string, img []byte) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	part, err := w.CreateFormFile("image", "captcha.png")
	if err != nil {
		return "", err
	}
	if _, err = part.Write(img); err != nil {
		return "", err
	}
	w.Close()
	req, err := http.NewRequest("POST", url, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
func (lg *NewLuoGu) doLogin(
	client *http.Client,
	url, username, password, captcha string,
) (success bool, body string, err error) {
	payload := fmt.Sprintf(
		`{"username":"%s","password":"%s","captcha":"%s"}`,
		username, password, captcha,
	)
	resp, err := client.Post(
		url,
		"application/json",
		bytes.NewReader([]byte(payload)),
	)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, "", err
	}
	body = string(b)
	// 只要出现 errorCode，就认为失败，交给外层重试
	if strings.Contains(body, "errorCode") {
		return false, body, nil
	}
	return true, body, nil
}

func (lg *NewLuoGu) login(username, password string) (*http.Client, error) {
	const (
		captchaURL = "https://www.luogu.com.cn/lg4/captcha"
		ocrURL     = "https://api.alistgo.com/ocr/file"
		loginURL   = "https://www.luogu.com.cn/do-auth/password"
		maxRetry   = luoguLoginMaxRetry
	)

	jar, _ := cookiejar.New(nil)
	client := ojhttp.NewWithJar(jar)

	for attempt := 1; attempt <= maxRetry; attempt++ {
		// 指数退避，减轻验证码/OCR 接口压力
		if attempt > 1 {
			time.Sleep(luoguLoginBaseDelay << (attempt - 2))
		}
		// 1. 拉验证码（cookie 在这里生成）
		resp, err := client.Get(captchaURL)
		if err != nil {
			return nil, err
		}
		imgBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		// 2. OCR 识别验证码
		code, err := lg.ocrImage(client, ocrURL, imgBytes)
		if err != nil {
			return nil, err
		}
		code = strings.TrimSpace(code)
		// 3. 发起登录
		ok, body, err := lg.doLogin(client, loginURL, username, password, code)
		if err != nil {
			return nil, err
		}
		// 没有 errorCode，说明成功
		if ok {
			// log.Info("login success:", body)
			return client, err
		}
		log.Info(fmt.Sprintf("retry %d/%d, captcha=%s, resp=%s\n",
			attempt, maxRetry, code, body))
	}
	return nil, fmt.Errorf("login failed after %d retries", maxRetry)
}

type Injection struct {
	Code        int `json:"code"`
	CurrentData struct {
		Records struct {
			Result  []Record `json:"result"`
			PerPage int      `json:"perPage"`
			Count   int      `json:"count"`
		} `json:"records"`
	} `json:"currentData"`
}

type Record struct {
	ID         int64 `json:"id"`
	SubmitTime int64 `json:"submitTime"`
	Status     int   `json:"status"`
	Score      *int  `json:"score"`
	Time       int   `json:"time"`
	Memory     int   `json:"memory"`
	Language   int   `json:"language"`
	Problem    struct {
		Pid        string `json:"pid"`
		Title      string `json:"title"`
		Difficulty int    `json:"difficulty"`
	} `json:"problem"`
}

func (lg *NewLuoGu) parseLuoGuHTML(html string) (*Injection, error) {
	// 抠 decodeURIComponent 里的字符串
	re := regexp.MustCompile(`window\._feInjection\s*=\s*JSON\.parse\(decodeURIComponent\("(.+?)"\)\)`)
	m := re.FindStringSubmatch(html)
	if len(m) != 2 {
		return nil, fmt.Errorf("未找到 _feInjection")
	}

	// URL 解码
	decoded, err := url.QueryUnescape(m[1])
	if err != nil {
		return nil, err
	}

	var inj Injection
	if err := json.Unmarshal([]byte(decoded), &inj); err != nil {
		return nil, err
	}

	return &inj, nil
}

// isSessionValid 校验给定客户端会话是否有效（锁内快照传入，网络请求不持锁读共享字段）
func (lg *NewLuoGu) isSessionValid(client *http.Client) bool {
	if client == nil {
		return false
	}
	resp, err := client.Get("https://www.luogu.com.cn/api/user/search?user=sanenchen")
	if err != nil {
		return false
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	// If redirected to login page, session is invalid
	if resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusTemporaryRedirect {
		return false
	}
	return true
}

// getClient returns a cached client or creates a new one via login
func (lg *NewLuoGu) getClient() (*http.Client, error) {
	lg.mu.RLock()
	cached := lg.client
	expired := time.Since(lg.lastUsed) >= 30*time.Minute
	lg.mu.RUnlock()

	if cached != nil && !expired {
		// Validate session without holding lock（用快照，不再无锁读共享字段）
		if lg.isSessionValid(cached) {
			// 会话命中：刷新 lastUsed，活跃会话不被 30 分钟窗口误判过期
			lg.mu.Lock()
			if lg.client == cached {
				lg.lastUsed = time.Now()
			}
			lg.mu.Unlock()
			return cached, nil
		}
	}

	lg.mu.Lock()
	defer lg.mu.Unlock()

	// Double-check after acquiring write lock
	if lg.client != nil && time.Since(lg.lastUsed) < 30*time.Minute && lg.isSessionValid(lg.client) {
		lg.lastUsed = time.Now()
		return lg.client, nil
	}

	u, p := lg.username, lg.password
	if u == "" || p == "" {
		return nil, fmt.Errorf("洛谷爬虫账号未配置，请在站点设置中填写")
	}
	client, err := lg.login(u, p)
	if err != nil {
		return nil, err
	}
	lg.client = client
	lg.lastUsed = time.Now()
	return client, nil
}

func (lg *NewLuoGu) FetchSubmitLog(ctx context.Context, userId int64, username string, needAll bool) ([]model.SubmitLog, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	baseUrl := fmt.Sprintf("https://www.luogu.com.cn/record/list?user=%s&page=", username)
	client, err := lg.getClient()
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", baseUrl+"1", nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var subs []Record
	inj, err := lg.parseLuoGuHTML(string(rb))
	if err != nil {
		return nil, err
	}
	subs = inj.CurrentData.Records.Result
	if needAll {
		// 页间短歇 + 硬顶 200 页，避免无界翻页触发风控 / 占满 worker
		perPage := inj.CurrentData.Records.PerPage
		if perPage <= 0 {
			perPage = 20
		}
		totPage := inj.CurrentData.Records.Count/perPage + 1
		if totPage > 200 {
			totPage = 200
		}
		for i := 2; i <= totPage; i++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			time.Sleep(300 * time.Millisecond)
			req, _ := http.NewRequestWithContext(ctx, "GET", baseUrl+fmt.Sprint(i), nil)
			resp, err := client.Do(req)
			if err != nil {
				return nil, err
			}
			rb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			inj, err := lg.parseLuoGuHTML(string(rb))
			if err != nil {
				return nil, err
			}
			subs = append(subs, inj.CurrentData.Records.Result...)
		}
	}
	var res []model.SubmitLog
	for _, sub := range subs {
		var status, lang string
		if sub.Status != 12 {
			status = "WA"
		} else {
			status = "AC"
		}
		if sub.Language == 34 {
			lang = "C++"
		} else {
			lang = "Others"
		}
		res = append(res, model.SubmitLog{
			UserID:   userId,
			Platform: spider.LuoGu,
			SubmitID: fmt.Sprint(sub.ID),
			Problem:  sub.Problem.Pid + " " + sub.Problem.Title,
			Lang:     lang,
			Status:   status,
			Time:     time.Unix(sub.SubmitTime, 0),
		})
	}
	return res, nil
}

// SetCredentials 注入爬虫登录凭证（从站点设置读取）
func (lg *NewLuoGu) SetCredentials(username, password string) {
	lg.mu.Lock()
	defer lg.mu.Unlock()
	if lg.username != username || lg.password != password {
		lg.client = nil
		lg.username = username
		lg.password = password
	}
}

func (lg *NewLuoGu) Name() string {
	return spider.LuoGu
}

// FetchRating 经官方 JSON API 取当前 Elo（需登录会话）。
// 洛谷前端已不再在用户页注入 _feInjection；rating 字段也已弃用，以 eloValue / elo.rating 为准。
// 未参加过 rated 比赛时 eloValue 为 null → hasRating=false。
func (lg *NewLuoGu) FetchRating(username string) (int, bool, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return 0, false, fmt.Errorf("luogu username 为空")
	}
	client, err := lg.getClient()
	if err != nil {
		return 0, false, err
	}
	// 绑定字段为用户编号（uid）；也兼容用户名搜索
	uid, err := lg.resolveUID(client, username)
	if err != nil {
		return 0, false, err
	}
	req, err := http.NewRequest("GET", fmt.Sprintf("https://www.luogu.com.cn/api/user/info/%d", uid), nil)
	if err != nil {
		return 0, false, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; GoAlgoSpider/1.0)")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, false, fmt.Errorf("luogu user info 请求失败: %w", err)
	}
	rb, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return 0, false, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, false, fmt.Errorf("luogu user info 状态码 %d: %s", resp.StatusCode, truncateForErr(string(rb), 200))
	}
	var out struct {
		User *struct {
			// 旧字段：多数账号已不再返回有效值
			Rating *int `json:"rating"`
			// 新 Elo：未参赛为 null
			EloValue *int `json:"eloValue"`
			Elo      *struct {
				Rating int `json:"rating"`
			} `json:"elo"`
		} `json:"user"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return 0, false, fmt.Errorf("luogu user info 解析失败: %w", err)
	}
	if out.User == nil {
		return 0, false, nil
	}
	// 优先 eloValue（主站当前展示），再 elo.rating，最后旧 rating
	if out.User.EloValue != nil && *out.User.EloValue > 0 {
		return *out.User.EloValue, true, nil
	}
	if out.User.Elo != nil && out.User.Elo.Rating > 0 {
		return out.User.Elo.Rating, true, nil
	}
	if out.User.Rating != nil && *out.User.Rating > 0 {
		return *out.User.Rating, true, nil
	}
	return 0, false, nil
}

func truncateForErr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (lg *NewLuoGu) resolveUID(client *http.Client, username string) (int64, error) {
	// 纯数字：直接当 uid
	if n, err := strconv.ParseInt(username, 10, 64); err == nil && n > 0 {
		return n, nil
	}
	api := "https://www.luogu.com.cn/api/user/search?keyword=" + url.QueryEscape(username)
	resp, err := client.Get(api)
	if err != nil {
		return 0, fmt.Errorf("luogu search 失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	var out struct {
		Users []struct {
			UID  int64  `json:"uid"`
			Name string `json:"name"`
		} `json:"users"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("luogu search 解析失败: %w", err)
	}
	for _, u := range out.Users {
		if strings.EqualFold(u.Name, username) {
			return u.UID, nil
		}
	}
	if len(out.Users) == 1 {
		return out.Users[0].UID, nil
	}
	return 0, fmt.Errorf("luogu 未找到用户: %s", username)
}

func init() {
	spider.Register(&NewLuoGu{})
}
