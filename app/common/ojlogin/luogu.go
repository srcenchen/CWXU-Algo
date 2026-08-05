package ojlogin

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"

	"cwxu-algo/app/common/utils/ojhttp"
)

const (
	luoguCaptchaURL = "https://www.luogu.com.cn/lg4/captcha"
	luoguOCRURL     = "https://api.alistgo.com/ocr/file"
	luoguLoginURL   = "https://www.luogu.com.cn/do-auth/password"
	luoguMaxRetry   = 5
	luoguBaseDelay  = 500 * time.Millisecond
)

// VerifyLuogu 尝试用账号密码登录洛谷；成功返回 nil。
func VerifyLuogu(username, password string) error {
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)
	if username == "" || password == "" {
		return fmt.Errorf("洛谷账号或密码为空")
	}
	jar, _ := cookiejar.New(nil)
	client := ojhttp.NewWithJar(jar)
	for attempt := 1; attempt <= luoguMaxRetry; attempt++ {
		if attempt > 1 {
			time.Sleep(luoguBaseDelay << (attempt - 2))
		}
		resp, err := client.Get(luoguCaptchaURL)
		if err != nil {
			return fmt.Errorf("拉取验证码失败: %w", err)
		}
		img, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("读取验证码失败: %w", err)
		}
		code, err := ocrImage(client, img)
		if err != nil {
			return fmt.Errorf("识别验证码失败: %w", err)
		}
		code = strings.TrimSpace(code)
		ok, body, err := doLuoguLogin(client, username, password, code)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		_ = body
	}
	return fmt.Errorf("洛谷登录失败（验证码或账号密码有误）")
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
		return "", err
	}
	defer resp.Body.Close()
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
		return false, "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, "", err
	}
	body := string(b)
	if strings.Contains(body, "errorCode") {
		return false, body, nil
	}
	return true, body, nil
}
