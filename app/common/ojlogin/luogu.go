package ojlogin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	"cwxu-algo/app/common/utils/ojhttp"

	"github.com/go-kratos/kratos/v2/log"
)

const (
	luoguCaptchaURL = "https://www.luogu.com.cn/lg4/captcha"
	luoguOCRURL     = "https://api.alistgo.com/ocr/file"
	luoguLoginURL   = "https://www.luogu.com.cn/do-auth/password"
	luoguMaxRetry   = 3
	luoguBaseDelay  = 800 * time.Millisecond
)

type luoguLoginResp struct {
	ErrorCode int    `json:"errorCode"`
	ErrMsg    string `json:"errorMessage"`
	Status     int   `json:"status"`
}

// VerifyLuogu 尝试用账号密码登录洛谷；成功返回 nil。
func VerifyLuogu(username, password string) error {
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)
	if username == "" || password == "" {
		return fmt.Errorf("账号或密码为空")
	}
	jar, _ := cookiejar.New(nil)
	client := ojhttp.NewWithJar(jar)

	var lastErr string
	for attempt := 1; attempt <= luoguMaxRetry; attempt++ {
		if attempt > 1 {
			time.Sleep(luoguBaseDelay << (attempt - 2))
		}
		// 1. 拉验证码
		resp, err := client.Get(luoguCaptchaURL)
		if err != nil {
			return fmt.Errorf("拉取验证码失败: %v", err)
		}
		img, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("读取验证码图片失败: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("验证码接口返回 %d，可能被限流", resp.StatusCode)
		}

		// 2. OCR
		code, err := ocrImage(client, img)
		if err != nil {
			log.Warnf("ojlogin luogu: OCR 失败 attempt=%d: %v", attempt, err)
			lastErr = fmt.Sprintf("验证码识别失败: %v", err)
			continue
		}
		code = strings.TrimSpace(code)
		if code == "" {
			lastErr = "验证码识别结果为空"
			continue
		}

		// 3. 登录
		ok, body, loginErr := doLuoguLogin(client, username, password, code)
		if loginErr != nil {
			return fmt.Errorf("登录请求失败: %v", loginErr)
		}
		if ok {
			return nil
		}
		// 解析洛谷返回的具体错误
		var parsed luoguLoginResp
		if json.Unmarshal([]byte(body), &parsed) == nil && parsed.ErrMsg != "" {
			lastErr = parsed.ErrMsg
		} else if body != "" {
			// 截取前 100 字符避免太长
			msg := body
			if len(msg) > 100 {
				msg = msg[:100]
			}
			lastErr = msg
		} else {
			lastErr = fmt.Sprintf("HTTP %d 无响应体", resp.StatusCode)
		}
		log.Warnf("ojlogin luogu: attempt=%d captcha=%s err=%s", attempt, code, lastErr)
	}
	if lastErr != "" {
		return fmt.Errorf("洛谷登录失败（%s）", lastErr)
	}
	return fmt.Errorf("洛谷登录失败，已重试 %d 次", luoguMaxRetry)
}

func ocrImage(client *http.Client, img []byte) (string, error) {
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
	req, err := http.NewRequest("POST", luoguOCRURL, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("OCR 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("OCR 返回 %d: %s", resp.StatusCode, string(b))
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func doLuoguLogin(client *http.Client, username, password, captcha string) (bool, string, error) {
	payload := fmt.Sprintf(
		`{"username":"%s","password":"%s","captcha":"%s"}`,
		username, password, captcha,
	)
	resp, err := client.Post(luoguLoginURL, "application/json", bytes.NewReader([]byte(payload)))
	if err != nil {
		return false, "", fmt.Errorf("POST 登录接口失败: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, "", fmt.Errorf("读取登录响应失败: %w", err)
	}
	body := string(b)
	if strings.Contains(body, "errorCode") {
		return false, body, nil
	}
	return true, body, nil
}
