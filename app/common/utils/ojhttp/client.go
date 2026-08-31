package ojhttp

import (
	"net"
	"net/http"
	"time"
)

const DefaultTimeout = 30 * time.Second

// DefaultUserAgent 浏览器态 UA：部分 OJ（尤其 CF Cloudflare）对 Go-http-client 更易 403。
const DefaultUserAgent = "Mozilla/5.0 (compatible; GoAlgo/1.0; +https://algo.zhiyuansofts.cn) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// Client 带超时的全局 OJ HTTP 客户端，避免 http.Get/DefaultClient 无超时挂死。
var Client = &http.Client{
	Timeout: DefaultTimeout,
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 25 * time.Second,
	},
}

// NewWithJar 返回带 CookieJar 与超时的客户端（洛谷/QOJ 登录场景）。
func NewWithJar(jar http.CookieJar) *http.Client {
	return &http.Client{
		Timeout: DefaultTimeout,
		Jar:     jar,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 25 * time.Second,
		},
	}
}

// EnsureHeaders 补默认 UA / Accept（已有则不覆盖）。
func EnsureHeaders(req *http.Request) {
	if req == nil {
		return
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", DefaultUserAgent)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json, text/plain, */*")
	}
}

func Get(url string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	EnsureHeaders(req)
	return Do(req)
}
