package ojlogin

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"cwxu-algo/app/common/utils/ojhttp"
)

const (
	qojLoginURL  = "https://qoj.ac/login"
	qojMaxRetry  = 5
	qojBaseDelay = 500 * time.Millisecond
)

// VerifyQOJ 尝试用账号密码登录 QOJ；成功返回 nil。
func VerifyQOJ(username, password string) error {
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)
	if username == "" || password == "" {
		return fmt.Errorf("QOJ 账号或密码为空")
	}
	jar, _ := cookiejar.New(nil)
	client := ojhttp.NewWithJar(jar)
	for attempt := 1; attempt <= qojMaxRetry; attempt++ {
		ok, body, err := doQOJLogin(client, username, password)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		_ = body
		time.Sleep(qojBaseDelay << (attempt - 1))
	}
	return fmt.Errorf("QOJ 登录失败（账号密码有误或站点拦截）")
}

func setQOJBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
}

func doQOJLogin(client *http.Client, username, password string) (bool, string, error) {
	getReq, err := http.NewRequest("GET", qojLoginURL, nil)
	if err != nil {
		return false, "", fmt.Errorf("创建登录请求失败: %w", err)
	}
	setQOJBrowserHeaders(getReq)
	getResp, err := client.Do(getReq)
	if err != nil {
		return false, "", fmt.Errorf("打开登录页失败: %w", err)
	}
	pageBytes, err := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if err != nil {
		return false, "", fmt.Errorf("读取登录页失败: %w", err)
	}
	pageContent := string(pageBytes)
	re := regexp.MustCompile(`_token\s*:\s*"([^"]+)"`)
	matches := re.FindStringSubmatch(pageContent)
	if len(matches) < 2 {
		return false, pageContent, fmt.Errorf("无法解析登录令牌，可能被拦截")
	}
	token := matches[1]

	hasher := md5.New()
	hasher.Write([]byte(password))
	passwordMD5 := hex.EncodeToString(hasher.Sum(nil))

	formData := url.Values{}
	formData.Set("_token", token)
	formData.Set("login", "")
	formData.Set("username", username)
	formData.Set("password", passwordMD5)
	formData.Set("trust", "")

	postReq, err := http.NewRequest("POST", qojLoginURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return false, "", fmt.Errorf("创建登录提交失败: %w", err)
	}
	setQOJBrowserHeaders(postReq)
	postReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	postReq.Header.Set("Origin", "https://qoj.ac")
	postReq.Header.Set("Referer", "https://qoj.ac/login")

	postResp, err := client.Do(postReq)
	if err != nil {
		return false, "", fmt.Errorf("提交登录失败: %w", err)
	}
	bodyBytes, err := io.ReadAll(postResp.Body)
	postResp.Body.Close()
	if err != nil {
		return false, "", fmt.Errorf("读取登录响应失败: %w", err)
	}
	body := string(bodyBytes)
	if strings.TrimSpace(body) == "ok" {
		return true, body, nil
	}
	return false, body, nil
}
