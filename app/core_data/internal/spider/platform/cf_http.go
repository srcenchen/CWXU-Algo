package platform

import (
	"context"
	"cwxu-algo/app/common/utils/ojhttp"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"
)

// Codeforces 访问策略（机房直连常被 Cloudflare/中间盒 403）：
//
//  1. 浏览器态 Header + Cookie 热身（缓解软拦截）
//  2. 403/429 有限重试 + 重新热身
//  3. 环境变量出口（真正绕过 IP 拦的关键）：
//     - CWXU_CF_HTTP_PROXY / CWXU_CF_HTTPS_PROXY / CWXU_CF_ALL_PROXY
//       支持 http:// 与 socks5://（与本机 Clash 同类）
//     - 未设时回退 HTTPS_PROXY / HTTP_PROXY / ALL_PROXY
//  4. 可换 API 根（中继）：
//     - CWXU_CF_API_BASE 默认 https://codeforces.com
//     - CWXU_CF_API_FALLBACKS 逗号分隔备用根（中继脚本 / 反代）
//
// 本机可通、服务器 403：通常是出口 IP；请在 core_data 进程设代理或跑 scripts/cf-api-relay.py。

const (
	cfChromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	cfWarmTTL  = 10 * time.Minute
	cfMaxTry   = 3
)

var (
	cfHTTPOnce sync.Once
	cfHTTP     *http.Client
	cfHTTPErr  error

	cfWarmMu   sync.Mutex
	cfLastWarm time.Time
)

func cfAPIBases() []string {
	primary := strings.TrimSpace(os.Getenv("CWXU_CF_API_BASE"))
	if primary == "" {
		primary = "https://codeforces.com"
	}
	primary = strings.TrimRight(primary, "/")
	out := []string{primary}
	seen := map[string]struct{}{primary: {}}
	for _, part := range strings.Split(os.Getenv("CWXU_CF_API_FALLBACKS"), ",") {
		b := strings.TrimRight(strings.TrimSpace(part), "/")
		if b == "" {
			continue
		}
		if _, ok := seen[b]; ok {
			continue
		}
		seen[b] = struct{}{}
		out = append(out, b)
	}
	return out
}

func cfProxyURL() *url.URL {
	for _, key := range []string{
		"CWXU_CF_HTTP_PROXY",
		"CWXU_CF_HTTPS_PROXY",
		"CWXU_CF_ALL_PROXY",
		"HTTPS_PROXY",
		"https_proxy",
		"HTTP_PROXY",
		"http_proxy",
		"ALL_PROXY",
		"all_proxy",
	} {
		raw := strings.TrimSpace(os.Getenv(key))
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			continue
		}
		return u
	}
	return nil
}

func cfBuildClient() (*http.Client, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   12 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   12 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 25 * time.Second,
		ForceAttemptHTTP2:     true,
	}

	if pu := cfProxyURL(); pu != nil {
		switch strings.ToLower(pu.Scheme) {
		case "socks5", "socks5h":
			var auth *proxy.Auth
			if pu.User != nil {
				pass, _ := pu.User.Password()
				auth = &proxy.Auth{User: pu.User.Username(), Password: pass}
			}
			host := pu.Host
			d, err := proxy.SOCKS5("tcp", host, auth, &net.Dialer{Timeout: 12 * time.Second})
			if err != nil {
				return nil, fmt.Errorf("cf socks5 proxy: %w", err)
			}
			transport.Proxy = nil
			transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.Dial(network, addr)
			}
		case "http", "https":
			transport.Proxy = http.ProxyURL(pu)
		default:
			return nil, fmt.Errorf("unsupported cf proxy scheme %q", pu.Scheme)
		}
	}

	return &http.Client{
		Timeout:   45 * time.Second,
		Jar:       jar,
		Transport: transport,
	}, nil
}

