package ojlogin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

// VerifyVJudge verifies a VirtualOJ/VJudge account without persisting its
// session cookie.
func VerifyVJudge(ctx context.Context, username, password string) error {
	if strings.TrimSpace(username) == "" || strings.TrimSpace(password) == "" {
		return fmt.Errorf("账号或密码为空")
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
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/131 Safari/537.36")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("VirtualOJ 登录失败: %w", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("VirtualOJ 登录失败: status %d", resp.StatusCode)
	}
	check, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://vjudge.net/user/checkLogInStatus", nil)
	if err != nil {
		return err
	}
	check.Header.Set("User-Agent", req.Header.Get("User-Agent"))
	status, err := client.Do(check)
	if err != nil {
		return fmt.Errorf("VirtualOJ 状态检查失败: %w", err)
	}
	body, err := io.ReadAll(status.Body)
	status.Body.Close()
	if err != nil {
		return err
	}
	if status.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) != "true" {
		return fmt.Errorf("VirtualOJ 登录状态未确认")
	}
	return nil
}