func cfClient() (*http.Client, error) {
	cfHTTPOnce.Do(func() {
		cfHTTP, cfHTTPErr = cfBuildClient()
	})
	return cfHTTP, cfHTTPErr
}

// cfResetClient 测试或代理配置热更新时可调（生产一般进程级一次）。
func cfResetClient() {
	cfHTTPOnce = sync.Once{}
	cfHTTP = nil
	cfHTTPErr = nil
	cfWarmMu.Lock()
	cfLastWarm = time.Time{}
	cfWarmMu.Unlock()
}

func cfApplyBrowserHeaders(req *http.Request, base string) {
	req.Header.Set("User-Agent", cfChromeUA)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9,zh-CN;q=0.8,zh;q=0.7")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	if base != "" {
		req.Header.Set("Referer", base+"/")
		req.Header.Set("Origin", base)
	} else {
		req.Header.Set("Referer", "https://codeforces.com/")
		req.Header.Set("Origin", "https://codeforces.com")
	}
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
}

func cfWarmSession(ctx context.Context, client *http.Client, base string) {
	cfWarmMu.Lock()
	defer cfWarmMu.Unlock()
	if time.Since(cfLastWarm) < cfWarmTTL {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	if err != nil {
		return
	}
	cfApplyBrowserHeaders(req, base)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Del("Origin")
	resp, err := ojhttp.DoWithClient(client, req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	cfLastWarm = time.Now()
}

func cfForceRewarm() {
	cfWarmMu.Lock()
	cfLastWarm = time.Time{}
	cfWarmMu.Unlock()
}

// cfGetJSON 拉取 CF API 路径（以 /api/ 开头），自动 base 回退、热身与有限重试。
// pathAndQuery 例: /api/user.status?handle=x&from=1&count=1000
func cfGetJSON(ctx context.Context, pathAndQuery string) (body []byte, usedBase string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	pathAndQuery = strings.TrimSpace(pathAndQuery)
	if pathAndQuery == "" || !strings.HasPrefix(pathAndQuery, "/") {
		return nil, "", fmt.Errorf("cf path must start with /")
	}
	client, err := cfClient()
	if err != nil {
		return nil, "", err
	}

	bases := cfAPIBases()
	var lastErr error
	for _, base := range bases {
		cfWarmSession(ctx, client, base)
		for try := 0; try < cfMaxTry; try++ {
			if err := ctx.Err(); err != nil {
				return nil, base, err
			}
			full := base + pathAndQuery
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
			if reqErr != nil {
				return nil, base, reqErr
			}
			cfApplyBrowserHeaders(req, base)
			resp, doErr := ojhttp.DoWithClient(client, req)
			if doErr != nil {
				lastErr = fmt.Errorf("发起http请求失败: %w", doErr)
				// 网络错误：换 base 前稍等
				time.Sleep(time.Duration(try+1) * 400 * time.Millisecond)
				continue
			}
			b, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			resp.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("解析body错误: %s", readErr.Error())
				continue
			}
			if resp.StatusCode == http.StatusOK {
				return b, base, nil
			}
			// 403/429：重新热身再试；HTML 墙也走重试
			lastErr = cfHTTPStatusErr(pathAndQuery, resp.StatusCode, b)
			if resp.StatusCode == http.StatusForbidden ||
				resp.StatusCode == http.StatusTooManyRequests ||
				resp.StatusCode >= 500 {
				cfForceRewarm()
				cfWarmSession(ctx, client, base)
				time.Sleep(time.Duration(try+1) * 600 * time.Millisecond)
				continue
			}
			// 其它 4xx 不空转
			return nil, base, lastErr
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("codeforces 请求失败")
	}
	// 提示可配置出口（只在最终失败时，避免日志噪音）
	if cfProxyURL() == nil && len(bases) == 1 {
		lastErr = fmt.Errorf("%w（可设 CWXU_CF_HTTP_PROXY 或 CWXU_CF_API_BASE 中继）", lastErr)
	}
	return nil, "", lastErr
}
